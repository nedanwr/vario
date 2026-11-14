package clickhouse

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"regexp"
	"sort"
	"strings"

	"github.com/nedanwr/vario/internal/config"
)

// Admin is a temporary administrative connection, used only for explicitly
// authorized provisioning: discovering databases and users, creating them,
// granting privileges and migrating Vario's schema. Its credentials are kept
// in memory for the duration of the command and never stored.
type Admin struct {
	*Client
}

// NewAdmin connects as an administrative user. The connection settings
// (endpoint, TLS, timeouts) come from cfg; only the user and password differ
// from the operational connection.
func NewAdmin(cfg config.ClickHouseConfig, user, password string) (*Admin, error) {
	cfg.Username = user
	c, err := New(cfg, password)
	if err != nil {
		return nil, err
	}
	return &Admin{Client: c}, nil
}

// ForDatabase returns an administrative connection for another database.
func (a *Admin) ForDatabase(db string) *Admin { return &Admin{Client: a.WithDatabase(db)} }

// Authenticate checks reachability and the credentials and returns the server version.
func (a *Admin) Authenticate(ctx context.Context) (string, error) {
	if err := a.Ping(ctx); err != nil {
		return "", &CheckError{"reach", Classify(err), err}
	}
	v, err := a.ServerVersion(ctx)
	if err != nil {
		return "", &CheckError{"authenticate", Classify(err), err}
	}
	return v, nil
}

// SystemDatabases are hidden from database selection by default.
var SystemDatabases = []string{"system", "INFORMATION_SCHEMA", "information_schema", "default"}

// IsSystemDatabase reports whether name is one of SystemDatabases.
func IsSystemDatabase(name string) bool {
	for _, s := range SystemDatabases {
		if name == s {
			return true
		}
	}
	return false
}

// ListDatabases lists the databases visible to the administrative user.
// ClickHouse only lists databases the user has some privilege on, so the
// list may be incomplete for restricted administrators.
func (a *Admin) ListDatabases(ctx context.Context) ([]string, error) {
	var rows []struct {
		Name string `json:"name"`
	}
	if err := a.Query(ctx, "SELECT name FROM system.databases ORDER BY name", &rows); err != nil {
		return nil, err
	}
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.Name
	}
	return out, nil
}

// FilterDatabases returns the names to offer: system databases only when
// includeSystem is set, and only names containing filter (case-insensitive).
func FilterDatabases(names []string, includeSystem bool, filter string) []string {
	filter = strings.ToLower(strings.TrimSpace(filter))
	var out []string
	for _, n := range names {
		if !includeSystem && IsSystemDatabase(n) {
			continue
		}
		if filter != "" && !strings.Contains(strings.ToLower(n), filter) {
			continue
		}
		out = append(out, n)
	}
	return out
}

// User is a ClickHouse user as listed in system.users.
type User struct {
	Name string
	// Storage is the access storage the user is defined in, for example
	// local_directory (created with SQL), replicated, or users_xml (server
	// configuration files), ldap.
	Storage string
}

// Modifiable reports whether the user can be changed with SQL (GRANT).
// Users defined in server configuration files or external directories
// (LDAP) are read-only.
func (u User) Modifiable() bool {
	switch u.Storage {
	case "local_directory", "replicated", "memory":
		return true
	}
	return false
}

// StorageNote explains a read-only user's storage.
func (u User) StorageNote() string {
	switch {
	case u.Modifiable():
		return ""
	case strings.Contains(u.Storage, "xml"):
		return "defined in the server configuration files; privileges cannot be changed with SQL"
	case u.Storage == "ldap":
		return "managed by LDAP; privileges cannot be changed with SQL"
	case u.Storage == "":
		return "storage unknown; it may not be modifiable with SQL"
	}
	return "stored in " + u.Storage + "; it may not be modifiable with SQL"
}

// ListUsers lists ClickHouse users. It needs the SHOW USERS privilege;
// managed services may restrict it.
func (a *Admin) ListUsers(ctx context.Context) ([]User, error) {
	var rows []struct {
		Name    string `json:"name"`
		Storage string `json:"storage"`
	}
	if err := a.Query(ctx, "SELECT name, storage FROM system.users ORDER BY name", &rows); err != nil {
		return nil, err
	}
	out := make([]User, len(rows))
	for i, r := range rows {
		out[i] = User{r.Name, r.Storage}
	}
	return out, nil
}

// LookupUser returns a user by name. ok is false if it does not exist (or
// is not visible).
func (a *Admin) LookupUser(ctx context.Context, name string) (User, bool, error) {
	var rows []struct {
		Name    string `json:"name"`
		Storage string `json:"storage"`
	}
	if err := a.Query(ctx, fmt.Sprintf("SELECT name, storage FROM system.users WHERE name = '%s'", escapeString(name)), &rows); err != nil {
		return User{}, false, err
	}
	if len(rows) == 0 {
		return User{}, false, nil
	}
	return User{rows[0].Name, rows[0].Storage}, true, nil
}

var userNameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.-]{0,63}$`)

// CheckUserName validates the name of a user Vario creates.
func CheckUserName(name string) error {
	if !userNameRe.MatchString(name) {
		return errors.New("must start with a letter or underscore and contain only letters, digits, '_', '.' and '-' (at most 64 characters)")
	}
	return nil
}

// Requirement is one privilege the operational user needs.
type Requirement struct {
	Access string // SELECT or INSERT
	Table  string // object in the Vario database
}

func (r Requirement) String(db string) string {
	return fmt.Sprintf("%s ON %s.%s", r.Access, db, r.Table)
}

// OperationalRequirements lists the privileges of the operational user:
// SELECT and INSERT on Vario's tables and on vario_checkpoints (the collector
// reads and writes its delivery checkpoint), SELECT on its views and on
// vario_meta. Grants are scoped to these objects, so the user cannot read or change
// unrelated tables in a shared database, and holds no global privileges, no
// DDL privileges and no GRANT OPTION.
func OperationalRequirements() []Requirement {
	var out []Requirement
	for _, t := range requiredTables() {
		out = append(out, Requirement{"SELECT", t}, Requirement{"INSERT", t})
	}
	for _, v := range viewDefs("\x00") {
		out = append(out, Requirement{"SELECT", v.name})
	}
	return append(out, Requirement{"SELECT", MetaTable}, Requirement{"SELECT", CheckpointTable}, Requirement{"INSERT", CheckpointTable})
}

// GrantPlan returns GRANT statements for the given requirements, one per object.
func GrantPlan(user, db string, reqs []Requirement) *Plan {
	p := &Plan{Database: db}
	byTable := map[string][]string{}
	var order []string
	for _, r := range reqs {
		if _, ok := byTable[r.Table]; !ok {
			order = append(order, r.Table)
		}
		byTable[r.Table] = append(byTable[r.Table], r.Access)
	}
	for _, t := range order {
		access := strings.Join(byTable[t], ", ")
		p.Steps = append(p.Steps, Step{Kind: StepPrivileges, Summary: fmt.Sprintf("grant %s on %s.%s to %s", access, db, t, user),
			SQL: fmt.Sprintf("GRANT %s ON %s.%s TO %s", access, quoteIdent(db), quoteIdent(t), quoteIdent(user))})
	}
	return p
}

// GrantStatus is the outcome of inspecting a user's grants.
type GrantStatus int

const (
	// GrantsSufficient: every required privilege is granted directly or
	// through a default role. Effective access is still verified by logging in.
	GrantsSufficient GrantStatus = iota
	// GrantsInsufficient: the inspection was complete and privileges are missing.
	GrantsInsufficient
	// GrantsInconclusive: grants could not be fully evaluated (no access to
	// system tables, partial revokes, non-default roles, column-level grants).
	GrantsInconclusive
)

func (s GrantStatus) String() string {
	return [...]string{"sufficient", "insufficient", "inconclusive"}[s]
}

// GrantAssessment is the result of AssessGrants.
type GrantAssessment struct {
	Status  GrantStatus
	Missing []Requirement
	// Notes explain an inconclusive result and the limits of the inspection.
	Notes []string
	// Excess lists privileges beyond what Vario needs (global or GRANT OPTION).
	Excess []string
	Roles  []string
}

type grantRow struct {
	User      *string `json:"user_name"`
	Role      *string `json:"role_name"`
	Access    string  `json:"access_type"`
	Database  *string `json:"database"`
	Table     *string `json:"table"`
	Column    *string `json:"column"`
	Partial   int     `json:"is_partial_revoke"`
	GrantOpt  int     `json:"grant_option"`
	viaRole   string
	activeVia bool
}

// AssessGrants inspects the grants of user (directly and through roles) for
// the operational requirements on database db. It reads system.grants and
// system.role_grants, which shows granted privileges, not effective access:
// settings profiles (readonly), quotas, host restrictions and the roles
// actually enabled in a session can still prevent access. The final answer
// is always an operational login check (CheckConnection as that user).
func (a *Admin) AssessGrants(ctx context.Context, user, db string) (GrantAssessment, error) {
	var res GrantAssessment
	res.Notes = append(res.Notes, "settings profiles (readonly), quotas and host restrictions are not evaluated here; logging in as the user verifies them")
	type roleRow struct {
		Role    string `json:"granted_role_name"`
		Default int    `json:"granted_role_is_default"`
	}
	active := map[string]bool{} // role -> reachable through default roles
	var queue []string
	var rr []roleRow
	if err := a.Query(ctx, fmt.Sprintf("SELECT granted_role_name, granted_role_is_default FROM system.role_grants WHERE user_name = '%s'", escapeString(user)), &rr); err != nil {
		res.Status = GrantsInconclusive
		res.Notes = append(res.Notes, "roles could not be inspected: "+err.Error())
		return res, nil
	}
	for _, r := range rr {
		if _, seen := active[r.Role]; !seen {
			queue = append(queue, r.Role)
		}
		active[r.Role] = active[r.Role] || r.Default == 1
	}
	for len(queue) > 0 { // roles granted to roles
		role := queue[0]
		queue = queue[1:]
		var nested []roleRow
		if err := a.Query(ctx, fmt.Sprintf("SELECT granted_role_name, granted_role_is_default FROM system.role_grants WHERE role_name = '%s'", escapeString(role)), &nested); err != nil {
			res.Status = GrantsInconclusive
			res.Notes = append(res.Notes, "roles could not be inspected: "+err.Error())
			return res, nil
		}
		for _, n := range nested {
			if _, seen := active[n.Role]; !seen {
				queue = append(queue, n.Role)
				active[n.Role] = active[role]
			} else if active[role] {
				active[n.Role] = true
			}
		}
	}
	for r := range active {
		res.Roles = append(res.Roles, r)
	}
	sort.Strings(res.Roles)

	where := fmt.Sprintf("user_name = '%s'", escapeString(user))
	for _, r := range res.Roles {
		where += fmt.Sprintf(" OR role_name = '%s'", escapeString(r))
	}
	var grants []grantRow
	if err := a.Query(ctx, "SELECT user_name, role_name, access_type, database, table, column, is_partial_revoke, grant_option FROM system.grants WHERE "+where, &grants); err != nil {
		res.Status = GrantsInconclusive
		res.Notes = append(res.Notes, "grants could not be inspected: "+err.Error())
		return res, nil
	}
	for i := range grants {
		g := &grants[i]
		if g.Role != nil {
			g.viaRole, g.activeVia = *g.Role, active[*g.Role]
		} else {
			g.activeVia = true
		}
		if g.Database == nil && g.Partial == 0 {
			res.Excess = append(res.Excess, fmt.Sprintf("%s ON *.*%s", g.Access, via(g.viaRole)))
		}
		if g.GrantOpt == 1 && g.Partial == 0 {
			res.Excess = append(res.Excess, fmt.Sprintf("%s ON %s WITH GRANT OPTION%s", g.Access, scope(g), via(g.viaRole)))
		}
	}
	inconclusive := false
	notes := map[string]bool{}
	note := func(s string) {
		if !notes[s] {
			notes[s] = true
			res.Notes = append(res.Notes, s)
		}
	}
	for _, req := range OperationalRequirements() {
		matches := func(g grantRow) bool {
			return (g.Access == req.Access || g.Access == "ALL") &&
				(g.Database == nil || *g.Database == db) && (g.Table == nil || *g.Table == req.Table)
		}
		covered, coveredInactive, revoked := false, false, false
		for _, g := range grants {
			if !matches(g) {
				continue
			}
			switch {
			case g.Partial == 1:
				revoked = true
			case g.Column != nil:
				note(fmt.Sprintf("column-level grant %s on %s cannot be evaluated", g.Access, scope(&g)))
			case g.activeVia:
				covered = true
			default:
				coveredInactive = true
				note(fmt.Sprintf("role %s grants %s but is not a default role, so it may not be active in Vario's sessions", g.viaRole, req.String(db)))
			}
		}
		switch {
		case revoked:
			inconclusive = true
			note(fmt.Sprintf("partial revokes affect %s; the effective privilege cannot be determined here", req.String(db)))
		case covered:
			continue
		case coveredInactive:
			inconclusive = true
		}
		res.Missing = append(res.Missing, req)
	}
	sort.Strings(res.Excess)
	switch {
	case inconclusive:
		res.Status = GrantsInconclusive
	case len(res.Missing) > 0:
		res.Status = GrantsInsufficient
	default:
		res.Status = GrantsSufficient
	}
	return res, nil
}

func via(role string) string {
	if role == "" {
		return ""
	}
	return " (via role " + role + ")"
}

func scope(g *grantRow) string {
	d, t := "*", "*"
	if g.Database != nil {
		d = *g.Database
	}
	if g.Table != nil {
		t = *g.Table
	}
	return d + "." + t
}

// CreateUserStep creates a dedicated operational user authenticated by a
// salted SHA-256 hash computed locally, so the password itself is never sent
// to ClickHouse or shown. Plain CREATE USER (without IF NOT EXISTS) fails if
// the user exists, so an existing user is never modified by accident.
func CreateUserStep(user, password, db string) (Step, error) {
	if err := CheckUserName(user); err != nil {
		return Step{}, fmt.Errorf("user name %q %w", user, err)
	}
	var salt [16]byte
	if _, err := rand.Read(salt[:]); err != nil {
		return Step{}, err
	}
	saltHex := strings.ToUpper(hex.EncodeToString(salt[:]))
	sum := sha256.Sum256([]byte(password + saltHex))
	format := "CREATE USER %s IDENTIFIED WITH sha256_hash BY '%s' SALT '%s' DEFAULT DATABASE %s"
	return Step{
		Kind:    StepUser,
		Summary: fmt.Sprintf("create user %s (password authentication; default database %s; no privileges yet)", user, db),
		SQL:     fmt.Sprintf(format, quoteIdent(user), "<salted SHA-256 of the password>", "<random>", quoteIdent(db)),
		exec:    fmt.Sprintf(format, quoteIdent(user), strings.ToUpper(hex.EncodeToString(sum[:])), saltHex, quoteIdent(db)),
		secrets: []string{strings.ToUpper(hex.EncodeToString(sum[:])), saltHex, password},
	}, nil
}

const passwordAlphabet = "ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz23456789"

// GeneratePassword returns a random password of 32 characters from an
// unambiguous alphanumeric alphabet (about 186 bits of entropy), using
// crypto/rand. Alphanumeric passwords need no escaping in headers, shells or
// configuration files.
func GeneratePassword() (string, error) {
	b := make([]byte, 32)
	max := big.NewInt(int64(len(passwordAlphabet)))
	for i := range b {
		n, err := rand.Int(rand.Reader, max)
		if err != nil {
			return "", err
		}
		b[i] = passwordAlphabet[n.Int64()]
	}
	return string(b), nil
}
