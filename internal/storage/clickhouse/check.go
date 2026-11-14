package clickhouse

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// Kind classifies a ClickHouse failure so that users get actionable messages
// instead of a generic connection error.
type Kind string

const (
	KindNetwork         Kind = "network"
	KindTLS             Kind = "tls"
	KindAuth            Kind = "authentication"
	KindDatabaseMissing Kind = "database missing"
	KindPermission      Kind = "permission"
	KindSchema          Kind = "schema"
	KindServer          Kind = "server"
	// KindMigration: Vario's schema is missing or incomplete; run `vario migrate`.
	KindMigration Kind = "migration required"
	// KindRestriction: a settings profile, settings constraint or quota of
	// the user prevents inserts although it may hold the privileges.
	KindRestriction Kind = "session restriction"
)

// ClickHouse error codes used for classification.
const (
	codeSyntaxError     = 62
	codeUnknownDatabase = 81
	codeUnknownSetting  = 115
	codeReadonly        = 164
	codeQuotaExceeded   = 201
	codeSettingLimit    = 452 // SETTING_CONSTRAINT_VIOLATION
	codeUnknownUser     = 192
	codeWrongPassword   = 193
	codeRequirePassword = 194
	codeAccessDenied    = 497
	codeAuthFailed      = 516
)

// Classify returns the kind of a ClickHouse client error.
func Classify(err error) Kind {
	var se *SchemaError
	if errors.As(err, &se) {
		return KindSchema
	}
	var me *MigrationRequiredError
	if errors.As(err, &me) {
		return KindMigration
	}
	if errors.Is(err, ErrDatabaseMissing) {
		return KindDatabaseMissing
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return KindNetwork
	}
	var e *Error
	if !errors.As(err, &e) {
		return KindServer
	}
	switch e.Code {
	case codeUnknownUser, codeWrongPassword, codeRequirePassword, codeAuthFailed:
		return KindAuth
	case codeAccessDenied:
		return KindPermission
	case codeReadonly, codeQuotaExceeded, codeSettingLimit:
		return KindRestriction
	case codeUnknownDatabase:
		return KindDatabaseMissing
	}
	if e.Code == 0 && (e.Status == 401 || e.Status == 403) {
		return KindAuth
	}
	if e.Status == 0 {
		m := strings.ToLower(e.Message)
		for _, s := range []string{"x509", "tls:", "certificate", "http response to https client", "first record does not look like a tls handshake"} {
			if strings.Contains(m, s) {
				return KindTLS
			}
		}
		return KindNetwork
	}
	return KindServer
}

// Hint explains a failure kind and what to do about it.
func Hint(k Kind) string {
	switch k {
	case KindNetwork:
		return "ClickHouse is not reachable. Check that the server is running and that the host, port (HTTP 8123, HTTPS 8443 by default) and any firewall or proxy allow the connection."
	case KindTLS:
		return "The TLS connection failed. Check that the URL scheme matches the port (https:// for a TLS port), and set clickhouse.ca_file for a private certificate authority."
	case KindAuth:
		return "ClickHouse rejected the credentials. Check clickhouse.username and the password (`vario config set-password`)."
	case KindDatabaseMissing:
		return "The database does not exist or the user has no access to it. Create it and grant access with `vario init` (guided setup) or `vario migrate`, or choose another database."
	case KindPermission:
		return "The user lacks a required privilege. Run `vario init` to inspect and fix the grants, or see \"ClickHouse permissions\" in the README."
	case KindSchema:
		return "The database contains objects with Vario's table or view names that Vario did not create or cannot use. Use a dedicated database, or remove or rename those objects."
	case KindRestriction:
		return "A settings profile, settings constraint or quota of this user prevents inserts (for example readonly = 1, a constraint on an insert setting, or an exhausted quota). An administrator has to change the user's profile or quota; Vario never changes them."
	case KindMigration:
		return "Vario's schema is missing or incomplete. Run `vario migrate` (it asks for administrative credentials); the collector keeps data spooled until then."
	}
	return "ClickHouse returned an error."
}

// CheckError is a connection check that could not proceed past Stage.
type CheckError struct {
	Stage string // "reach", "authenticate", "inspect"
	Kind  Kind
	Err   error
}

func (e *CheckError) Error() string { return e.Err.Error() }
func (e *CheckError) Unwrap() error { return e.Err }

// Privilege is the result of checking one grant for the current user.
type Privilege struct {
	Grant   string // e.g. "INSERT ON vario.messages"
	Granted bool
}

// Issue is a problem found by a connection check that does not prevent
// talking to ClickHouse but may prevent delivery.
type Issue struct {
	Kind    Kind
	Message string
}

// Report is the outcome of a successful operational connection check.
type Report struct {
	Version  string
	User     string
	Database string
	Schema   SchemaStatus
	// ReadOnly is the session's readonly setting (0 allows inserts).
	ReadOnly   int
	Privileges []Privilege
	// PrivilegesUnknown explains why privileges could not be checked.
	PrivilegesUnknown string
	// Probe describes the insert probe: "ok" when ClickHouse accepted an
	// insert without rows into every Vario table.
	Probe string
	// Unverified lists checks that could not be performed conclusively.
	// They are neither confirmed nor failed.
	Unverified []string
	// Excess lists administrative privileges the operational user holds.
	Excess []string
	// Warnings do not prevent delivery.
	Warnings []string
	Issues   []Issue
}

// Ready reports whether no check failed: the collector can deliver without
// further action as far as the check could tell.
func (r *Report) Ready() bool { return len(r.Issues) == 0 }

// Verified reports whether every check passed conclusively. Even then, rows
// are not written by the check, so limits that depend on the data (quotas
// on written bytes, disk space) are only confirmed by the first delivery.
func (r *Report) Verified() bool { return r.Ready() && len(r.Unverified) == 0 }

// excessChecks are privileges an operational user should not hold.
var excessChecks = []string{"CREATE USER ON *.*", "CREATE DATABASE ON *.*", "DROP DATABASE ON *.*"}

// CheckConnection verifies, as the configured (operational) user and without
// changing anything:
//
//   - the endpoint is reachable and the credentials are accepted (from this
//     machine, so host restrictions are evaluated as for the collector);
//   - the database and Vario's schema exist and are compatible, including
//     reading vario_meta, which is everything the collector reads;
//   - the session is not read-only (readonly comes from the user's settings
//     profile and is evaluated by the server);
//   - the user holds the privileges the collector needs (CHECK GRANT
//     evaluates effective privileges including roles and partial revokes;
//     available in ClickHouse 25.8, not in 24.8); this explains failures but
//     proves nothing on its own;
//   - ClickHouse accepts an insert into every Vario table with exactly the
//     settings of a delivery, without rows (see probeInsert). This is the
//     decisive check for inserts.
//
// A *CheckError is returned if a step fails so that later steps cannot run.
// Other failures are Issues; checks that could not be performed
// conclusively are listed in Unverified.
func (c *Client) CheckConnection(ctx context.Context) (*Report, error) {
	r := &Report{User: c.user, Database: c.database}
	if err := c.Ping(ctx); err != nil {
		return nil, &CheckError{"reach", Classify(err), err}
	}
	ver, err := c.ServerVersion(ctx)
	if isReadonlyRefusal(err) {
		// The credentials were accepted, but the session refuses to change
		// any setting, including the output format settings of Vario's
		// queries (ClickHouse 24.8 applies this to every setting; 25.8 does
		// not): readonly = 1. Nothing else can be inspected.
		r.ReadOnly = 1
		r.Issues = append(r.Issues, Issue{KindRestriction, fmt.Sprintf("sessions of user %s are read-only (readonly = 1, usually from a settings profile); Vario cannot insert", c.user)})
		r.Probe = "not run (read-only session)"
		r.Unverified = append(r.Unverified, "the schema and privileges were not inspected: the read-only session refuses the settings of Vario's queries")
		return r, nil
	}
	if err != nil {
		return nil, &CheckError{"authenticate", Classify(err), err}
	}
	r.Version = ver
	st, err := c.InspectSchema(ctx)
	if err != nil {
		return nil, &CheckError{"inspect", Classify(err), err}
	}
	r.Schema = st
	switch {
	case !st.DatabaseExists:
		r.Issues = append(r.Issues, Issue{KindDatabaseMissing, fmt.Sprintf("database %s does not exist or user %s has no access to it", c.database, c.user)})
	case len(st.Problems) > 0:
		for _, p := range st.Problems {
			r.Issues = append(r.Issues, Issue{KindSchema, p})
		}
	case len(st.MissingTables) > 0:
		r.Issues = append(r.Issues, Issue{KindMigration, (&MigrationRequiredError{Database: c.database, Missing: st.MissingTables}).Error()})
	}
	if st.Newer() {
		r.Warnings = append(r.Warnings, fmt.Sprintf("the schema has version %d, newer than this Vario (%d); upgrade Vario", st.Recorded, SchemaVersion))
	} else if st.Usable() && !st.Complete() {
		r.Warnings = append(r.Warnings, fmt.Sprintf("schema version %d; run `vario migrate` to update it to version %d", st.Version(), SchemaVersion))
	}

	var ro []struct {
		R string `json:"r"`
	}
	if err := c.Query(ctx, "SELECT toString(getSetting('readonly')) AS r", &ro); err != nil || len(ro) != 1 {
		if err == nil {
			err = errors.New("unexpected response")
		}
		r.Unverified = append(r.Unverified, "the session's readonly setting could not be read ("+err.Error()+")")
	} else {
		fmt.Sscan(ro[0].R, &r.ReadOnly)
		if r.ReadOnly > 0 {
			r.Issues = append(r.Issues, Issue{KindRestriction, fmt.Sprintf("sessions of user %s are read-only (readonly = %d, usually from a settings profile); Vario cannot insert", c.user, r.ReadOnly)})
		}
	}

	db := quoteIdent(c.database)
	var grants []string
	for _, t := range requiredTables() {
		grants = append(grants, "INSERT ON "+db+"."+quoteIdent(t))
	}
	if st.MetaExists {
		grants = append(grants, "SELECT ON "+db+"."+quoteIdent(MetaTable))
	}
	if st.Checkpoints {
		grants = append(grants, "SELECT ON "+db+"."+quoteIdent(CheckpointTable), "INSERT ON "+db+"."+quoteIdent(CheckpointTable))
	}
	for _, g := range grants {
		granted, err := c.checkGrant(ctx, g)
		if err != nil {
			r.Privileges = nil
			r.PrivilegesUnknown = "privileges could not be listed: " + err.Error()
			var e *Error
			if errors.As(err, &e) && e.Code == codeSyntaxError {
				r.PrivilegesUnknown = fmt.Sprintf("privileges could not be listed: ClickHouse %s does not support CHECK GRANT (the insert probe below still verifies inserts)", r.Version)
			}
			break
		}
		r.Privileges = append(r.Privileges, Privilege{Grant: strings.ReplaceAll(g, "`", ""), Granted: granted})
	}
	var missing []string
	for _, p := range r.Privileges {
		if !p.Granted {
			missing = append(missing, p.Grant)
		}
	}
	if len(missing) > 0 {
		r.Issues = append(r.Issues, Issue{KindPermission, fmt.Sprintf("user %s lacks %s", c.user, strings.Join(missing, ", "))})
	}
	if r.PrivilegesUnknown == "" {
		r.Excess, _ = c.ExcessPrivileges(ctx)
	}

	switch {
	case !st.Usable():
		r.Probe = "not run (the schema is not ready)"
		r.Unverified = append(r.Unverified, "inserts were not tested because Vario's schema is not ready")
	case r.ReadOnly > 0:
		r.Probe = "not run (read-only session)"
	default:
		r.Probe = "ok"
		probed := requiredTables()
		if st.Checkpoints {
			// The collector does not acknowledge delivered rows without a checkpoint.
			probed = append(probed, CheckpointTable)
		}
		for _, t := range probed {
			if err := c.probeInsert(ctx, t); err != nil {
				r.Probe = fmt.Sprintf("insert into %s.%s rejected: %v", c.database, t, err)
				r.Issues = append(r.Issues, Issue{Classify(err), r.Probe})
				break
			}
		}
	}
	return r, nil
}

// isReadonlyRefusal reports whether err is ClickHouse refusing to change a
// setting because the session is read-only.
func isReadonlyRefusal(err error) bool {
	var e *Error
	return errors.As(err, &e) && e.Code == codeReadonly && strings.Contains(e.Message, "in readonly mode")
}

// ExcessPrivileges returns administrative privileges the user holds that
// an operational account does not need (checked with CHECK GRANT).
func (c *Client) ExcessPrivileges(ctx context.Context) ([]string, error) {
	var out []string
	for _, g := range excessChecks {
		ok, err := c.checkGrant(ctx, g)
		if err != nil {
			return out, err
		}
		if ok {
			out = append(out, g)
		}
	}
	return out, nil
}

// DestinationInfo is what the collector learns about its destination.
type DestinationInfo struct {
	// ServerID is the destination id recorded in the database ("" for a
	// schema created before version 2, or if the user cannot read it).
	ServerID string
	Version  int
	// Checkpoints reports whether the delivery checkpoint table exists and
	// is visible to the user (schema 3+). Without it, a database restored
	// to an earlier state cannot be detected.
	Checkpoints bool
	Warnings    []string
}

// CheckDestination is the collector's startup check. It only reads, and it
// never creates or changes anything: a missing or incompatible schema is
// returned as an error (*MigrationRequiredError, *SchemaError or
// ErrDatabaseMissing) so that data stays spooled until `vario migrate` has
// been run. Missing or outdated views and missing metadata are warnings,
// since they do not affect inserts.
func (c *Client) CheckDestination(ctx context.Context) (DestinationInfo, error) {
	var info DestinationInfo
	st, err := c.InspectSchema(ctx)
	if err != nil {
		return info, err
	}
	switch {
	case !st.DatabaseExists:
		return info, fmt.Errorf("%w: %s (or user %s has no access to it); run `vario migrate` or `vario init`", ErrDatabaseMissing, c.database, c.user)
	case len(st.Problems) > 0:
		return info, &SchemaError{Database: c.database, Problems: st.Problems}
	case len(st.MissingTables) > 0:
		return info, &MigrationRequiredError{Database: c.database, Missing: st.MissingTables}
	}
	info.ServerID, info.Version, info.Checkpoints = st.DestinationID, st.Version(), st.Checkpoints
	switch {
	case st.Newer():
		info.Warnings = append(info.Warnings, fmt.Sprintf("schema version %d is newer than this Vario supports (%d); upgrade Vario", st.Recorded, SchemaVersion))
	case !st.Complete():
		info.Warnings = append(info.Warnings, fmt.Sprintf("schema version %d is behind the current version %d; run `vario migrate`", st.Version(), SchemaVersion))
	}
	return info, nil
}

func (c *Client) checkGrant(ctx context.Context, grant string) (bool, error) {
	var rows []struct {
		Result int `json:"result"`
	}
	if err := c.Query(ctx, "CHECK GRANT "+grant, &rows); err != nil {
		return false, err
	}
	if len(rows) != 1 {
		return false, errors.New("unexpected CHECK GRANT response")
	}
	return rows[0].Result == 1, nil
}
