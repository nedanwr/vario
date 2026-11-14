package clickhouse

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/nedanwr/vario/internal/config"
	"github.com/nedanwr/vario/internal/model"
)

// chAs returns a client for the integration server as the given user.
func chAs(t *testing.T, user, password, db string) *Client {
	t.Helper()
	cfg := config.Default().ClickHouse
	cfg.URL = os.Getenv("VARIO_TEST_CLICKHOUSE_URL")
	cfg.Username, cfg.Database = user, db
	c, err := New(cfg, password)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func mustExec(t *testing.T, c *Client, q string) {
	t.Helper()
	if err := c.Exec(context.Background(), q); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

func objectsIn(t *testing.T, c *Client, db string) []string {
	t.Helper()
	var rows []struct {
		Name    string `json:"name"`
		Engine  string `json:"engine"`
		Comment string `json:"comment"`
	}
	if err := c.Query(context.Background(), fmt.Sprintf("SELECT name, engine, comment FROM system.tables WHERE database = '%s'", db), &rows); err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, r := range rows {
		out = append(out, r.Name+"/"+r.Engine+"/"+r.Comment)
	}
	sort.Strings(out)
	return out
}

func grantsOf(t *testing.T, c *Client, user string) []string {
	t.Helper()
	var rows []struct {
		G string `json:"g"`
	}
	if err := c.Query(context.Background(), fmt.Sprintf("SELECT concat(access_type, ' ', ifNull(database, '*'), '.', ifNull(table, '*'), if(grant_option, ' WITH GRANT OPTION', '')) AS g FROM system.grants WHERE user_name = '%s' ORDER BY g", user), &rows); err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, r := range rows {
		out = append(out, r.G)
	}
	return out
}

// TestIntegrationProvisioning covers the administrative path end to end
// against a real server: discovery, a new database, a dedicated user with a
// locally hashed password, object-scoped grants, the schema migration, the
// operational login check (including the insert probe), idempotent reruns,
// restricted operational users, existing users with missing privileges,
// read-only (users.xml) users, incompatible objects, partial failures, and
// isolation from unrelated databases. It needs a user with access management
// (see `make clickhouse-up`).
func TestIntegrationProvisioning(t *testing.T) {
	if os.Getenv("VARIO_TEST_CLICKHOUSE_URL") == "" {
		t.Skip("set VARIO_TEST_CLICKHOUSE_URL to run ClickHouse integration tests")
	}
	ctx := context.Background()
	adminUser, adminPW := envOr("VARIO_TEST_CLICKHOUSE_USER", "default"), os.Getenv("VARIO_TEST_CLICKHOUSE_PASSWORD")
	n := time.Now().UnixNano()
	db, other, bad := fmt.Sprintf("vario_prov_%d", n), fmt.Sprintf("unrelated_%d", n), fmt.Sprintf("vario_bad_%d", n)
	opUser, existing, ro := fmt.Sprintf("vario_op_%d", n), fmt.Sprintf("vario_existing_%d", n), fmt.Sprintf("vario_ro_%d", n)
	cfg := config.Default().ClickHouse
	cfg.URL = os.Getenv("VARIO_TEST_CLICKHOUSE_URL")
	cfg.Database = db
	admin, err := NewAdmin(cfg, adminUser, adminPW)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Authenticate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := admin.Exec(ctx, fmt.Sprintf("CREATE USER %s IDENTIFIED BY 'ep'", existing)); err != nil {
		t.Skipf("the integration user cannot manage users: %v", err)
	}
	t.Cleanup(func() {
		for _, u := range []string{opUser, existing, ro} {
			_ = admin.Exec(ctx, "DROP USER IF EXISTS "+quoteIdent(u))
		}
		_ = admin.Exec(ctx, "DROP SETTINGS PROFILE IF EXISTS "+quoteIdent(ro+"_profile"))
		for _, d := range []string{db, other, bad} {
			_ = admin.Exec(ctx, "DROP DATABASE IF EXISTS "+quoteIdent(d))
		}
	})

	// An unrelated database on the same server, with objects named like Vario's.
	mustExec(t, admin.Client, "CREATE DATABASE "+other)
	mustExec(t, admin.Client, fmt.Sprintf("CREATE TABLE %s.messages (x String) ENGINE = MergeTree ORDER BY x", other))
	mustExec(t, admin.Client, fmt.Sprintf("INSERT INTO %s.messages VALUES ('a'), ('b'), ('c')", other))
	mustExec(t, admin.Client, fmt.Sprintf("CREATE VIEW %s.tool_usage AS SELECT 1 AS one", other))
	otherBefore := objectsIn(t, admin.Client, other)

	// 1. Discovery.
	dbs, err := admin.ListDatabases(ctx)
	if err != nil || !contains(dbs, other) || contains(FilterDatabases(dbs, false, ""), "system") {
		t.Fatalf("list databases: %v %v", dbs, err)
	}
	users, err := admin.ListUsers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, u := range users {
		if u.Name == adminUser && u.Modifiable() {
			t.Fatalf("the users.xml integration user must be reported as not modifiable: %+v", u)
		}
		if u.Name == existing && !u.Modifiable() {
			t.Fatalf("a SQL-created user must be modifiable: %+v", u)
		}
	}

	// 2. Guided provisioning: database, schema, dedicated user, grants.
	pw, err := GeneratePassword()
	if err != nil {
		t.Fatal(err)
	}
	dbAdmin := admin.ForDatabase(db)
	schema, err := dbAdmin.PlanSchema(ctx)
	if err != nil || !schema.Has(StepDatabase) {
		t.Fatalf("schema plan: %+v %v", schema, err)
	}
	userStep, err := CreateUserStep(opUser, pw, db)
	if err != nil {
		t.Fatal(err)
	}
	plan := &Plan{Database: db}
	plan.Append(schema)
	plan.Steps = append(plan.Steps, userStep)
	plan.Append(GrantPlan(opUser, db, OperationalRequirements()))
	if res := dbAdmin.Apply(ctx, plan, nil); !res.OK() {
		t.Fatalf("provisioning: %v", res.Err)
	}
	st, err := dbAdmin.VerifyMigrated(ctx)
	if err != nil || st.DestinationID == "" || st.Recorded != SchemaVersion {
		t.Fatalf("verify: %+v %v", st, err)
	}
	for _, g := range grantsOf(t, admin.Client, opUser) {
		if strings.Contains(g, "*") || strings.Contains(g, "GRANT OPTION") || !strings.Contains(g, " "+db+".") {
			t.Fatalf("operational grants must be scoped to Vario's objects: %v", grantsOf(t, admin.Client, opUser))
		}
	}
	as, err := dbAdmin.AssessGrants(ctx, opUser, db)
	if err != nil || as.Status != GrantsSufficient || len(as.Excess) != 0 {
		t.Fatalf("assessment of the provisioned user: %+v %v", as, err)
	}

	// 3. The operational login works with the generated password and is restricted.
	op := chAs(t, opUser, pw, db)
	r, err := op.CheckConnection(ctx)
	if err != nil || !r.Ready() || r.Probe != "ok" || len(r.Excess) != 0 || !r.Schema.Complete() {
		t.Fatalf("operational check: %+v %v", r, err)
	}
	info, err := op.CheckDestination(ctx)
	if err != nil || info.ServerID != st.DestinationID {
		t.Fatalf("destination id as the operational user: %+v %v", info, err)
	}
	if err := op.Insert(ctx, model.TableProviderTurns, rowsBody(t, turn(1, u64(5), "complete")), "t1"); err != nil {
		t.Fatalf("insert as the operational user: %v", err)
	}
	// It reads and writes its delivery checkpoint; the metadata stays read-only.
	if err := op.WriteCheckpoint(ctx, "writer-1", 1); err != nil {
		t.Fatalf("checkpoint write as the operational user: %v", err)
	}
	if seq, found, err := op.ReadCheckpoint(ctx, "writer-1"); err != nil || !found || seq != 1 {
		t.Fatalf("checkpoint read as the operational user: %d %v %v", seq, found, err)
	}
	if err := op.Exec(ctx, fmt.Sprintf("INSERT INTO %s.%s (key, value) VALUES ('destination_id', 'forged')", db, MetaTable)); Classify(err) != KindPermission {
		t.Fatalf("the operational user must not write the metadata: %v", err)
	}
	var cnt []struct {
		N int `json:"n"`
	}
	if err := op.Query(ctx, fmt.Sprintf("SELECT count() AS n FROM %s.provider_turns", db), &cnt); err != nil || cnt[0].N != 1 {
		t.Fatalf("the probe must not write rows: %+v %v", cnt, err)
	}
	for _, q := range []string{
		fmt.Sprintf("CREATE TABLE %s.x (a UInt8) ENGINE = Memory", db),
		fmt.Sprintf("DROP TABLE %s.messages", db),
		"CREATE DATABASE vario_should_not_exist",
		fmt.Sprintf("SELECT count() FROM %s.messages", other),
		fmt.Sprintf("GRANT SELECT ON %s.* TO %s", db, existing),
	} {
		if err := op.Exec(ctx, q); Classify(err) != KindPermission {
			t.Fatalf("the operational user must not be allowed %q: %v", q, err)
		}
	}
	if _, err := chAs(t, opUser, "wrong", db).CheckConnection(ctx); err == nil {
		t.Fatal("a wrong password must fail")
	}

	// 4. Reruns are idempotent: nothing to plan, the user cannot be created twice.
	if p, err := dbAdmin.PlanSchema(ctx); err != nil || !p.Empty() {
		t.Fatalf("repeated plan: %+v %v", p, err)
	}
	if res := dbAdmin.Apply(ctx, &Plan{Steps: []Step{userStep}}, nil); res.OK() {
		t.Fatal("creating an existing user must fail instead of modifying it")
	}
	if u, ok, _ := dbAdmin.LookupUser(ctx, opUser); !ok || !u.Modifiable() {
		t.Fatalf("lookup: %+v %v", u, ok)
	}
	if _, err := chAs(t, opUser, pw, db).ServerVersion(ctx); err != nil {
		t.Fatalf("the failed CREATE USER must not change the password: %v", err)
	}

	// 5. An existing user without privileges: insufficient, then granted after "confirmation".
	as, _ = dbAdmin.AssessGrants(ctx, existing, db)
	if as.Status != GrantsInsufficient || len(as.Missing) != len(OperationalRequirements()) {
		t.Fatalf("existing user without grants: %+v", as)
	}
	if r, err := chAs(t, existing, "ep", db).CheckConnection(ctx); err != nil || r.Ready() {
		t.Fatalf("the login check must fail without privileges: %+v %v", r, err)
	}
	if res := dbAdmin.Apply(ctx, GrantPlan(existing, db, as.Missing), nil); !res.OK() {
		t.Fatal(res.Err)
	}
	if r, err := chAs(t, existing, "ep", db).CheckConnection(ctx); err != nil || !r.Ready() {
		t.Fatalf("after granting: %+v %v", r, err)
	}
	// Database-wide grants are recognized too, through a default role.
	role := existing + "_role"
	mustExec(t, admin.Client, "CREATE ROLE "+role)
	t.Cleanup(func() { _ = admin.Exec(ctx, "DROP ROLE IF EXISTS "+role) })
	mustExec(t, admin.Client, fmt.Sprintf("REVOKE ALL ON *.* FROM %s", existing))
	mustExec(t, admin.Client, fmt.Sprintf("GRANT SELECT, INSERT ON %s.* TO %s", db, role))
	mustExec(t, admin.Client, fmt.Sprintf("GRANT %s TO %s", role, existing))
	mustExec(t, admin.Client, fmt.Sprintf("SET DEFAULT ROLE %s TO %s", role, existing))
	if as, _ = dbAdmin.AssessGrants(ctx, existing, db); as.Status != GrantsSufficient {
		t.Fatalf("grants through a default role: %+v", as)
	}
	mustExec(t, admin.Client, fmt.Sprintf("SET DEFAULT ROLE NONE TO %s", existing))
	if as, _ = dbAdmin.AssessGrants(ctx, existing, db); as.Status != GrantsInconclusive {
		t.Fatalf("grants through a non-default role: %+v", as)
	}

	// 6. A read-only settings profile is detected by the login check.
	mustExec(t, admin.Client, fmt.Sprintf("CREATE SETTINGS PROFILE %s SETTINGS readonly = 1", quoteIdent(ro+"_profile")))
	mustExec(t, admin.Client, fmt.Sprintf("CREATE USER %s IDENTIFIED BY 'rp' SETTINGS PROFILE %s", ro, quoteIdent(ro+"_profile")))
	mustExec(t, admin.Client, fmt.Sprintf("GRANT SELECT, INSERT ON %s.* TO %s", db, ro))
	if r, err := chAs(t, ro, "rp", db).CheckConnection(ctx); err != nil || r.Ready() || r.ReadOnly != 1 {
		t.Fatalf("readonly profile: %+v %v", r, err)
	}

	if r, _ := chAs(t, ro, "rp", db).CheckConnection(ctx); r.Issues[0].Kind != KindRestriction {
		t.Fatalf("readonly is a session restriction: %+v", r.Issues)
	}

	// 6b. Restrictions the grants do not show: a settings constraint on a
	// setting every delivery uses, and an exhausted quota. CHECK GRANT
	// passes in both cases; the probe, which uses the delivery's settings,
	// fails like a real delivery would.
	cons, quota := ro+"_cons", ro+"_quota"
	t.Cleanup(func() {
		_ = admin.Exec(ctx, "DROP USER IF EXISTS "+quoteIdent(cons))
		_ = admin.Exec(ctx, "DROP USER IF EXISTS "+quoteIdent(quota))
		_ = admin.Exec(ctx, "DROP QUOTA IF EXISTS "+quoteIdent(quota+"_q"))
		_ = admin.Exec(ctx, "DROP SETTINGS PROFILE IF EXISTS "+quoteIdent(cons+"_profile"))
	})
	mustExec(t, admin.Client, fmt.Sprintf("CREATE SETTINGS PROFILE %s SETTINGS input_format_skip_unknown_fields = 0 CONST", quoteIdent(cons+"_profile")))
	mustExec(t, admin.Client, fmt.Sprintf("CREATE USER %s IDENTIFIED BY 'cp' SETTINGS PROFILE %s", cons, quoteIdent(cons+"_profile")))
	mustExec(t, admin.Client, fmt.Sprintf("GRANT SELECT, INSERT ON %s.* TO %s", db, cons))
	r, err = chAs(t, cons, "cp", db).CheckConnection(ctx)
	if err != nil || r.Ready() || r.Issues[0].Kind != KindRestriction || !strings.Contains(r.Probe, "rejected") {
		t.Fatalf("settings constraint: %+v %v", r, err)
	}
	for _, p := range r.Privileges {
		if !p.Granted {
			t.Fatalf("the grants are complete; only the probe can see the constraint: %+v", r.Privileges)
		}
	}
	if err := chAs(t, cons, "cp", db).Insert(ctx, model.TableProviderTurns, rowsBody(t, turn(2, u64(5), "complete")), "t2"); Classify(err) != KindRestriction {
		t.Fatalf("a real delivery fails the same way: %v", err)
	}
	mustExec(t, admin.Client, fmt.Sprintf("CREATE USER %s IDENTIFIED BY 'qp'", quota))
	mustExec(t, admin.Client, fmt.Sprintf("GRANT SELECT, INSERT ON %s.* TO %s", db, quota))
	mustExec(t, admin.Client, fmt.Sprintf("CREATE QUOTA %s FOR INTERVAL 1 hour MAX query_inserts = 1 TO %s", quoteIdent(quota+"_q"), quota))
	r, err = chAs(t, quota, "qp", db).CheckConnection(ctx)
	if err != nil || r.Ready() || r.Issues[0].Kind != KindRestriction {
		t.Fatalf("exhausted quota: %+v %v", r, err)
	}

	// 7. The users.xml integration user cannot be granted to with SQL.
	if res := dbAdmin.Apply(ctx, GrantPlan(adminUser, db, OperationalRequirements()[:1]), nil); res.OK() {
		t.Fatal("GRANT to a user defined in users.xml must fail")
	}

	// 8. An outdated view (from Vario 0.1: no comment, the first definition
	// without usage_scope) is replaced by a migration; data is kept.
	for _, v := range viewDefs(db) {
		if v.name == "token_usage_daily" {
			mustExec(t, admin.Client, fmt.Sprintf("CREATE OR REPLACE VIEW %s.%s AS %s", db, v.name, v.legacy[1]))
		}
	}
	if r, _ := op.CheckConnection(ctx); !r.Ready() || len(r.Warnings) != 1 {
		t.Fatalf("an outdated view is a warning, not a blocker: %+v", r)
	}
	if p, err := dbAdmin.PlanSchema(ctx); err != nil || len(p.Steps) != 1 {
		t.Fatalf("outdated view plan: %+v %v", p, err)
	} else if res := dbAdmin.Apply(ctx, p, nil); !res.OK() || len(res.Warnings) != 0 {
		t.Fatalf("view update: %+v", res)
	}
	if err := op.Query(ctx, fmt.Sprintf("SELECT count() AS n FROM %s.provider_turns", db), &cnt); err != nil || cnt[0].N != 1 {
		t.Fatalf("existing data must be preserved: %+v %v", cnt, err)
	}

	// 9. A legacy (version 1) schema is upgraded additively and keeps its data.
	mustExec(t, admin.Client, fmt.Sprintf("DROP TABLE %s.%s", db, MetaTable))
	if st, _ := dbAdmin.InspectSchema(ctx); st.Version() != 1 {
		t.Fatalf("legacy detection: %+v", st)
	}
	if info, err := op.CheckDestination(ctx); err != nil || info.ServerID != "" || len(info.Warnings) != 1 {
		t.Fatalf("legacy schema at runtime: %+v %v", info, err)
	}
	p, err := dbAdmin.PlanSchema(ctx)
	if err != nil || len(p.Steps) != 3 {
		t.Fatalf("legacy upgrade plan: %+v %v", p, err)
	}
	if res := dbAdmin.Apply(ctx, p, nil); !res.OK() {
		t.Fatal(res.Err)
	}
	if st2, err := dbAdmin.VerifyMigrated(ctx); err != nil || st2.DestinationID == st.DestinationID {
		t.Fatalf("a recreated metadata table has a new destination id: %+v %v", st2, err)
	}

	// 10. Incompatible objects stop the migration; nothing is created next to them.
	mustExec(t, admin.Client, "CREATE DATABASE "+bad)
	mustExec(t, admin.Client, fmt.Sprintf("CREATE TABLE %s.messages (id UInt64) ENGINE = MergeTree ORDER BY id", bad))
	var se *SchemaError
	if _, err := admin.ForDatabase(bad).PlanSchema(ctx); !errors.As(err, &se) {
		t.Fatalf("incompatible table: %v", err)
	}
	if objs := objectsIn(t, admin.Client, bad); len(objs) != 1 {
		t.Fatalf("nothing may be created next to incompatible objects: %v", objs)
	}

	// 11. A failure part-way is reported as partial, and a rerun resumes.
	fresh := fmt.Sprintf("vario_partial_%d", n)
	t.Cleanup(func() { _ = admin.Exec(ctx, "DROP DATABASE IF EXISTS "+fresh) })
	fa := admin.ForDatabase(fresh)
	p, _ = fa.PlanSchema(ctx)
	p.Steps = append(p.Steps[:4:4], append([]Step{{Kind: StepSchema, Summary: "broken", SQL: "CREATE TABLE broken syntax"}}, p.Steps[4:]...)...)
	res := fa.Apply(ctx, p, nil)
	if res.OK() || !res.Partial() || len(res.Applied) != 4 {
		t.Fatalf("partial failure: %+v", res)
	}
	if p, err = fa.PlanSchema(ctx); err != nil || p.Has(StepDatabase) {
		t.Fatalf("resumed plan: %+v %v", p, err)
	}
	if res := fa.Apply(ctx, p, nil); !res.OK() {
		t.Fatal(res.Err)
	}
	if _, err := fa.VerifyMigrated(ctx); err != nil {
		t.Fatal(err)
	}

	// 12. The unrelated database was never touched.
	if after := objectsIn(t, admin.Client, other); strings.Join(after, ",") != strings.Join(otherBefore, ",") {
		t.Fatalf("unrelated database changed: %v -> %v", otherBefore, after)
	}
	if err := admin.Query(ctx, fmt.Sprintf("SELECT count() AS n FROM %s.messages", other), &cnt); err != nil || cnt[0].N != 3 {
		t.Fatalf("unrelated data changed: %+v %v", cnt, err)
	}
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}
