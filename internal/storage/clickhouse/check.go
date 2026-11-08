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
)

// ClickHouse error codes used for classification.
const (
	codeUnknownDatabase = 81
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
		return "The database does not exist or the user has no access to it. Create it (CREATE DATABASE), grant the user access, choose another database, or set clickhouse.create_database = true."
	case KindPermission:
		return "The user lacks a required privilege. See \"ClickHouse permissions\" in the README for the grants Vario needs."
	case KindSchema:
		return "The database contains objects with Vario's table or view names that Vario did not create or cannot use. Use a dedicated database, or remove or rename those objects."
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
	Grant string // e.g. "INSERT ON vario.*"
	// Required is false for grants that are only needed in some situations.
	Required bool
	Granted  bool
}

// Issue is a problem found by a connection check that does not prevent
// talking to ClickHouse but may prevent delivery.
type Issue struct {
	Kind    Kind
	Message string
}

// Report is the outcome of a successful connection check.
type Report struct {
	Version        string
	User           string
	Database       string
	CreateDatabase bool
	Schema         SchemaStatus
	Privileges     []Privilege
	// PrivilegesUnknown explains why privileges could not be checked.
	PrivilegesUnknown string
	Issues            []Issue
}

// Ready reports whether the collector can deliver without further action.
// Missing tables and views do not count: they are created on first delivery.
func (r *Report) Ready() bool { return len(r.Issues) == 0 }

// CheckConnection verifies, in order: that the endpoint is reachable, that the
// credentials are accepted, whether the database exists, whether Vario's
// schema in it is compatible, and whether the user has the privileges Vario
// needs. It never creates or modifies anything. A *CheckError is returned if
// a step fails so that later steps cannot run; other findings are Issues.
func (c *Client) CheckConnection(ctx context.Context) (*Report, error) {
	r := &Report{User: c.user, Database: c.database, CreateDatabase: c.createDatabase}
	if err := c.Ping(ctx); err != nil {
		return nil, &CheckError{"reach", Classify(err), err}
	}
	ver, err := c.ServerVersion(ctx)
	if err != nil {
		return nil, &CheckError{"authenticate", Classify(err), err}
	}
	r.Version = ver
	st, err := c.InspectSchema(ctx)
	if err != nil {
		return nil, &CheckError{"inspect", Classify(err), err}
	}
	r.Schema = st
	db := quoteIdent(c.database) + ".*"
	type want struct {
		grant    string
		required bool
	}
	var wants []want
	if !st.DatabaseExists {
		if c.createDatabase {
			wants = append(wants, want{"CREATE DATABASE ON " + db, true})
		} else {
			r.Issues = append(r.Issues, Issue{KindDatabaseMissing, fmt.Sprintf(
				"database %s does not exist or user %s has no access to it, and clickhouse.create_database is false", c.database, c.user)})
		}
	}
	wants = append(wants, want{"SELECT ON " + db, true}, want{"INSERT ON " + db, true})
	if len(st.MissingTables) > 0 {
		wants = append(wants, want{"CREATE TABLE ON " + db, true})
	}
	if len(st.MissingViews) > 0 || len(st.OutdatedViews) > 0 {
		wants = append(wants, want{"CREATE VIEW ON " + db, true})
	}
	if len(st.OutdatedViews) > 0 {
		wants = append(wants, want{"DROP VIEW ON " + db, false})
	}
	for _, p := range st.Problems {
		r.Issues = append(r.Issues, Issue{KindSchema, p})
	}
	for _, w := range wants {
		granted, err := c.checkGrant(ctx, w.grant)
		if err != nil {
			r.Privileges = nil
			r.PrivilegesUnknown = "privileges could not be verified (CHECK GRANT requires ClickHouse 24.5 or newer): " + err.Error()
			break
		}
		r.Privileges = append(r.Privileges, Privilege{Grant: strings.ReplaceAll(w.grant, "`", ""), Required: w.required, Granted: granted})
	}
	for _, p := range r.Privileges {
		if p.Required && !p.Granted {
			r.Issues = append(r.Issues, Issue{KindPermission, fmt.Sprintf("user %s lacks %s", c.user, p.Grant)})
		}
	}
	return r, nil
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
