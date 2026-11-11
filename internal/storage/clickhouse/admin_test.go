package clickhouse

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strings"
	"testing"

	"github.com/nedanwr/vario/internal/model"
)

func TestDiscovery(t *testing.T) {
	f, srv := newFake(t)
	admin := &Admin{testClient(t, srv.URL)}
	f.dbs = []string{"INFORMATION_SCHEMA", "analytics", "default", "information_schema", "system", "vario", "vario_staging"}
	all, err := admin.ListDatabases(bg)
	if err != nil || len(all) != 7 {
		t.Fatalf("list databases: %v %v", all, err)
	}
	if got := FilterDatabases(all, false, ""); strings.Join(got, ",") != "analytics,vario,vario_staging" {
		t.Fatalf("system databases must be hidden by default: %v", got)
	}
	if got := FilterDatabases(all, true, ""); len(got) != 7 {
		t.Fatalf("system databases can be revealed: %v", got)
	}
	if got := FilterDatabases(all, false, "VARIO"); strings.Join(got, ",") != "vario,vario_staging" {
		t.Fatalf("filter: %v", got)
	}

	f.users = []string{"default|users_xml", "grafana|local_directory", "ldap_user|ldap", "vario|replicated"}
	users, err := admin.ListUsers(bg)
	if err != nil || len(users) != 4 {
		t.Fatalf("list users: %v %v", users, err)
	}
	for _, u := range users {
		want := u.Name == "grafana" || u.Name == "vario"
		if u.Modifiable() != want || (u.StorageNote() == "") != want {
			t.Errorf("%s (%s): modifiable %v, note %q", u.Name, u.Storage, u.Modifiable(), u.StorageNote())
		}
	}
	if u, ok, err := admin.LookupUser(bg, "grafana"); err != nil || !ok || u.Storage != "local_directory" {
		t.Fatalf("lookup: %+v %v %v", u, ok, err)
	}
	if _, ok, err := admin.LookupUser(bg, "nobody"); err != nil || ok {
		t.Fatalf("lookup missing: %v %v", ok, err)
	}
	// Restricted visibility is an error the caller turns into manual entry.
	f.noSystem["users"] = true
	if _, err := admin.ListUsers(bg); err == nil || Classify(err) != KindPermission {
		t.Fatalf("SHOW USERS denied: %v", err)
	}
	readOnly(t, f)
}

// grantsFor returns system.grants rows for every operational requirement.
func grantsFor(user, role, db string) []string {
	var out []string
	for _, r := range OperationalRequirements() {
		out = append(out, strings.Join([]string{user, role, r.Access, db, r.Table, "0", "0"}, "|"))
	}
	return out
}

func TestAssessGrants(t *testing.T) {
	f, srv := newFake(t)
	admin := &Admin{testClient(t, srv.URL)}

	// Direct, object-scoped grants: sufficient.
	f.grants = grantsFor("vario", "", "vario")
	as, err := admin.AssessGrants(bg, "vario", "vario")
	if err != nil || as.Status != GrantsSufficient || len(as.Missing) != 0 || len(as.Excess) != 0 {
		t.Fatalf("direct grants: %+v %v", as, err)
	}
	// Database-wide grants cover every object.
	f.grants = []string{"vario||SELECT|vario||0|0", "vario||INSERT|vario||0|0"}
	if as, _ = admin.AssessGrants(bg, "vario", "vario"); as.Status != GrantsSufficient {
		t.Fatalf("database grants: %+v", as)
	}
	// Grants on another database do not count: insufficient, with the missing list.
	f.grants = []string{"vario||SELECT|other||0|0", "vario||INSERT|other||0|0"}
	as, _ = admin.AssessGrants(bg, "vario", "vario")
	if as.Status != GrantsInsufficient || len(as.Missing) != len(OperationalRequirements()) {
		t.Fatalf("other database: %+v", as)
	}
	// Through a default role (nested): sufficient.
	f.grants = grantsFor("", "inner", "vario")
	f.roleGrants = []string{"vario|outer|1", "outer|inner|0"}
	if as, _ = admin.AssessGrants(bg, "vario", "vario"); as.Status != GrantsSufficient || len(as.Roles) != 2 {
		t.Fatalf("default role: %+v", as)
	}
	// Through a role that is not a default role: inconclusive.
	f.roleGrants = []string{"vario|outer|0", "outer|inner|0"}
	if as, _ = admin.AssessGrants(bg, "vario", "vario"); as.Status != GrantsInconclusive || !strings.Contains(strings.Join(as.Notes, " "), "not a default role") {
		t.Fatalf("non-default role: %+v", as)
	}
	// Partial revokes: inconclusive.
	f.roleGrants = nil
	f.grants = append(grantsFor("vario", "", "vario"), "vario||INSERT|vario|messages|1|0")
	if as, _ = admin.AssessGrants(bg, "vario", "vario"); as.Status != GrantsInconclusive {
		t.Fatalf("partial revoke: %+v", as)
	}
	// Global and grant-option privileges are flagged as excess.
	f.grants = []string{"vario||ALL|||0|1"}
	as, _ = admin.AssessGrants(bg, "vario", "vario")
	if as.Status != GrantsSufficient || len(as.Excess) != 2 {
		t.Fatalf("global privileges: %+v", as)
	}
	// No access to system.grants: inconclusive, not insufficient.
	f.noSystem["grants"] = true
	if as, _ = admin.AssessGrants(bg, "vario", "vario"); as.Status != GrantsInconclusive {
		t.Fatalf("no system.grants: %+v", as)
	}
	readOnly(t, f)
}

func TestGrantPlanIsScopedToVarioObjects(t *testing.T) {
	p := GrantPlan("we`ird", "vario", OperationalRequirements())
	// Tables, views, vario_meta and vario_checkpoints.
	if len(p.Steps) != len(model.Tables)+len(viewDefs("x"))+2 {
		t.Fatalf("one statement per object: %d", len(p.Steps))
	}
	for _, s := range p.Steps {
		if s.Kind != StepPrivileges || !strings.HasPrefix(s.SQL, "GRANT ") || !strings.HasSuffix(s.SQL, " TO `we``ird`") {
			t.Fatalf("unexpected statement: %s", s.SQL)
		}
		if strings.Contains(s.SQL, "*") || strings.Contains(s.SQL, "GRANT OPTION") || strings.Contains(s.SQL, "ALL") ||
			strings.Contains(s.SQL, "CREATE") || strings.Contains(s.SQL, "DROP") || strings.Contains(s.SQL, "ALTER") {
			t.Fatalf("grant is broader than needed: %s", s.SQL)
		}
	}
	if p.Steps[0].SQL != "GRANT SELECT, INSERT ON `vario`.`sources` TO `we``ird`" {
		t.Fatalf("table grant: %s", p.Steps[0].SQL)
	}
	// The metadata stays read-only for the collector; it writes checkpoints only.
	if meta := p.Steps[len(p.Steps)-2].SQL; meta != "GRANT SELECT ON `vario`.`vario_meta` TO `we``ird`" {
		t.Fatalf("metadata grant: %s", meta)
	}
	if last := p.Steps[len(p.Steps)-1].SQL; last != "GRANT SELECT, INSERT ON `vario`.`vario_checkpoints` TO `we``ird`" {
		t.Fatalf("checkpoint grant: %s", last)
	}
}

func TestCreateUserStep(t *testing.T) {
	pw := "s3cret-Password"
	s, err := CreateUserStep("vario", pw, "vario")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(s.SQL, pw) || strings.Contains(s.exec, pw) {
		t.Fatal("the password itself must never be part of a statement")
	}
	m := regexp.MustCompile(`^CREATE USER ` + "`vario`" + ` IDENTIFIED WITH sha256_hash BY '([0-9A-F]{64})' SALT '([0-9A-F]{32})' DEFAULT DATABASE ` + "`vario`$").FindStringSubmatch(s.exec)
	if m == nil {
		t.Fatalf("statement: %s", s.exec)
	}
	sum := sha256.Sum256([]byte(pw + m[2]))
	if strings.ToUpper(hex.EncodeToString(sum[:])) != m[1] {
		t.Fatal("hash must be SHA-256 of the password followed by the salt")
	}
	if strings.Contains(s.SQL, m[1]) || strings.Contains(s.SQL, m[2]) || strings.Contains(s.SQL, "IF NOT EXISTS") {
		t.Fatalf("the displayed statement must not contain the hash, and must fail for an existing user: %s", s.SQL)
	}
	for _, bad := range []string{"", "a b", "x;DROP", "`v`", strings.Repeat("a", 65), "1abc"} {
		if _, err := CreateUserStep(bad, pw, "vario"); err == nil {
			t.Errorf("user name %q must be rejected", bad)
		}
	}
}

func TestGeneratePassword(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		pw, err := GeneratePassword()
		if err != nil {
			t.Fatal(err)
		}
		if len(pw) != 32 || strings.Trim(pw, passwordAlphabet) != "" {
			t.Fatalf("unexpected password %q", pw)
		}
		if seen[pw] {
			t.Fatal("generated passwords must not repeat")
		}
		seen[pw] = true
	}
}

func TestQuoteIdent(t *testing.T) {
	for in, want := range map[string]string{"vario": "`vario`", "a`b": "`a``b`", `a\b`: "`a\\\\b`"} {
		if got := quoteIdent(in); got != want {
			t.Errorf("quoteIdent(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestCreateUserErrorsAreRedacted(t *testing.T) {
	f, srv := newFake(t)
	admin := &Admin{testClient(t, srv.URL)}
	step, err := CreateUserStep("vario", "pw-secret", "vario")
	if err != nil {
		t.Fatal(err)
	}
	hash := step.secrets[0]
	f.fail["CREATE USER"] = 62
	srvErr := &Error{Status: 500, Code: 62, Message: "Syntax error near '" + hash + "' SALT"}
	if got := redact(srvErr, step.secrets); strings.Contains(got.Error(), hash) {
		t.Fatalf("hash not redacted: %v", got)
	}
	res := admin.Apply(bg, &Plan{Steps: []Step{step}}, nil)
	if res.OK() || strings.Contains(res.Err.Error(), hash) || strings.Contains(res.Err.Error(), "pw-secret") {
		t.Fatalf("apply error: %v", res.Err)
	}
}
