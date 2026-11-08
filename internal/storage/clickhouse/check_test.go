package clickhouse

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nedanwr/vario/internal/model"
)

func TestClassify(t *testing.T) {
	cases := []struct {
		err  error
		want Kind
	}{
		{&Error{Message: "Get: dial tcp 127.0.0.1:1: connect: connection refused", Transient: true}, KindNetwork},
		{&Error{Message: "Get: tls: failed to verify certificate: x509: certificate signed by unknown authority"}, KindTLS},
		{&Error{Message: "Get: http: server gave HTTP response to HTTPS client"}, KindTLS},
		{&Error{Status: 403, Code: 516, Message: "Authentication failed"}, KindAuth},
		{&Error{Status: 401, Message: "unauthorized"}, KindAuth},
		{&Error{Status: 500, Code: 497, Message: "Not enough privileges"}, KindPermission},
		{&Error{Status: 404, Code: 81, Message: "Database x does not exist"}, KindDatabaseMissing},
		{fmt.Errorf("wrapped: %w", ErrDatabaseMissing), KindDatabaseMissing},
		{&SchemaError{Database: "d", Problems: []string{"x"}}, KindSchema},
		{context.DeadlineExceeded, KindNetwork},
		{&Error{Status: 500, Code: 62, Message: "Syntax error"}, KindServer},
	}
	for _, tc := range cases {
		if got := Classify(tc.err); got != tc.want {
			t.Errorf("Classify(%v) = %s, want %s", tc.err, got, tc.want)
		}
		if Hint(Classify(tc.err)) == "" {
			t.Errorf("no hint for %s", tc.want)
		}
	}
}

func TestExpectedColumns(t *testing.T) {
	ddl := tableDDL("vario")
	cols := map[string]string{}
	for _, c := range expectedColumns(ddl[model.TableMessages]) {
		cols[c.name] = c.typ
	}
	for name, typ := range map[string]string{
		"text": "String", "role": "LowCardinality(String)", "created_at": "DateTime64(3, 'UTC')",
		"ingested_at": "DateTime64(3, 'UTC')", "version": "UInt64", "attachment_count": "UInt32",
	} {
		if cols[name] != typ {
			t.Errorf("messages.%s: got %q, want %q", name, cols[name], typ)
		}
	}
	if _, ok := cols["INDEX"]; ok {
		t.Error("index lines must not be parsed as columns")
	}
	if c := expectedColumns(ddl[model.TableProviderTurns]); len(c) < 30 {
		t.Errorf("provider_turns: only %d columns parsed", len(c))
	}
}

func TestViewDDLIsTaggedAndScoped(t *testing.T) {
	for _, v := range viewDefs("my_db") {
		create, replace := v.ddl("my_db", false), v.ddl("my_db", true)
		if !strings.HasPrefix(create, "CREATE VIEW IF NOT EXISTS `my_db`.") || !strings.HasPrefix(replace, "CREATE OR REPLACE VIEW `my_db`.") {
			t.Errorf("%s: unexpected DDL %q", v.name, create)
		}
		if !strings.Contains(create, "COMMENT 'vario:") || viewTag(v.name) == "" {
			t.Errorf("%s: view must carry the Vario tag", v.name)
		}
		if strings.Contains(v.query, "`vario`") {
			t.Errorf("%s: view must only reference the configured database", v.name)
		}
	}
	if viewTag("token_usage_daily") == viewTag("tool_usage") {
		t.Error("tags must identify the definition")
	}
}

// fakeServer answers the queries the connection check issues.
type fakeServer struct {
	dbExists bool
	tables   []string // existing objects: "name|engine|as_select|comment"
	columns  []string // "table|name|type"
	denied   map[string]bool
	noCheck  bool
	queries  []string
}

func (f *fakeServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/ping" {
		io.WriteString(w, "Ok.\n")
		return
	}
	if r.Header.Get("X-ClickHouse-Key") != "pw-123" {
		w.Header().Set("X-ClickHouse-Exception-Code", "516")
		w.WriteHeader(403)
		io.WriteString(w, "Code: 516. DB::Exception: Authentication failed")
		return
	}
	b, _ := io.ReadAll(r.Body)
	q := string(b)
	f.queries = append(f.queries, q)
	var rows []string
	switch {
	case strings.Contains(q, "version()"):
		rows = []string{`{"v":"25.8.1"}`}
	case strings.Contains(q, "system.databases"):
		n := 0
		if f.dbExists {
			n = 1
		}
		rows = []string{fmt.Sprintf(`{"n":%d}`, n)}
	case strings.Contains(q, "system.tables"):
		for _, t := range f.tables {
			p := strings.Split(t, "|")
			rows = append(rows, fmt.Sprintf(`{"name":%q,"engine":%q,"as_select":%q,"comment":%q}`, p[0], p[1], p[2], p[3]))
		}
	case strings.Contains(q, "system.columns"):
		for _, c := range f.columns {
			p := strings.Split(c, "|")
			rows = append(rows, fmt.Sprintf(`{"table":%q,"name":%q,"type":%q}`, p[0], p[1], p[2]))
		}
	case strings.HasPrefix(q, "CHECK GRANT"):
		if f.noCheck {
			w.Header().Set("X-ClickHouse-Exception-Code", "62")
			w.WriteHeader(400)
			io.WriteString(w, "Code: 62. Syntax error")
			return
		}
		res := 1
		if f.denied[q] {
			res = 0
		}
		rows = []string{fmt.Sprintf(`{"result":%d}`, res)}
	default:
		w.Header().Set("X-ClickHouse-Exception-Code", "497")
		w.WriteHeader(500)
		io.WriteString(w, "Code: 497. unexpected statement in a read-only check")
		return
	}
	fmt.Fprintf(w, `{"data":[%s]}`, strings.Join(rows, ","))
}

// completeSchema returns system.tables and system.columns rows for an up-to-date Vario schema.
func completeSchema(db string) (tables, columns []string) {
	ddl := tableDDL(db)
	for _, name := range model.Tables {
		tables = append(tables, name+"|ReplacingMergeTree||")
		for _, c := range expectedColumns(ddl[name]) {
			columns = append(columns, name+"|"+c.name+"|"+c.typ)
		}
	}
	for _, v := range viewDefs(db) {
		tables = append(tables, v.name+"|View|"+v.query+"|"+viewTag(v.name))
	}
	return
}

func TestCheckConnection(t *testing.T) {
	ctx := context.Background()
	f := &fakeServer{dbExists: true, denied: map[string]bool{}}
	srv := httptest.NewServer(f)
	defer srv.Close()
	c := testClient(t, srv.URL)

	// Existing, empty database with all privileges.
	r, err := c.CheckConnection(ctx)
	if err != nil || !r.Ready() || r.Version != "25.8.1" || len(r.Schema.MissingTables) != len(model.Tables) {
		t.Fatalf("empty database: %+v %v", r, err)
	}
	// Missing database, creation disabled: reported, and CREATE DATABASE is not required.
	f.dbExists = false
	r, _ = c.CheckConnection(ctx)
	if r.Ready() || r.Issues[0].Kind != KindDatabaseMissing {
		t.Fatalf("missing database: %+v", r.Issues)
	}
	for _, p := range r.Privileges {
		if strings.HasPrefix(p.Grant, "CREATE DATABASE") {
			t.Fatal("CREATE DATABASE must not be required when create_database = false")
		}
	}
	// Creation enabled but not permitted: a permission issue.
	c.createDatabase = true
	f.denied["CHECK GRANT CREATE DATABASE ON `vario`.*"] = true
	r, _ = c.CheckConnection(ctx)
	if r.Ready() || r.Issues[0].Kind != KindPermission || !strings.Contains(r.Issues[0].Message, "CREATE DATABASE") {
		t.Fatalf("missing CREATE DATABASE: %+v", r.Issues)
	}
	c.createDatabase = false

	// Complete schema: only SELECT and INSERT are needed.
	f.dbExists = true
	f.tables, f.columns = completeSchema("vario")
	f.denied = map[string]bool{"CHECK GRANT CREATE TABLE ON `vario`.*": true, "CHECK GRANT CREATE VIEW ON `vario`.*": true}
	r, _ = c.CheckConnection(ctx)
	if !r.Ready() || !r.Schema.Complete() || len(r.Privileges) != 2 {
		t.Fatalf("complete schema with SELECT/INSERT only: %+v", r)
	}
	// Incompatible table.
	f.columns = append(f.columns[:0:0], f.columns...)
	for i, col := range f.columns {
		if col == "messages|text|String" {
			f.columns[i] = "messages|text|UInt8"
		}
	}
	r, _ = c.CheckConnection(ctx)
	if r.Ready() || r.Issues[0].Kind != KindSchema || !strings.Contains(r.Issues[0].Message, "column text is UInt8") {
		t.Fatalf("incompatible column: %+v", r.Issues)
	}
	// A foreign view with a Vario name, and a table where a view belongs.
	f.tables, f.columns = completeSchema("vario")
	for i, tb := range f.tables {
		if strings.HasPrefix(tb, "tool_usage|") {
			f.tables[i] = "tool_usage|View|SELECT 1|"
		}
		if strings.HasPrefix(tb, "messages_latest|") {
			f.tables[i] = "messages_latest|MergeTree||"
		}
	}
	r, _ = c.CheckConnection(ctx)
	if len(r.Schema.Problems) != 2 {
		t.Fatalf("foreign objects: %+v", r.Schema.Problems)
	}
	// Legacy (untagged) Vario views are outdated, not foreign; updating them wants DROP VIEW.
	f.tables, _ = completeSchema("vario")
	for i, tb := range f.tables {
		if strings.HasPrefix(tb, "token_usage_daily|") {
			f.tables[i] = "token_usage_daily|View|SELECT day FROM vario.provider_turns AS p FINAL|"
		}
	}
	f.denied = map[string]bool{"CHECK GRANT DROP VIEW ON `vario`.*": true}
	r, _ = c.CheckConnection(ctx)
	if !r.Ready() || len(r.Schema.OutdatedViews) != 1 {
		t.Fatalf("outdated view: %+v", r)
	}
	// Older servers without CHECK GRANT: privileges unknown, not failed.
	f.noCheck = true
	r, _ = c.CheckConnection(ctx)
	if r.PrivilegesUnknown == "" || !r.Ready() {
		t.Fatalf("no CHECK GRANT: %+v", r)
	}
	// The check never modifies anything.
	for _, q := range f.queries {
		if !strings.HasPrefix(q, "SELECT") && !strings.HasPrefix(q, "CHECK GRANT") {
			t.Fatalf("connection check issued %q", q)
		}
	}

	// Authentication and network failures are classified.
	bad := testClient(t, srv.URL)
	bad.password = "wrong"
	var ce *CheckError
	if _, err := bad.CheckConnection(ctx); !errors.As(err, &ce) || ce.Kind != KindAuth || ce.Stage != "authenticate" {
		t.Fatalf("auth: %v", err)
	}
	srv.Close()
	if _, err := c.CheckConnection(ctx); !errors.As(err, &ce) || ce.Kind != KindNetwork || ce.Stage != "reach" {
		t.Fatalf("network: %v", err)
	}
}

func TestInitSchemaRespectsDatabasePolicy(t *testing.T) {
	f := &fakeServer{dbExists: false}
	srv := httptest.NewServer(f)
	defer srv.Close()
	c := testClient(t, srv.URL)
	if _, err := c.InitSchema(context.Background()); !errors.Is(err, ErrDatabaseMissing) {
		t.Fatalf("expected ErrDatabaseMissing, got %v", err)
	}
	for _, q := range f.queries {
		if strings.HasPrefix(q, "CREATE") {
			t.Fatalf("nothing may be created when the database is missing and creation is disabled: %q", q)
		}
	}
	// Incompatible objects: nothing is created.
	f.dbExists = true
	f.tables = []string{"messages|MergeTree||"}
	f.queries = nil
	var se *SchemaError
	if _, err := c.InitSchema(context.Background()); !errors.As(err, &se) {
		t.Fatalf("expected SchemaError, got %v", err)
	}
	for _, q := range f.queries {
		if strings.HasPrefix(q, "CREATE") {
			t.Fatalf("nothing may be created next to incompatible objects: %q", q)
		}
	}
	// An up-to-date schema issues no DDL at all.
	f.tables, f.columns = completeSchema("vario")
	f.queries = nil
	res, err := c.InitSchema(context.Background())
	if err != nil || len(res.CreatedTables)+len(res.CreatedViews)+len(res.UpdatedViews) != 0 {
		t.Fatalf("up-to-date schema: %+v %v", res, err)
	}
	for _, q := range f.queries {
		if !strings.HasPrefix(q, "SELECT") {
			t.Fatalf("unexpected statement for an up-to-date schema: %q", q)
		}
	}
}
