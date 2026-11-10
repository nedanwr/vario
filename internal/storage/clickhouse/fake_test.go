package clickhouse

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
)

// fakeServer is a small ClickHouse HTTP endpoint for unit tests. It answers
// the inspection, discovery and grant queries Vario issues from the fields
// below, and records every statement. Statements that change something are
// recorded in execs and succeed unless listed in fail.
type fakeServer struct {
	mu       sync.Mutex
	password string

	dbExists bool
	dbs      []string
	tables   []string // existing objects: "name|engine|as_select|comment"
	columns  []string // "table|name|type"
	meta     []string // vario_meta rows: "key|value"

	denied       map[string]bool // CHECK GRANT statements that return 0
	noCheck      bool            // CHECK GRANT unsupported (old server)
	readonly     int
	noReadonly   bool            // the readonly setting cannot be read
	strictRO     bool            // readonly = 1 refuses every query setting (ClickHouse 24.8)
	insertDenied map[string]bool // tables whose insert probe is rejected
	insertCode   int             // error code for every insert probe (0: accepted)
	probeParams  url.Values      // URL parameters of the last insert probe

	users      []string // "name|storage"
	roleGrants []string // "grantee|granted_role|is_default"
	grants     []string // "user|role|access|database|table|partial|grant_option" (empty = NULL)
	noSystem   map[string]bool

	fail    map[string]int // statement prefix -> error code
	queries []string
	execs   []string
}

func newFake(t *testing.T) (*fakeServer, *httptest.Server) {
	f := &fakeServer{password: "pw-123", dbExists: true, denied: map[string]bool{}, insertDenied: map[string]bool{},
		noSystem: map[string]bool{}, fail: map[string]int{}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return f, srv
}

var (
	nameEqRe   = regexp.MustCompile(`name = '([^']*)'`)
	userEqRe   = regexp.MustCompile(`user_name = '([^']*)'`)
	roleEqRe   = regexp.MustCompile(`role_name = '([^']*)'`)
	insertToRe = regexp.MustCompile("^INSERT INTO `[^`]*`\\.`([^`]*)` FORMAT JSONEachRow$")
)

func (f *fakeServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/ping" {
		io.WriteString(w, "Ok.\n")
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("X-ClickHouse-Key") != f.password {
		w.Header().Set("X-ClickHouse-Exception-Code", "516")
		w.WriteHeader(403)
		io.WriteString(w, "Code: 516. DB::Exception: Authentication failed")
		return
	}
	b, _ := io.ReadAll(r.Body)
	q := string(b)
	if qp := r.URL.Query().Get("query"); qp != "" {
		q = qp
	}
	f.queries = append(f.queries, q)
	fail := func(code int, msg string) {
		w.Header().Set("X-ClickHouse-Exception-Code", fmt.Sprint(code))
		w.WriteHeader(500)
		fmt.Fprintf(w, "Code: %d. DB::Exception: %s", code, msg)
	}
	if f.strictRO && r.URL.Query().Get("output_format_json_quote_64bit_integers") != "" {
		fail(164, "Cannot modify 'output_format_json_quote_64bit_integers' setting in readonly mode. (READONLY)")
		return
	}
	var rows []string
	switch {
	case strings.Contains(q, "version()"):
		rows = []string{`{"v":"25.8.1"}`}
	case strings.Contains(q, "getSetting('readonly')") && f.noReadonly:
		fail(62, "Syntax error")
		return
	case strings.Contains(q, "getSetting('readonly')"):
		rows = []string{fmt.Sprintf(`{"r":"%d"}`, f.readonly)}
	case strings.Contains(q, "system.databases WHERE"):
		n := 0
		if f.dbExists {
			n = 1
		}
		rows = []string{fmt.Sprintf(`{"n":%d}`, n)}
	case strings.Contains(q, "system.databases"):
		for _, d := range f.dbs {
			rows = append(rows, fmt.Sprintf(`{"name":%q}`, d))
		}
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
	case strings.HasPrefix(q, "SELECT key, value FROM"):
		for _, m := range f.meta {
			k, v, _ := strings.Cut(m, "|")
			rows = append(rows, fmt.Sprintf(`{"key":%q,"value":%q}`, k, v))
		}
	case strings.Contains(q, "system.users"):
		if f.noSystem["users"] {
			fail(497, "Not enough privileges. To execute this query, it's necessary to have the grant SHOW USERS ON *.*")
			return
		}
		want := ""
		if m := nameEqRe.FindStringSubmatch(q); m != nil {
			want = m[1]
		}
		for _, u := range f.users {
			n, st, _ := strings.Cut(u, "|")
			if want == "" || n == want {
				rows = append(rows, fmt.Sprintf(`{"name":%q,"storage":%q}`, n, st))
			}
		}
	case strings.Contains(q, "system.role_grants"):
		if f.noSystem["role_grants"] {
			fail(497, "Not enough privileges")
			return
		}
		grantee := ""
		if m := userEqRe.FindStringSubmatch(q); m != nil {
			grantee = m[1]
		} else if m := roleEqRe.FindStringSubmatch(q); m != nil {
			grantee = m[1]
		}
		for _, g := range f.roleGrants {
			p := strings.Split(g, "|")
			if p[0] == grantee {
				rows = append(rows, fmt.Sprintf(`{"granted_role_name":%q,"granted_role_is_default":%s}`, p[1], p[2]))
			}
		}
	case strings.Contains(q, "system.grants"):
		if f.noSystem["grants"] {
			fail(497, "Not enough privileges")
			return
		}
		for _, g := range f.grants {
			p := strings.Split(g, "|")
			match := (p[0] != "" && strings.Contains(q, "user_name = '"+p[0]+"'")) || (p[1] != "" && strings.Contains(q, "role_name = '"+p[1]+"'"))
			if !match {
				continue
			}
			null := func(s string) string {
				if s == "" {
					return "null"
				}
				return fmt.Sprintf("%q", s)
			}
			rows = append(rows, fmt.Sprintf(`{"user_name":%s,"role_name":%s,"access_type":%q,"database":%s,"table":%s,"column":null,"is_partial_revoke":%s,"grant_option":%s}`,
				null(p[0]), null(p[1]), p[2], null(p[3]), null(p[4]), p[5], p[6]))
		}
	case strings.HasPrefix(q, "CHECK GRANT"):
		if f.noCheck {
			fail(62, "Syntax error")
			return
		}
		res := 1
		if f.denied[q] {
			res = 0
		}
		rows = []string{fmt.Sprintf(`{"result":%d}`, res)}
	case insertToRe.MatchString(q) && len(b) == 0:
		table := insertToRe.FindStringSubmatch(q)[1]
		f.probeParams = r.URL.Query()
		if f.insertCode != 0 {
			fail(f.insertCode, "simulated restriction")
			return
		}
		if f.insertDenied[table] {
			fail(497, "Not enough privileges. To execute this query, it's necessary to have the grant INSERT ON db."+table)
			return
		}
		return
	default:
		for prefix, code := range f.fail {
			if strings.HasPrefix(q, prefix) {
				fail(code, "simulated failure")
				return
			}
		}
		f.execs = append(f.execs, q)
		return
	}
	fmt.Fprintf(w, `{"data":[%s]}`, strings.Join(rows, ","))
}

// completeSchema returns system.tables and system.columns rows for an
// up-to-date Vario schema (version 2) and its vario_meta rows.
func completeSchema(db string) (tables, columns, meta []string) {
	tables, columns = legacySchema(db)
	tables = append(tables, MetaTable+"|MergeTree||"+metaTag)
	for _, c := range expectedColumns(metaDDL(db)) {
		columns = append(columns, MetaTable+"|"+c.name+"|"+c.typ)
	}
	return tables, columns, []string{"destination_id|11111111-2222-4333-8444-555555555555", "schema_version|2"}
}

// legacySchema returns the objects Vario 0.1 created (schema version 1).
func legacySchema(db string) (tables, columns []string) {
	ddl := tableDDL(db)
	for _, name := range requiredTables() {
		tables = append(tables, name+"|ReplacingMergeTree||")
		for _, c := range expectedColumns(ddl[name]) {
			columns = append(columns, name+"|"+c.name+"|"+c.typ)
		}
	}
	for _, v := range viewDefs(db) {
		tables = append(tables, v.name+"|View|"+v.query+"|"+viewTag(v.name))
	}
	return tables, columns
}

// mutations returns the recorded statements that change ClickHouse.
func (f *fakeServer) mutations() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.execs...)
}
