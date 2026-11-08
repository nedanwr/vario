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
func chAs(t *testing.T, user, password, db string, create bool) *Client {
	t.Helper()
	cfg := config.Default().ClickHouse
	cfg.URL = os.Getenv("VARIO_TEST_CLICKHOUSE_URL")
	cfg.Username, cfg.Database, cfg.CreateDatabase = user, db, create
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

// TestIntegrationDatabasePolicy covers existing and Vario-managed databases,
// restricted users, idempotency and isolation from unrelated databases on a
// shared server. It needs a user with access management (see `make clickhouse-up`).
func TestIntegrationDatabasePolicy(t *testing.T) {
	if os.Getenv("VARIO_TEST_CLICKHOUSE_URL") == "" {
		t.Skip("set VARIO_TEST_CLICKHOUSE_URL to run ClickHouse integration tests")
	}
	ctx := context.Background()
	adminUser, adminPW := envOr("VARIO_TEST_CLICKHOUSE_USER", "default"), os.Getenv("VARIO_TEST_CLICKHOUSE_PASSWORD")
	n := time.Now().UnixNano()
	db, other, prov, bad := fmt.Sprintf("vario_pol_%d", n), fmt.Sprintf("unrelated_%d", n), fmt.Sprintf("vario_prov_%d", n), fmt.Sprintf("vario_bad_%d", n)
	user, reader := fmt.Sprintf("vario_lim_%d", n), fmt.Sprintf("vario_ins_%d", n)
	admin := chAs(t, adminUser, adminPW, db, true)
	if err := admin.Exec(ctx, fmt.Sprintf("CREATE USER %s IDENTIFIED BY 'lp'", user)); err != nil {
		t.Skipf("the integration user cannot manage users: %v", err)
	}
	t.Cleanup(func() {
		for _, q := range []string{"DROP USER IF EXISTS " + user, "DROP USER IF EXISTS " + reader} {
			_ = admin.Exec(ctx, q)
		}
		for _, d := range []string{db, other, prov, bad} {
			_ = admin.Exec(ctx, "DROP DATABASE IF EXISTS "+quoteIdent(d))
		}
	})
	mustExec(t, admin, fmt.Sprintf("CREATE USER %s IDENTIFIED BY 'rp'", reader))

	// An unrelated database on the same server, with objects named like Vario's.
	mustExec(t, admin, "CREATE DATABASE "+other)
	mustExec(t, admin, fmt.Sprintf("CREATE TABLE %s.messages (x String) ENGINE = MergeTree ORDER BY x", other))
	mustExec(t, admin, fmt.Sprintf("INSERT INTO %s.messages VALUES ('a'), ('b'), ('c')", other))
	mustExec(t, admin, fmt.Sprintf("CREATE VIEW %s.tool_usage AS SELECT 1 AS one", other))
	otherBefore := objectsIn(t, admin, other)

	// 1. Missing database, creation disabled: reported, nothing created.
	lim := chAs(t, user, "lp", db, false)
	if _, err := lim.InitSchema(ctx); !errors.Is(err, ErrDatabaseMissing) || Classify(err) != KindDatabaseMissing {
		t.Fatalf("missing database without creation: %v", err)
	}
	if r, err := lim.CheckConnection(ctx); err != nil || r.Ready() || r.Issues[0].Kind != KindDatabaseMissing {
		t.Fatalf("check, missing database: %+v %v", r, err)
	}

	// 2. Creation enabled, but the user lacks CREATE DATABASE: a permission error.
	limCreate := chAs(t, user, "lp", db, true)
	if _, err := limCreate.InitSchema(ctx); Classify(err) != KindPermission || !strings.Contains(err.Error(), "CREATE DATABASE privilege") {
		t.Fatalf("missing CREATE DATABASE privilege: %v", err)
	}
	if r, _ := limCreate.CheckConnection(ctx); r == nil || r.Ready() || r.Issues[0].Kind != KindPermission {
		t.Fatalf("check, missing CREATE DATABASE: %+v", r)
	}

	// 3. An administrator creates the database and grants exactly the documented
	// privileges (no CREATE DATABASE, no DROP). Both policies then work.
	mustExec(t, admin, "CREATE DATABASE "+db)
	mustExec(t, admin, fmt.Sprintf("GRANT SELECT, INSERT, CREATE TABLE, CREATE VIEW ON %s.* TO %s", db, user))
	if r, err := lim.CheckConnection(ctx); err != nil || !r.Ready() || r.Schema.DatabaseExists != true {
		t.Fatalf("check, existing database: %+v %v", r, err)
	}
	res, err := limCreate.InitSchema(ctx) // create_database = true reuses the existing database
	if err != nil || res.CreatedDatabase || len(res.CreatedTables) != len(model.Tables) || len(res.CreatedViews) != len(viewDefs(db)) {
		t.Fatalf("first schema init: %+v %v", res, err)
	}
	for i := 0; i < 2; i++ { // idempotent, without DROP VIEW
		res, err = lim.InitSchema(ctx)
		if err != nil || len(res.CreatedTables)+len(res.CreatedViews)+len(res.UpdatedViews)+len(res.Warnings) != 0 {
			t.Fatalf("repeated schema init must be a no-op: %+v %v", res, err)
		}
	}
	if r, _ := lim.CheckConnection(ctx); !r.Ready() || !r.Schema.Complete() {
		t.Fatalf("check, complete schema: %+v", r)
	}
	if err := lim.Insert(ctx, model.TableProviderTurns, rowsBody(t, turn(1, u64(5), "complete")), "t1"); err != nil {
		t.Fatalf("insert as restricted user: %v", err)
	}

	// 4. A view from an earlier Vario version (no tag) is outdated. Without DROP VIEW
	// it is kept with a warning; with it, it is replaced. Data is untouched.
	mustExec(t, admin, fmt.Sprintf("CREATE OR REPLACE VIEW %s.tool_usage AS SELECT source_id FROM %s.execution_events FINAL", db, db))
	res, err = lim.InitSchema(ctx)
	if err != nil || len(res.Warnings) != 1 || len(res.UpdatedViews) != 0 {
		t.Fatalf("outdated view without DROP VIEW: %+v %v", res, err)
	}
	mustExec(t, admin, fmt.Sprintf("GRANT DROP VIEW ON %s.* TO %s", db, user))
	if res, err = lim.InitSchema(ctx); err != nil || len(res.UpdatedViews) != 1 {
		t.Fatalf("outdated view with DROP VIEW: %+v %v", res, err)
	}
	var cnt []struct {
		N int `json:"n"`
	}
	if err := lim.Query(ctx, fmt.Sprintf("SELECT count() AS n FROM %s.provider_turns", db), &cnt); err != nil || cnt[0].N != 1 {
		t.Fatalf("existing data must be preserved: %+v %v", cnt, err)
	}

	// 5. A schema provisioned by an administrator works for a user with only SELECT and INSERT.
	provAdmin := chAs(t, adminUser, adminPW, prov, true)
	if _, err := provAdmin.InitSchema(ctx); err != nil {
		t.Fatal(err)
	}
	mustExec(t, admin, fmt.Sprintf("GRANT SELECT, INSERT ON %s.* TO %s", prov, reader))
	ins := chAs(t, reader, "rp", prov, false)
	if r, err := ins.CheckConnection(ctx); err != nil || !r.Ready() {
		t.Fatalf("SELECT/INSERT-only user on a provisioned schema: %+v %v", r, err)
	}
	if res, err := ins.InitSchema(ctx); err != nil || len(res.CreatedTables)+len(res.CreatedViews) != 0 {
		t.Fatalf("schema init on a provisioned schema must not need DDL: %+v %v", res, err)
	}
	if err := ins.Insert(ctx, model.TableProviderTurns, rowsBody(t, turn(1, u64(5), "complete")), "t1"); err != nil {
		t.Fatal(err)
	}

	// 6. Incompatible objects are reported; nothing is created next to them.
	mustExec(t, admin, "CREATE DATABASE "+bad)
	mustExec(t, admin, fmt.Sprintf("CREATE TABLE %s.messages (id UInt64) ENGINE = MergeTree ORDER BY id", bad))
	badC := chAs(t, adminUser, adminPW, bad, false)
	var se *SchemaError
	if _, err := badC.InitSchema(ctx); !errors.As(err, &se) || Classify(err) != KindSchema {
		t.Fatalf("incompatible table: %v", err)
	}
	if objs := objectsIn(t, admin, bad); len(objs) != 1 {
		t.Fatalf("nothing may be created next to incompatible objects: %v", objs)
	}

	// 7. The unrelated database was never touched.
	if after := objectsIn(t, admin, other); strings.Join(after, ",") != strings.Join(otherBefore, ",") {
		t.Fatalf("unrelated database changed: %v -> %v", otherBefore, after)
	}
	if err := admin.Query(ctx, fmt.Sprintf("SELECT count() AS n FROM %s.messages", other), &cnt); err != nil || cnt[0].N != 3 {
		t.Fatalf("unrelated data changed: %+v %v", cnt, err)
	}

	// 8. Wrong credentials are an authentication failure.
	var ce *CheckError
	if _, err := chAs(t, user, "wrong", db, false).CheckConnection(ctx); !errors.As(err, &ce) || ce.Kind != KindAuth {
		t.Fatalf("wrong password: %v", err)
	}
}
