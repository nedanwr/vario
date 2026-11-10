package clickhouse

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/nedanwr/vario/internal/buildinfo"
	"github.com/nedanwr/vario/internal/model"
)

// SchemaVersion is the schema this Vario version migrates to.
//
//	1: the tables and views of Vario 0.1 (no recorded version).
//	2: adds vario_meta with the schema version and a random destination id,
//	   which identifies the database independently of the URL used to reach it.
//
// Migrations are additive: they create missing objects, replace Vario's own
// outdated views and record metadata. Tables and databases are never
// dropped, truncated or altered, and objects Vario did not create are never
// touched; incompatible objects stop the migration (see SchemaError).
const SchemaVersion = 2

// StepKind groups provisioning steps so that users can tell database
// creation, user creation, privilege changes and schema changes apart.
type StepKind string

const (
	StepDatabase   StepKind = "database"
	StepUser       StepKind = "user"
	StepPrivileges StepKind = "privileges"
	StepSchema     StepKind = "schema"
)

// Step is one statement or operation that changes ClickHouse.
type Step struct {
	Kind StepKind
	// Summary describes the step for people.
	Summary string
	// SQL is the statement as displayed. Secrets are never part of it.
	SQL string
	// exec is the statement executed when it differs from SQL (for
	// example, it contains a password hash).
	exec string
	// secrets are removed from error messages of this step.
	secrets []string
	// run performs steps that are more than one statement. It may return a
	// warning for a step that was deliberately skipped.
	run func(ctx context.Context, a *Admin) (warning string, err error)
}

// Plan is an ordered list of steps.
type Plan struct {
	Database string
	Steps    []Step
	// Status is the inspected schema the plan was made from (schema plans only).
	Status SchemaStatus
}

// Empty reports whether nothing needs to be done.
func (p *Plan) Empty() bool { return p == nil || len(p.Steps) == 0 }

// Has reports whether the plan contains a step of kind k.
func (p *Plan) Has(k StepKind) bool {
	if p == nil {
		return false
	}
	for _, s := range p.Steps {
		if s.Kind == k {
			return true
		}
	}
	return false
}

// Append adds the steps of other.
func (p *Plan) Append(other *Plan) {
	if other != nil {
		p.Steps = append(p.Steps, other.Steps...)
	}
}

// PlanSchema inspects the database and returns the steps that bring Vario's
// schema to SchemaVersion. It only reads, so it is also used for previews
// with the operational user (who may see less than an administrator). It
// fails with a *SchemaError for incompatible objects and with ErrSchemaNewer
// for a schema migrated by a newer Vario; nothing is planned in either case.
func (c *Client) PlanSchema(ctx context.Context) (*Plan, error) {
	st, err := c.InspectSchema(ctx)
	if err != nil {
		return nil, err
	}
	return planSchema(c.database, st)
}

func planSchema(db string, st SchemaStatus) (*Plan, error) {
	p := &Plan{Database: db, Status: st}
	if st.Newer() {
		return p, fmt.Errorf("%w (database %s has schema version %d; this Vario supports up to %d): upgrade Vario", ErrSchemaNewer, db, st.Recorded, SchemaVersion)
	}
	if len(st.Problems) > 0 {
		return p, &SchemaError{Database: db, Problems: st.Problems}
	}
	q := func(name string) string { return quoteIdent(db) + "." + quoteIdent(name) }
	if !st.DatabaseExists {
		p.Steps = append(p.Steps, CreateDatabaseStep(db))
	}
	ddl := tableDDL(db)
	for _, name := range st.MissingTables {
		p.Steps = append(p.Steps, Step{Kind: StepSchema, Summary: "create table " + name, SQL: ddl[name]})
	}
	if !st.MetaExists {
		p.Steps = append(p.Steps, Step{Kind: StepSchema, Summary: "create table " + MetaTable + " (schema version and destination id)", SQL: metaDDL(db)})
	}
	if st.DestinationID == "" {
		id, err := newUUID()
		if err != nil {
			return p, err
		}
		p.Steps = append(p.Steps, Step{Kind: StepSchema, Summary: "record destination id " + id,
			SQL: fmt.Sprintf("INSERT INTO %s (key, value) VALUES ('destination_id', '%s')", q(MetaTable), id)})
	}
	views := map[string]viewDef{}
	for _, v := range viewDefs(db) {
		views[v.name] = v
	}
	for _, name := range st.MissingViews {
		p.Steps = append(p.Steps, Step{Kind: StepSchema, Summary: "create view " + name, SQL: views[name].ddl(db, false)})
	}
	for _, name := range st.OutdatedViews {
		v := views[name]
		p.Steps = append(p.Steps, Step{Kind: StepSchema, Summary: "update Vario's outdated view " + name, SQL: v.ddl(db, true),
			run: func(ctx context.Context, a *Admin) (string, error) { return a.replaceView(ctx, v) }})
	}
	if st.Recorded < SchemaVersion {
		p.Steps = append(p.Steps, Step{Kind: StepSchema, Summary: fmt.Sprintf("record schema version %d", SchemaVersion),
			SQL: fmt.Sprintf("INSERT INTO %s (key, value) VALUES ('schema_version', '%d')", q(MetaTable), SchemaVersion),
			run: recordVersion})
	}
	return p, nil
}

// recordVersion records SchemaVersion after verifying that every table and
// the metadata exist, so a version is never recorded for a partial schema.
func recordVersion(ctx context.Context, a *Admin) (string, error) {
	st, err := a.InspectSchema(ctx)
	if err != nil {
		return "", err
	}
	if !st.Usable() || !st.MetaExists || st.DestinationID == "" {
		return "", fmt.Errorf("schema verification failed before recording the version (missing tables: %s; metadata present: %t)",
			strings.Join(st.MissingTables, ", "), st.MetaExists && st.DestinationID != "")
	}
	if st.Recorded >= SchemaVersion {
		return "", nil
	}
	return "", a.Exec(ctx, fmt.Sprintf("INSERT INTO %s.%s (key, value) VALUES ('schema_version', '%d'), ('migrated_by', '%s')",
		quoteIdent(a.database), quoteIdent(MetaTable), SchemaVersion, escapeString("vario "+buildinfo.Version)))
}

// replaceView replaces one of Vario's own outdated views. CREATE OR REPLACE
// is atomic but also needs DROP TABLE (ClickHouse swaps in a temporary
// table); without it, the view is dropped and recreated, which needs only
// DROP VIEW. Without either, the old view is kept and a warning returned.
func (a *Admin) replaceView(ctx context.Context, v viewDef) (string, error) {
	err := a.Exec(ctx, v.ddl(a.database, true))
	if err == nil || Classify(err) != KindPermission {
		return "", err
	}
	if err = a.Exec(ctx, fmt.Sprintf("DROP VIEW IF EXISTS %s.%s", quoteIdent(a.database), quoteIdent(v.name))); err != nil {
		if Classify(err) == KindPermission {
			return fmt.Sprintf("view %s is outdated and was kept: updating it needs DROP VIEW on %s.*", v.name, a.database), nil
		}
		return "", err
	}
	return "", a.Exec(ctx, v.ddl(a.database, false))
}

// ApplyResult reports what Apply did. ClickHouse DDL is not transactional:
// when a step fails, the steps before it remain applied. Every step is
// idempotent or re-planned from inspection, so applying again resumes.
type ApplyResult struct {
	Applied      []Step
	Failed       *Step
	Err          error
	NotAttempted []Step
	Warnings     []string
}

// OK reports whether every step succeeded.
func (r ApplyResult) OK() bool { return r.Err == nil }

// Partial reports whether some steps were applied before a failure.
func (r ApplyResult) Partial() bool { return r.Err != nil && len(r.Applied) > 0 }

// Apply executes the plan in order and stops at the first failure. progress,
// if set, is called after each step.
func (a *Admin) Apply(ctx context.Context, p *Plan, progress func(s Step, warning string, err error)) ApplyResult {
	var res ApplyResult
	if p.Database != "" && p.Database != a.database {
		// Multi-statement steps (version recording, view replacement) work on
		// the plan's database, whatever database a was opened for.
		a = a.ForDatabase(p.Database)
	}
	for i, s := range p.Steps {
		var warning string
		var err error
		switch {
		case s.run != nil:
			warning, err = s.run(ctx, a)
		case s.exec != "":
			err = a.Exec(ctx, s.exec)
		default:
			err = a.Exec(ctx, s.SQL)
		}
		if progress != nil {
			progress(s, warning, err)
		}
		if err != nil {
			err = redact(err, s.secrets)
			failed := s
			res.Failed, res.Err = &failed, fmt.Errorf("%s: %w", s.Summary, err)
			res.NotAttempted = append(res.NotAttempted, p.Steps[i+1:]...)
			return res
		}
		if warning != "" {
			res.Warnings = append(res.Warnings, warning)
		}
		res.Applied = append(res.Applied, s)
	}
	return res
}

// redact removes secrets that ClickHouse may echo in an error message (for
// example, a syntax error quoting part of the statement).
func redact(err error, secrets []string) error {
	var e *Error
	if len(secrets) == 0 || !errors.As(err, &e) {
		return err
	}
	c := *e
	for _, sec := range secrets {
		if sec != "" {
			c.Message = strings.ReplaceAll(c.Message, sec, "[redacted]")
		}
	}
	return &c
}

// CreateDatabaseStep creates a database. IF NOT EXISTS makes it safe to
// repeat; an existing database is reused, never dropped or recreated.
func CreateDatabaseStep(db string) Step {
	return Step{Kind: StepDatabase, Summary: "create database " + db, SQL: "CREATE DATABASE IF NOT EXISTS " + quoteIdent(db)}
}

func newUUID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:], nil
}

// VerifyMigrated inspects the schema after a migration and returns an error
// describing anything that is still not migrated.
func (c *Client) VerifyMigrated(ctx context.Context) (SchemaStatus, error) {
	st, err := c.InspectSchema(ctx)
	if err != nil {
		return st, err
	}
	var missing []string
	switch {
	case !st.DatabaseExists:
		return st, fmt.Errorf("%w: %s", ErrDatabaseMissing, c.database)
	case len(st.Problems) > 0:
		return st, &SchemaError{Database: c.database, Problems: st.Problems}
	}
	missing = append(missing, st.MissingTables...)
	missing = append(missing, st.MissingViews...)
	if !st.MetaExists || st.DestinationID == "" {
		missing = append(missing, MetaTable)
	}
	if len(missing) > 0 {
		return st, &MigrationRequiredError{Database: c.database, Missing: missing}
	}
	if st.Recorded < SchemaVersion {
		return st, errors.New("the schema version was not recorded")
	}
	return st, nil
}

// requiredTables lists the tables the collector inserts into.
func requiredTables() []string { return append([]string(nil), model.Tables...) }

// OperationalTables lists the tables the collector inserts into.
func OperationalTables() []string { return requiredTables() }
