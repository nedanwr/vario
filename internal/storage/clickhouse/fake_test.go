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

	"github.com/nedanwr/vario/internal/model"
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
	tables   []string // existing objects: "name|engine|as_select|comment[|sorting_key|engine_full[|partition_key[|primary_key]]]"
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

	checkpoints  map[string]uint64 // vario_checkpoints: writer -> highest seq
	cpParams     []url.Values      // URL parameters of every checkpoint read and write
	cpBodies     []string          // bodies of checkpoint inserts
	cpSettingErr int               // error code for a checkpoint read that changes a setting
	cpReadErr    int               // error code for every checkpoint read
	cpWriteErr   int               // error code for every checkpoint insert

	fail    map[string]int // statement prefix -> error code
	queries []string
	execs   []string
}

func newFake(t *testing.T) (*fakeServer, *httptest.Server) {
	f := &fakeServer{password: "pw-123", dbExists: true, denied: map[string]bool{}, insertDenied: map[string]bool{},
		noSystem: map[string]bool{}, fail: map[string]int{}, checkpoints: map[string]uint64{}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return f, srv
}

var (
	nameEqRe   = regexp.MustCompile(`name = '([^']*)'`)
	userEqRe   = regexp.MustCompile(`user_name = '([^']*)'`)
	roleEqRe   = regexp.MustCompile(`role_name = '([^']*)'`)
	insertToRe = regexp.MustCompile("^INSERT INTO `[^`]*`\\.`([^`]*)` FORMAT JSONEachRow$")
	cpReadRe   = regexp.MustCompile("^SELECT max\\(seq\\) AS seq, count\\(\\) AS n FROM `[^`]*`\\.`vario_checkpoints` WHERE writer_id = '((?:[^'\\\\]|\\\\.)*)'$")
	cpRowRe    = regexp.MustCompile(`^\{"writer_id":"([^"]*)","seq":(\d+)\}\n$`)
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
			p := append(strings.Split(t, "|"), "", "", "", "")
			if len(strings.Split(t, "|")) < 8 {
				// ClickHouse reports the sorting key as primary key unless
				// PRIMARY KEY is given.
				p[7] = p[4]
			}
			rows = append(rows, fmt.Sprintf(`{"name":%q,"engine":%q,"as_select":%q,"comment":%q,"sorting_key":%q,"engine_full":%q,"partition_key":%q,"primary_key":%q}`, p[0], p[1], p[2], p[3], p[4], p[5], p[6], p[7]))
		}
	case strings.Contains(q, "system.columns"):
		for _, c := range f.columns {
			p := strings.Split(c, "|")
			rows = append(rows, fmt.Sprintf(`{"table":%q,"name":%q,"type":%q}`, p[0], p[1], p[2]))
		}
	case cpReadRe.MatchString(q):
		f.cpParams = append(f.cpParams, r.URL.Query())
		if f.cpSettingErr != 0 && r.URL.Query().Get("select_sequential_consistency") != "" {
			fail(f.cpSettingErr, "Setting select_sequential_consistency should not be changed")
			return
		}
		if f.cpReadErr != 0 {
			fail(f.cpReadErr, "simulated failure")
			return
		}
		seq, ok := f.checkpoints[cpReadRe.FindStringSubmatch(q)[1]]
		n := 0
		if ok {
			n = 1
		}
		rows = []string{fmt.Sprintf(`{"seq":%d,"n":%d}`, seq, n)}
	case insertToRe.MatchString(q) && len(b) > 0 && insertToRe.FindStringSubmatch(q)[1] == CheckpointTable:
		f.cpParams = append(f.cpParams, r.URL.Query())
		f.cpBodies = append(f.cpBodies, string(b))
		if f.cpWriteErr != 0 {
			fail(f.cpWriteErr, "simulated failure")
			return
		}
		m := cpRowRe.FindStringSubmatch(string(b))
		if m == nil {
			fail(27, "Cannot parse input")
			return
		}
		var seq uint64
		fmt.Sscan(m[2], &seq)
		if seq > f.checkpoints[m[1]] {
			f.checkpoints[m[1]] = seq
		}
		return
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
// up-to-date Vario schema (version 3) and its vario_meta rows.
func completeSchema(db string) (tables, columns, meta []string) {
	tables, columns, _ = v2Schema(db)
	tables = append(tables, checkpointRow)
	for _, c := range expectedColumns(checkpointDDL(db)) {
		columns = append(columns, CheckpointTable+"|"+c.name+"|"+c.typ)
	}
	return tables, columns, []string{"destination_id|11111111-2222-4333-8444-555555555555", "schema_version|3"}
}

// checkpointRow is the system.tables row of vario_checkpoints as Vario
// creates it, in the format ClickHouse reports.
const checkpointRow = CheckpointTable + "|ReplacingMergeTree||" + checkpointTag + "|writer_id|ReplacingMergeTree(seq) ORDER BY writer_id SETTINGS index_granularity = 8192||writer_id"

// v2Schema returns a schema migrated by a Vario version before delivery
// checkpoints: vario_meta with a destination id, no vario_checkpoints.
func v2Schema(db string) (tables, columns, meta []string) {
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
		tables = append(tables, tableRow(name, "ReplacingMergeTree(version)", sortingKeys[name], partitionKeys[name]))
		for _, c := range expectedColumns(ddl[name]) {
			columns = append(columns, name+"|"+c.name+"|"+c.typ)
		}
	}
	for _, v := range viewDefs(db) {
		tables = append(tables, v.name+"|View|"+v.query+"|"+viewTag(v.name))
	}
	return tables, columns
}

// sortingKeys are the sorting keys of Vario's tables as ClickHouse reports
// them in system.tables.sorting_key. They are written out here, not derived
// from the DDL, so that a changed ORDER BY is noticed.
var sortingKeys = map[string]string{
	model.TableSources:         "source_id",
	model.TableProjects:        "source_id, project_id",
	model.TableConversations:   "source_id, conversation_id",
	model.TableRuns:            "source_id, conversation_id, run_id",
	model.TableMessages:        "source_id, conversation_id, message_id",
	model.TableExecutionEvents: "source_id, conversation_id, event_id",
	model.TableProviderTurns:   "source_id, conversation_id, provider_turn_id",
	model.TableRawEvents:       "source_id, sequence, event_id",
}

// partitionKeys are the partition keys of Vario's tables as ClickHouse
// reports them in system.tables.partition_key ("" for an unpartitioned
// table). Like sortingKeys, they are written out so that a changed PARTITION
// BY is noticed.
var partitionKeys = map[string]string{
	model.TableSources:         "",
	model.TableProjects:        "",
	model.TableConversations:   "toYYYYMM(created_at)",
	model.TableRuns:            "toYYYYMM(conversation_created_at)",
	model.TableMessages:        "toYYYYMM(conversation_created_at)",
	model.TableExecutionEvents: "toYYYYMM(conversation_created_at)",
	model.TableProviderTurns:   "toYYYYMM(conversation_created_at)",
	model.TableRawEvents:       "toYYYYMM(occurred_at)",
}

// tableRow is the system.tables row of a ReplacingMergeTree table with the
// given engine clause, sorting key and partition key, in the format
// ClickHouse reports.
func tableRow(name, engine, sortingKey, partitionKey string) string {
	order := sortingKey
	if strings.Contains(order, ",") {
		order = "(" + order + ")"
	}
	full := engine
	if partitionKey != "" {
		full += " PARTITION BY " + partitionKey
	}
	return name + "|ReplacingMergeTree||" + "|" + sortingKey + "|" + full + " ORDER BY " + order + " SETTINGS non_replicated_deduplication_window = 1000, index_granularity = 8192|" + partitionKey
}

// mutations returns the recorded statements that change ClickHouse.
func (f *fakeServer) mutations() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.execs...)
}
