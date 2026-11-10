package clickhouse

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/nedanwr/vario/internal/model"
)

var bg = context.Background()

func readOnly(t *testing.T, f *fakeServer) {
	t.Helper()
	if m := f.mutations(); len(m) != 0 {
		t.Fatalf("a read-only operation issued statements: %v", m)
	}
	for _, q := range f.queries {
		if !strings.HasPrefix(q, "SELECT") && !strings.HasPrefix(q, "CHECK GRANT") && !insertToRe.MatchString(q) {
			t.Fatalf("unexpected statement %q", q)
		}
	}
}

func TestCheckConnection(t *testing.T) {
	f, srv := newFake(t)
	c := testClient(t, srv.URL)

	// Existing, empty database: a migration is required; nothing is created.
	r, err := c.CheckConnection(bg)
	if err != nil || r.Ready() || r.Issues[0].Kind != KindMigration || !strings.Contains(r.Issues[0].Message, "vario migrate") {
		t.Fatalf("empty database: %+v %v", r, err)
	}
	if !strings.HasPrefix(r.Probe, "not run") {
		t.Fatalf("no probe without a schema: %q", r.Probe)
	}
	// Missing database.
	f.dbExists = false
	if r, _ = c.CheckConnection(bg); r.Ready() || r.Issues[0].Kind != KindDatabaseMissing {
		t.Fatalf("missing database: %+v", r.Issues)
	}
	// Complete schema: INSERT on every table and SELECT on vario_meta are checked; the probe passes.
	f.dbExists = true
	f.tables, f.columns, f.meta = completeSchema("vario")
	r, err = c.CheckConnection(bg)
	if err != nil || !r.Ready() || !r.Schema.Complete() || r.Probe != "ok" || len(r.Privileges) != len(model.Tables)+1 {
		t.Fatalf("complete schema: %+v %v", r, err)
	}
	if r.Schema.DestinationID == "" || r.Schema.Recorded != SchemaVersion || len(r.Warnings) != 0 {
		t.Fatalf("metadata: %+v", r.Schema)
	}
	// A missing privilege, a rejected insert and a read-only profile are reported.
	f.denied["CHECK GRANT INSERT ON `vario`.`messages`"] = true
	if r, _ = c.CheckConnection(bg); r.Ready() || r.Issues[0].Kind != KindPermission || !strings.Contains(r.Issues[0].Message, "INSERT ON vario.messages") {
		t.Fatalf("missing INSERT: %+v", r.Issues)
	}
	f.denied = map[string]bool{}
	f.insertDenied["runs"] = true
	if r, _ = c.CheckConnection(bg); r.Ready() || !strings.Contains(r.Probe, "runs") {
		t.Fatalf("rejected probe: %+v", r)
	}
	f.insertDenied = map[string]bool{}
	f.readonly = 1
	if r, _ = c.CheckConnection(bg); r.Ready() || r.ReadOnly != 1 || !strings.Contains(r.Issues[0].Message, "read-only") {
		t.Fatalf("readonly: %+v", r)
	}
	f.readonly = 0
	// Administrative privileges held by the operational user are flagged.
	r, _ = c.CheckConnection(bg)
	if len(r.Excess) == 0 {
		t.Fatalf("excess privileges should be reported: %+v", r)
	}
	f.denied = map[string]bool{"CHECK GRANT CREATE USER ON *.*": true, "CHECK GRANT CREATE DATABASE ON *.*": true, "CHECK GRANT DROP DATABASE ON *.*": true}
	if r, _ = c.CheckConnection(bg); len(r.Excess) != 0 {
		t.Fatalf("a restricted user has no excess privileges: %+v", r.Excess)
	}
	// Legacy schema (version 1, no vario_meta): usable, with a warning.
	f.tables, f.columns = legacySchema("vario")
	f.meta = nil
	if r, _ = c.CheckConnection(bg); !r.Ready() || r.Schema.Version() != 1 || len(r.Warnings) != 1 {
		t.Fatalf("legacy schema: %+v", r)
	}
	// Incompatible column; foreign views and tables with Vario's names.
	f.tables, f.columns, f.meta = completeSchema("vario")
	for i, col := range f.columns {
		if col == "messages|text|String" {
			f.columns[i] = "messages|text|UInt8"
		}
	}
	if r, _ = c.CheckConnection(bg); r.Ready() || r.Issues[0].Kind != KindSchema || !strings.Contains(r.Issues[0].Message, "column text is UInt8") {
		t.Fatalf("incompatible column: %+v", r.Issues)
	}
	f.tables, f.columns, f.meta = completeSchema("vario")
	for i, tb := range f.tables {
		switch {
		case strings.HasPrefix(tb, "tool_usage|"):
			f.tables[i] = "tool_usage|View|SELECT 1|"
		case strings.HasPrefix(tb, "messages_latest|"):
			f.tables[i] = "messages_latest|MergeTree||"
		case strings.HasPrefix(tb, MetaTable+"|"):
			f.tables[i] = MetaTable + "|MergeTree||"
		}
	}
	if r, _ = c.CheckConnection(bg); len(r.Schema.Problems) != 3 {
		t.Fatalf("foreign objects: %+v", r.Schema.Problems)
	}
	// Older servers without CHECK GRANT: privileges unknown, not failed.
	f.tables, f.columns, f.meta = completeSchema("vario")
	f.noCheck = true
	if r, _ = c.CheckConnection(bg); r.PrivilegesUnknown == "" || !r.Ready() {
		t.Fatalf("no CHECK GRANT: %+v", r)
	}
	readOnly(t, f)

	// Authentication and network failures are classified.
	bad := testClient(t, srv.URL)
	bad.password = "wrong"
	var ce *CheckError
	if _, err := bad.CheckConnection(bg); !errors.As(err, &ce) || ce.Kind != KindAuth || ce.Stage != "authenticate" {
		t.Fatalf("auth: %v", err)
	}
	srv.Close()
	if _, err := c.CheckConnection(bg); !errors.As(err, &ce) || ce.Kind != KindNetwork || ce.Stage != "reach" {
		t.Fatalf("network: %v", err)
	}
}

func TestCheckDestinationIsReadOnly(t *testing.T) {
	f, srv := newFake(t)
	c := testClient(t, srv.URL)
	var me *MigrationRequiredError
	if _, err := c.CheckDestination(bg); !errors.As(err, &me) || Classify(err) != KindMigration {
		t.Fatalf("missing schema: %v", err)
	}
	f.dbExists = false
	if _, err := c.CheckDestination(bg); !errors.Is(err, ErrDatabaseMissing) {
		t.Fatalf("missing database: %v", err)
	}
	f.dbExists = true
	f.tables = []string{"messages|MergeTree||"}
	var se *SchemaError
	if _, err := c.CheckDestination(bg); !errors.As(err, &se) {
		t.Fatalf("incompatible: %v", err)
	}
	f.tables, f.columns = legacySchema("vario")
	info, err := c.CheckDestination(bg)
	if err != nil || info.ServerID != "" || info.Version != 1 || len(info.Warnings) != 1 {
		t.Fatalf("legacy schema: %+v %v", info, err)
	}
	f.tables, f.columns, f.meta = completeSchema("vario")
	info, err = c.CheckDestination(bg)
	if err != nil || info.ServerID != "11111111-2222-4333-8444-555555555555" || info.Version != 2 || len(info.Warnings) != 0 {
		t.Fatalf("complete schema: %+v %v", info, err)
	}
	f.meta = append(f.meta, "schema_version|7")
	if info, err = c.CheckDestination(bg); err != nil || len(info.Warnings) != 1 {
		t.Fatalf("newer schema: %+v %v", info, err)
	}
	readOnly(t, f)
}

func kinds(p *Plan) map[StepKind]int {
	out := map[StepKind]int{}
	for _, s := range p.Steps {
		out[s.Kind]++
	}
	return out
}

func TestPlanSchema(t *testing.T) {
	f, srv := newFake(t)
	c := testClient(t, srv.URL)

	// Empty database: every table, the metadata, every view and the version.
	p, err := c.PlanSchema(bg)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(p.Steps); n != len(model.Tables)+2+len(viewDefs("vario"))+1 || p.Has(StepDatabase) || kinds(p)[StepSchema] != n {
		t.Fatalf("empty database plan: %d steps %+v", n, kinds(p))
	}
	last := p.Steps[len(p.Steps)-1]
	if !strings.Contains(last.Summary, "schema version") {
		t.Fatalf("the version must be recorded last: %q", last.Summary)
	}
	for _, s := range p.Steps {
		if strings.Contains(s.SQL, "DROP") || strings.Contains(s.SQL, "ALTER") || strings.Contains(s.SQL, "TRUNCATE") {
			t.Fatalf("destructive statement planned: %s", s.SQL)
		}
	}
	// Missing database: created first, with IF NOT EXISTS.
	f.dbExists = false
	if p, _ = c.PlanSchema(bg); !p.Has(StepDatabase) || p.Steps[0].SQL != "CREATE DATABASE IF NOT EXISTS `vario`" {
		t.Fatalf("missing database: %+v", p.Steps[0])
	}
	f.dbExists = true
	// Legacy schema: only the additive version 2 steps.
	f.tables, f.columns = legacySchema("vario")
	p, _ = c.PlanSchema(bg)
	if len(p.Steps) != 3 || !strings.Contains(p.Steps[0].SQL, MetaTable) || !strings.Contains(p.Steps[1].SQL, "destination_id") {
		t.Fatalf("legacy upgrade plan: %+v", p.Steps)
	}
	// Up to date: nothing.
	f.tables, f.columns, f.meta = completeSchema("vario")
	if p, err = c.PlanSchema(bg); err != nil || !p.Empty() {
		t.Fatalf("up-to-date schema: %+v %v", p, err)
	}
	// An outdated Vario view is replaced; a foreign one stops the plan.
	for i, tb := range f.tables {
		if strings.HasPrefix(tb, "token_usage_daily|") {
			f.tables[i] = "token_usage_daily|View|SELECT day FROM vario.provider_turns AS p FINAL|"
		}
	}
	if p, _ = c.PlanSchema(bg); len(p.Steps) != 1 || !strings.Contains(p.Steps[0].SQL, "CREATE OR REPLACE VIEW") {
		t.Fatalf("outdated view: %+v", p.Steps)
	}
	f.tables = append(f.tables[:0:0], f.tables...)
	for i, tb := range f.tables {
		if strings.HasPrefix(tb, "token_usage_daily|") {
			f.tables[i] = "token_usage_daily|View|SELECT 1|"
		}
	}
	var se *SchemaError
	if p, err = c.PlanSchema(bg); !errors.As(err, &se) || !p.Empty() {
		t.Fatalf("foreign view: %+v %v", p, err)
	}
	// A schema from a newer Vario is not touched.
	f.tables, f.columns, f.meta = completeSchema("vario")
	f.meta = append(f.meta, "schema_version|3")
	if _, err = c.PlanSchema(bg); !errors.Is(err, ErrSchemaNewer) {
		t.Fatalf("newer schema: %v", err)
	}
	// Planning (the dry run) only reads.
	readOnly(t, f)
}

func TestApplyStopsAndReportsPartialFailure(t *testing.T) {
	f, srv := newFake(t)
	admin := &Admin{testClient(t, srv.URL)}
	p, err := admin.PlanSchema(bg)
	if err != nil {
		t.Fatal(err)
	}
	f.fail["CREATE TABLE IF NOT EXISTS `vario`.`conversations`"] = 497
	var seen int
	res := admin.Apply(bg, p, func(Step, string, error) { seen++ })
	if res.OK() || !res.Partial() || res.Failed == nil || !strings.Contains(res.Failed.Summary, "conversations") {
		t.Fatalf("partial failure: %+v", res)
	}
	if len(res.Applied)+1+len(res.NotAttempted) != len(p.Steps) || seen != len(res.Applied)+1 {
		t.Fatalf("every step must be accounted for: %+v", res)
	}
	if Classify(res.Err) != KindPermission {
		t.Fatalf("classification: %v", res.Err)
	}
	if m := f.mutations(); len(m) != len(res.Applied) {
		t.Fatalf("statements after the failure must not run: %v", m)
	}
}

func TestReplaceViewFallsBack(t *testing.T) {
	f, srv := newFake(t)
	admin := &Admin{testClient(t, srv.URL)}
	v := viewDefs("vario")[0]
	f.fail["CREATE OR REPLACE VIEW"] = 497
	if w, err := admin.replaceView(bg, v); err != nil || w != "" {
		t.Fatalf("DROP VIEW fallback: %q %v", w, err)
	}
	if m := f.mutations(); len(m) != 2 || !strings.HasPrefix(m[0], "DROP VIEW IF EXISTS `vario`.`"+v.name+"`") {
		t.Fatalf("fallback statements: %v", m)
	}
	f.fail["DROP VIEW"] = 497
	f.execs = nil
	if w, err := admin.replaceView(bg, v); err != nil || !strings.Contains(w, "kept") || len(f.mutations()) != 0 {
		t.Fatalf("without DROP VIEW the view is kept: %q %v", w, err)
	}
}

func TestRecordVersionRequiresCompleteTables(t *testing.T) {
	f, srv := newFake(t)
	admin := &Admin{testClient(t, srv.URL)}
	if _, err := recordVersion(bg, admin); err == nil {
		t.Fatal("the version must not be recorded for a missing schema")
	}
	f.tables, f.columns, f.meta = completeSchema("vario")
	f.meta = f.meta[:1] // destination id, no version
	if _, err := recordVersion(bg, admin); err != nil {
		t.Fatal(err)
	}
	if m := f.mutations(); len(m) != 1 || !strings.Contains(m[0], "'schema_version', '2'") {
		t.Fatalf("version insert: %v", m)
	}
}

// Apply works on the plan's database even when the administrative
// connection was opened for another one.
func TestApplyUsesPlanDatabase(t *testing.T) {
	f, srv := newFake(t)
	admin := &Admin{testClient(t, srv.URL)} // opened for "vario"
	f.tables, f.columns, f.meta = completeSchema("metrics")
	f.meta = f.meta[:1]
	p := &Plan{Database: "metrics", Steps: []Step{{Kind: StepSchema, Summary: "record", run: recordVersion}}}
	if res := admin.Apply(bg, p, nil); !res.OK() {
		t.Fatal(res.Err)
	}
	if m := f.mutations(); len(m) != 1 || !strings.Contains(m[0], "`metrics`.`vario_meta`") {
		t.Fatalf("the version must be recorded in the plan's database: %v", m)
	}
}
