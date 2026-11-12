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
	// Complete schema: INSERT on every table, SELECT on vario_meta and both on
	// vario_checkpoints are checked; the probe passes and covers the checkpoints.
	f.dbExists = true
	f.tables, f.columns, f.meta = completeSchema("vario")
	r, err = c.CheckConnection(bg)
	if err != nil || !r.Ready() || !r.Schema.Complete() || r.Probe != "ok" || len(r.Privileges) != len(model.Tables)+3 {
		t.Fatalf("complete schema: %+v %v", r, err)
	}
	if !strings.Contains(f.probeParams.Get("query"), CheckpointTable) {
		t.Fatalf("the insert probe must cover the checkpoint table: %v", f.probeParams)
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
	if err != nil || info.ServerID != "11111111-2222-4333-8444-555555555555" || info.Version != 3 || !info.Checkpoints || len(info.Warnings) != 0 {
		t.Fatalf("complete schema: %+v %v", info, err)
	}
	// A database migrated before delivery checkpoints existed stays usable:
	// it is reported as behind, without checkpoints, and nothing is created.
	f.tables, f.columns, f.meta = v2Schema("vario")
	info, err = c.CheckDestination(bg)
	if err != nil || info.ServerID == "" || info.Version != 2 || info.Checkpoints || len(info.Warnings) != 1 || !strings.Contains(info.Warnings[0], "vario migrate") {
		t.Fatalf("version 2 schema: %+v %v", info, err)
	}
	f.tables, f.columns, f.meta = completeSchema("vario")
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
	if n := len(p.Steps); n != len(model.Tables)+3+len(viewDefs("vario"))+1 || p.Has(StepDatabase) || kinds(p)[StepSchema] != n {
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
	// Legacy schema: only the additive version 2 and 3 steps.
	f.tables, f.columns = legacySchema("vario")
	p, _ = c.PlanSchema(bg)
	if len(p.Steps) != 4 || !strings.Contains(p.Steps[0].SQL, MetaTable) || !strings.Contains(p.Steps[1].SQL, CheckpointTable) || !strings.Contains(p.Steps[2].SQL, "destination_id") {
		t.Fatalf("legacy upgrade plan: %+v", p.Steps)
	}
	// Version 2 schema: the checkpoint table and the version, nothing else.
	// The destination id is kept, so the database stays the same destination.
	f.tables, f.columns, f.meta = v2Schema("vario")
	p, _ = c.PlanSchema(bg)
	if len(p.Steps) != 2 || !strings.HasPrefix(p.Steps[0].SQL, "CREATE TABLE IF NOT EXISTS `vario`.`"+CheckpointTable+"`") ||
		!strings.Contains(p.Steps[0].SQL, "ReplacingMergeTree(seq)") || !strings.Contains(p.Steps[1].SQL, "'schema_version', '3'") {
		t.Fatalf("version 2 upgrade plan: %+v", p.Steps)
	}
	// A table with that name that Vario did not create is never used.
	f.tables = append(f.tables, CheckpointTable+"|MergeTree||")
	if _, err := c.PlanSchema(bg); err == nil || !strings.Contains(err.Error(), CheckpointTable+" exists but was not created by Vario") {
		t.Fatalf("foreign checkpoint table: %v", err)
	}
	// Up to date: nothing.
	f.tables, f.columns, f.meta = completeSchema("vario")
	if p, err = c.PlanSchema(bg); err != nil || !p.Empty() {
		t.Fatalf("up-to-date schema: %+v %v", p, err)
	}
	// An outdated Vario view (an older tag) is replaced; a foreign one stops the plan.
	for i, tb := range f.tables {
		if strings.HasPrefix(tb, "token_usage_daily|") {
			f.tables[i] = "token_usage_daily|View|SELECT day FROM vario.provider_turns AS p FINAL|vario:000000000000"
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
	f.meta = append(f.meta, "schema_version|4")
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
	if m := f.mutations(); len(m) != 1 || !strings.Contains(m[0], "'schema_version', '3'") {
		t.Fatalf("version insert: %v", m)
	}
	// Version 3 promises the checkpoint table: it is not recorded without it.
	f.execs = nil
	f.tables, f.columns, f.meta = v2Schema("vario")
	if _, err := recordVersion(bg, admin); err == nil || len(f.mutations()) != 0 {
		t.Fatalf("the version must not be recorded without the checkpoint table: %v %v", err, f.mutations())
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

func setTable(f *fakeServer, name, row string) {
	f.tables = append(f.tables[:0:0], f.tables...)
	for i, tb := range f.tables {
		if strings.HasPrefix(tb, name+"|") {
			f.tables[i] = row
			return
		}
	}
	f.tables = append(f.tables, row)
}

// B-1: a table with Vario's column names and types but another sorting key,
// partition key or version argument does not deduplicate the way Vario relies
// on (12 messages of one conversation collapsed into one row under ORDER BY
// (source_id, conversation_id); versions of one message kept in different
// partitions are never merged). It is an incompatible schema for the
// collector, doctor and migrate alike, and is never modified.
func TestSortingKeyAndVersionAreVerified(t *testing.T) {
	const rmt = "ReplacingMergeTree(version)"
	msgKey, msgPart := sortingKeys["messages"], partitionKeys["messages"]
	cases := []struct {
		name, row, want string
	}{
		{"shorter key", tableRow("messages", rmt, "source_id, conversation_id", msgPart),
			"table messages: sorting key (ORDER BY) is source_id, conversation_id, expected source_id, conversation_id, message_id"},
		{"reordered key", tableRow("messages", rmt, "source_id, message_id, conversation_id", msgPart), "sorting key"},
		{"longer key", tableRow("messages", rmt, "source_id, conversation_id, message_id, version", msgPart), "sorting key"},
		{"single column", tableRow("sources", rmt, "host_name", ""), "table sources: sorting key (ORDER BY) is host_name, expected source_id"},
		{"no key reported", "messages|ReplacingMergeTree|||", "sorting key (ORDER BY) is empty"},
		{"no version", tableRow("messages", "ReplacingMergeTree", msgKey, msgPart), "version argument is none, expected version"},
		{"other version column", tableRow("messages", "ReplacingMergeTree(ingested_at)", msgKey, msgPart), "version argument is ingested_at, expected version"},
		{"no partition key", tableRow("messages", rmt, msgKey, ""),
			"table messages: partition key (PARTITION BY) is none, expected toYYYYMM(conversation_created_at)"},
		{"explicitly unpartitioned", tableRow("messages", rmt, msgKey, "tuple()"), "partition key (PARTITION BY) is none, expected toYYYYMM(conversation_created_at)"},
		{"mutable partition column", tableRow("messages", rmt, msgKey, "toYYYYMM(updated_at)"),
			"partition key (PARTITION BY) is toYYYYMM(updated_at), expected toYYYYMM(conversation_created_at)"},
		{"other granularity", tableRow("messages", rmt, msgKey, "toYYYYMMDD(conversation_created_at)"), "partition key (PARTITION BY) is toYYYYMMDD(conversation_created_at)"},
		{"composite partition key", tableRow("messages", rmt, msgKey, "(source_id, toYYYYMM(conversation_created_at))"), "partition key"},
		{"partitioned sources", tableRow("sources", rmt, "source_id", "source_kind"), "table sources: partition key (PARTITION BY) is source_kind, expected none"},
		{"is_deleted", tableRow("messages", "ReplacingMergeTree(version, streaming)", msgKey, msgPart), "version argument is version, streaming"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, srv := newFake(t)
			c := testClient(t, srv.URL)
			f.tables, f.columns, f.meta = completeSchema("vario")
			setTable(f, strings.SplitN(tc.row, "|", 2)[0], tc.row)

			st, err := c.InspectSchema(bg)
			if err != nil || st.Usable() || len(st.Problems) != 1 || !strings.Contains(st.Problems[0], tc.want) {
				t.Fatalf("inspect: %v %v", st.Problems, err)
			}
			var se *SchemaError
			if _, err := c.CheckDestination(bg); !errors.As(err, &se) {
				t.Fatalf("the collector must not deliver into it: %v", err)
			}
			if r, err := c.CheckConnection(bg); err != nil || r.Ready() || r.Issues[0].Kind != KindSchema || r.Probe == "ok" {
				t.Fatalf("doctor must report it: %+v %v", r, err)
			}
			if p, err := c.PlanSchema(bg); !errors.As(err, &se) || !p.Empty() {
				t.Fatalf("migrate must stop: %+v %v", p, err)
			}
			if _, err := c.VerifyMigrated(bg); !errors.As(err, &se) {
				t.Fatalf("verify: %v", err)
			}
			readOnly(t, f)
		})
	}

	// Replicated and shared engines put the path and replica first; quoting
	// and spacing of the keys do not matter, and PARTITION BY tuple() is the
	// same as no partitioning.
	f, srv := newFake(t)
	c := testClient(t, srv.URL)
	f.tables, f.columns, f.meta = completeSchema("vario")
	setTable(f, "messages", "messages|SharedReplacingMergeTree|||`source_id`,conversation_id,  message_id|"+
		"SharedReplacingMergeTree('/clickhouse/tables/{uuid}/{shard}', '{replica}', version) PARTITION BY toYYYYMM(conversation_created_at) ORDER BY (source_id, conversation_id, message_id)|"+
		"toYYYYMM( `conversation_created_at` )")
	setTable(f, "runs", "runs|ReplicatedReplacingMergeTree|||source_id, conversation_id, run_id|"+
		"ReplicatedReplacingMergeTree('/ch/it\\'s(1),x', 'r1', version) PARTITION BY toYYYYMM(conversation_created_at) ORDER BY (source_id, conversation_id, run_id)|"+
		"toYYYYMM(conversation_created_at)")
	setTable(f, "sources", tableRow("sources", rmt, "source_id", "tuple()"))
	if st, err := c.InspectSchema(bg); err != nil || !st.Complete() {
		t.Fatalf("equivalent engines must be accepted: %+v %v", st.Problems, err)
	}
}

// cpRow is a system.tables row of vario_checkpoints, owned by Vario (its
// comment), with the given engine, keys and engine_full.
func cpRow(engine, sortingKey, engineFull, partitionKey, primaryKey string) string {
	return strings.Join([]string{CheckpointTable, engine, "", checkpointTag, sortingKey, engineFull, partitionKey, primaryKey}, "|")
}

// setColumn replaces (or, with typ "", removes) a column of the fake schema.
func setColumn(f *fakeServer, table, name, typ string) {
	var out []string
	for _, c := range f.columns {
		if !strings.HasPrefix(c, table+"|"+name+"|") {
			out = append(out, c)
		}
	}
	if typ != "" {
		out = append(out, table+"|"+name+"|"+typ)
	}
	f.columns = out
}

// P2: a vario_checkpoints table that carries Vario's comment and columns but
// another engine, key, partitioning or version column does not keep the
// checkpoints Vario writes (ENGINE = Null acknowledges inserts and discards
// them), so a database restored to an earlier state would go unnoticed. It
// is an incompatible schema for the collector, doctor and migrate alike, it
// is never reported as up to date, and it is never modified.
func TestCheckpointSchemaIsVerified(t *testing.T) {
	const opts = " SETTINGS index_granularity = 8192"
	cases := []struct {
		name, row string
		col, typ  string // a changed column ("" type: missing)
		want      string
	}{
		{name: "null engine", row: cpRow("Null", "", "Null", "", ""),
			want: "table vario_checkpoints: engine is Null, expected ReplacingMergeTree"},
		{name: "memory engine", row: cpRow("Memory", "", "Memory", "", ""), want: "engine is Memory, expected ReplacingMergeTree"},
		{name: "plain MergeTree", row: cpRow("MergeTree", "writer_id", "MergeTree ORDER BY writer_id"+opts, "", "writer_id"),
			want: "engine is MergeTree, expected ReplacingMergeTree"},
		{name: "sorting key seq", row: cpRow("ReplacingMergeTree", "seq", "ReplacingMergeTree(seq) ORDER BY seq"+opts, "", "seq"),
			want: "table vario_checkpoints: sorting key (ORDER BY) is seq, expected writer_id"},
		{name: "longer sorting key", row: cpRow("ReplacingMergeTree", "writer_id, seq", "ReplacingMergeTree(seq) ORDER BY (writer_id, seq)"+opts, "", "writer_id, seq"),
			want: "sorting key (ORDER BY) is writer_id, seq, expected writer_id"},
		{name: "no sorting key", row: cpRow("ReplacingMergeTree", "", "ReplacingMergeTree(seq) ORDER BY tuple()"+opts, "", ""),
			want: "sorting key (ORDER BY) is empty, expected writer_id"},
		{name: "empty primary key", row: cpRow("ReplacingMergeTree", "writer_id", "ReplacingMergeTree(seq) PRIMARY KEY tuple() ORDER BY writer_id"+opts, "", ""),
			want: "primary key is empty, expected writer_id"},
		{name: "partitioned", row: cpRow("ReplacingMergeTree", "writer_id", "ReplacingMergeTree(seq) PARTITION BY toYYYYMM(written_at) ORDER BY writer_id"+opts, "toYYYYMM(written_at)", "writer_id"),
			want: "partition key (PARTITION BY) is toYYYYMM(written_at), expected none"},
		{name: "no version column", row: cpRow("ReplacingMergeTree", "writer_id", "ReplacingMergeTree ORDER BY writer_id"+opts, "", "writer_id"),
			want: "ReplacingMergeTree version argument is none, expected seq"},
		{name: "other version column", row: cpRow("ReplacingMergeTree", "writer_id", "ReplacingMergeTree(written_at) ORDER BY writer_id"+opts, "", "writer_id"),
			want: "ReplacingMergeTree version argument is written_at, expected seq"},
		{name: "is_deleted", row: cpRow("ReplacingMergeTree", "writer_id", "ReplacingMergeTree(seq, flag) ORDER BY writer_id"+opts, "", "writer_id"),
			want: "version argument is seq, flag, expected seq"},
		{name: "missing seq", row: cpRow("ReplacingMergeTree", "writer_id", "ReplacingMergeTree(written_at) ORDER BY writer_id"+opts, "", "writer_id"),
			col: "seq", want: "version argument is written_at, expected seq, missing column seq"},
		{name: "seq type", row: checkpointRow, col: "seq", typ: "UInt32",
			want: "table vario_checkpoints: column seq is UInt32, expected UInt64"},
		{name: "signed seq", row: checkpointRow, col: "seq", typ: "Int64", want: "column seq is Int64, expected UInt64"},
		{name: "writer_id type", row: checkpointRow, col: "writer_id", typ: "LowCardinality(String)",
			want: "column writer_id is LowCardinality(String), expected String"},
		{name: "missing writer_id", row: checkpointRow, col: "writer_id", want: "missing column writer_id"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, srv := newFake(t)
			c := testClient(t, srv.URL)
			f.tables, f.columns, f.meta = completeSchema("vario")
			setTable(f, CheckpointTable, tc.row)
			if tc.col != "" {
				setColumn(f, CheckpointTable, tc.col, tc.typ)
			}

			st, err := c.InspectSchema(bg)
			if err != nil || st.Usable() || st.Complete() || st.Checkpoints || len(st.Problems) != 1 || !strings.Contains(st.Problems[0], tc.want) {
				t.Fatalf("inspect: %q checkpoints=%v %v", st.Problems, st.Checkpoints, err)
			}
			// Collector startup: nothing is delivered (data stays spooled).
			var se *SchemaError
			if info, err := c.CheckDestination(bg); !errors.As(err, &se) || info.Checkpoints {
				t.Fatalf("the collector must not deliver: %+v %v", info, err)
			}
			// Doctor: an issue, never healthy, and no insert probe.
			r, err := c.CheckConnection(bg)
			if err != nil || r.Ready() || r.Issues[0].Kind != KindSchema || !strings.Contains(r.Issues[0].Message, tc.want) || r.Probe == "ok" {
				t.Fatalf("doctor must report it: %+v %v", r, err)
			}
			for _, w := range r.Warnings {
				if strings.Contains(w, "up to date") {
					t.Fatalf("doctor must not call it up to date: %q", w)
				}
			}
			// Migrate: stops without planning or applying anything.
			if p, err := c.PlanSchema(bg); !errors.As(err, &se) || !p.Empty() {
				t.Fatalf("migrate must stop: %+v %v", p, err)
			}
			if _, err := c.VerifyMigrated(bg); !errors.As(err, &se) {
				t.Fatalf("verify: %v", err)
			}
			readOnly(t, f)
		})
	}

	// What Vario creates is accepted, also replicated or shared, with quoting
	// and PARTITION BY tuple() (the same as no partitioning).
	for name, row := range map[string]string{
		"as created": checkpointRow,
		"replicated": cpRow("ReplicatedReplacingMergeTree", "writer_id",
			"ReplicatedReplacingMergeTree('/clickhouse/tables/{uuid}/{shard}', '{replica}', seq) ORDER BY writer_id"+opts, "", "writer_id"),
		"shared": cpRow("SharedReplacingMergeTree", "`writer_id`",
			"SharedReplacingMergeTree('/clickhouse/tables/{uuid}/{shard}', '{replica}', `seq`) ORDER BY writer_id"+opts, "tuple()", "writer_id"),
	} {
		f, srv := newFake(t)
		c := testClient(t, srv.URL)
		f.tables, f.columns, f.meta = completeSchema("vario")
		setTable(f, CheckpointTable, row)
		if st, err := c.InspectSchema(bg); err != nil || !st.Complete() || !st.Checkpoints {
			t.Fatalf("%s: a compatible checkpoint table must be accepted: %q %v", name, st.Problems, err)
		}
		if info, err := c.CheckDestination(bg); err != nil || !info.Checkpoints || len(info.Warnings) != 0 {
			t.Fatalf("%s: collector: %+v %v", name, info, err)
		}
		if r, err := c.CheckConnection(bg); err != nil || !r.Ready() || r.Probe != "ok" {
			t.Fatalf("%s: doctor: %+v %v", name, r, err)
		}
		if p, err := c.PlanSchema(bg); err != nil || !p.Empty() {
			t.Fatalf("%s: migrate: %+v %v", name, p, err)
		}
	}
}

// Upgrading a version 1 or 2 schema creates the checkpoint table; one that
// already exists with an incompatible engine stops the upgrade instead of
// being reused, and the version is not recorded even if it appears between
// planning and applying (CREATE TABLE IF NOT EXISTS keeps it).
func TestCheckpointUpgradeRefusesIncompatibleTable(t *testing.T) {
	nullRow := cpRow("Null", "", "Null", "", "")
	for name, schema := range map[string]func() (tables, columns, meta []string){
		"version 1": func() ([]string, []string, []string) { tb, cl := legacySchema("vario"); return tb, cl, nil },
		"version 2": func() ([]string, []string, []string) { return v2Schema("vario") },
	} {
		t.Run(name, func(t *testing.T) {
			f, srv := newFake(t)
			c := testClient(t, srv.URL)
			admin := &Admin{c}
			f.tables, f.columns, f.meta = schema()
			// The legitimate upgrade path plans the checkpoint table.
			p, err := c.PlanSchema(bg)
			if err != nil || !strings.Contains(p.Steps[len(p.Steps)-1].SQL, "'schema_version', '3'") {
				t.Fatalf("upgrade plan: %+v %v", p, err)
			}
			var cpStep bool
			for _, s := range p.Steps {
				cpStep = cpStep || strings.Contains(s.SQL, "CREATE TABLE IF NOT EXISTS `vario`.`"+CheckpointTable+"`")
			}
			if !cpStep {
				t.Fatalf("the upgrade must create %s: %+v", CheckpointTable, p.Steps)
			}

			// An incompatible table already there: nothing is planned.
			tables := f.tables
			f.tables = append(append([]string(nil), tables...), nullRow)
			for _, c := range expectedColumns(checkpointDDL("vario")) {
				f.columns = append(f.columns, CheckpointTable+"|"+c.name+"|"+c.typ)
			}
			var se *SchemaError
			if q, err := c.PlanSchema(bg); !errors.As(err, &se) || !q.Empty() || !strings.Contains(err.Error(), "engine is Null") {
				t.Fatalf("migrate must stop: %+v %v", q, err)
			}
			readOnly(t, f)

			// The table appears after planning: the upgrade runs, CREATE ... IF
			// NOT EXISTS leaves the table alone, and the version is not recorded.
			if f.meta == nil {
				f.meta = []string{"destination_id|11111111-2222-4333-8444-555555555555"}
			}
			res := admin.Apply(bg, p, nil)
			if res.OK() || res.Failed == nil || !strings.Contains(res.Failed.Summary, "record schema version") {
				t.Fatalf("the version must not be recorded: %+v", res)
			}
			for _, m := range f.mutations() {
				if strings.Contains(m, "schema_version") || strings.Contains(m, "DROP") || strings.Contains(m, "ALTER") ||
					strings.Contains(m, "TRUNCATE") || strings.Contains(m, "REPLACE TABLE") {
					t.Fatalf("unexpected statement: %s", m)
				}
			}
			if _, err := c.VerifyMigrated(bg); !errors.As(err, &se) {
				t.Fatalf("verify: %v", err)
			}
		})
	}
}

func TestEngineArgs(t *testing.T) {
	for full, want := range map[string]string{
		"ReplacingMergeTree(version) ORDER BY a":                            "version",
		"ReplacingMergeTree ORDER BY a":                                     "",
		"ReplacingMergeTree() ORDER BY a":                                   "",
		"ReplacingMergeTree(`version`) ORDER BY (a, b)":                     "version",
		"ReplacingMergeTree(version, is_deleted) PARTITION BY toYYYYMM(c)":  "version|is_deleted",
		"ReplacingMergeTree('/p/{shard}', '{replica}', version) ORDER BY a": "version",
		"ReplacingMergeTree('/p', 'r') ORDER BY a":                          "",
		"ReplacingMergeTree(toUInt64(version)) ORDER BY a":                  "toUInt64(version)",
		"ReplacingMergeTree(version":                                        "",
		"MergeTree ORDER BY (version)":                                      "",
	} {
		if got := strings.Join(engineArgs("ReplacingMergeTree", full), "|"); got != want {
			t.Errorf("engineArgs(%q) = %q, want %q", full, got, want)
		}
	}
}

// The queries of Vario 0.1's untagged views as ClickHouse 24.8 and 25.8
// report them in system.tables.as_select (one line, quoting dropped).
const (
	legacyTokenUsage   = "SELECT toDate(coalesce(started_at, conversation_created_at)) AS day, source_id, provider, model, usage_scope, count() AS provider_turns, countIf(p.input_tokens IS NOT NULL) AS turns_with_usage, countIf(usage_status != 'complete') AS turns_incomplete_usage, sum(p.input_tokens) AS input_tokens, sum(p.cached_input_tokens) AS cached_input_tokens, sum(p.cache_creation_input_tokens) AS cache_creation_input_tokens, sum(p.uncached_input_tokens) AS uncached_input_tokens, sum(p.output_tokens) AS output_tokens, sum(p.reasoning_tokens) AS reasoning_tokens FROM vario.provider_turns AS p FINAL GROUP BY day, source_id, provider, model, usage_scope"
	legacyTokenUsageV0 = "SELECT toDate(coalesce(started_at, conversation_created_at)) AS day, source_id, provider, model, count() AS provider_turns, countIf(p.input_tokens IS NOT NULL) AS turns_with_usage, countIf(usage_status != 'complete') AS turns_incomplete_usage, sum(p.input_tokens) AS input_tokens, sum(p.cached_input_tokens) AS cached_input_tokens, sum(p.cache_creation_input_tokens) AS cache_creation_input_tokens, sum(p.uncached_input_tokens) AS uncached_input_tokens, sum(p.output_tokens) AS output_tokens, sum(p.reasoning_tokens) AS reasoning_tokens FROM vario.provider_turns AS p FINAL GROUP BY day, source_id, provider, model"
	legacyToolUsage    = "SELECT source_id, event_type, tool_name, count() AS executions, countIf(is_error = 1) AS failures, avg(duration_ms) AS avg_duration_ms, min(started_at) AS first_seen, max(started_at) AS last_seen FROM vario.execution_events FINAL WHERE event_type IN ('command_execution', 'dynamic_tool', 'file_change', 'web_search') GROUP BY source_id, event_type, tool_name"
)

// B-2: an untagged view is Vario's own only if its query is exactly one that
// Vario 0.1 created. A user's view that merely reads from a table whose name
// contains a Vario table name (execution_events_archive) is not, and is
// never replaced.
func TestViewOwnership(t *testing.T) {
	f, srv := newFake(t)
	c := testClient(t, srv.URL)
	admin := &Admin{c}

	// Genuine Vario 0.1 views: recognized, and replaced by the tagged versions.
	f.tables, f.columns, f.meta = completeSchema("vario")
	setTable(f, "token_usage_daily", "token_usage_daily|View|"+legacyTokenUsageV0+"|")
	setTable(f, "tool_usage", "tool_usage|View|"+legacyToolUsage+"|")
	setTable(f, "messages_latest", "messages_latest|View|SELECT * FROM vario.messages FINAL|")
	p, err := c.PlanSchema(bg)
	if err != nil || len(p.Steps) != 3 || len(p.Status.OutdatedViews) != 3 {
		t.Fatalf("legacy views: %+v %v", p, err)
	}
	if res := admin.Apply(bg, p, nil); !res.OK() || len(f.mutations()) != 3 {
		t.Fatalf("apply: %+v %v", res, f.mutations())
	}
	setTable(f, "token_usage_daily", "token_usage_daily|View|"+legacyTokenUsage+"|")
	if st, err := c.InspectSchema(bg); err != nil || len(st.Problems) != 0 {
		t.Fatalf("the later 0.1 definition is Vario's as well: %+v %v", st.Problems, err)
	}

	// User-owned views under Vario's names.
	for name, query := range map[string]string{
		"archive table":       strings.ReplaceAll(legacyToolUsage, "vario.execution_events FINAL", "vario.execution_events_archive FINAL"),
		"other database":      strings.ReplaceAll(legacyToolUsage, "vario.execution_events", "analytics.execution_events"),
		"extra filter":        legacyToolUsage + " HAVING executions > 10",
		"mentions the table":  "SELECT 'execution_events' AS source, count() AS executions FROM vario.my_events",
		"reads Vario's table": "SELECT tool_name, count() AS executions FROM vario.execution_events GROUP BY tool_name",
		"no query reported":   "",
	} {
		t.Run(name, func(t *testing.T) {
			f, srv := newFake(t)
			c := testClient(t, srv.URL)
			f.tables, f.columns, f.meta = completeSchema("vario")
			setTable(f, "tool_usage", "tool_usage|View|"+query+"|")
			st, err := c.InspectSchema(bg)
			if err != nil || len(st.OutdatedViews) != 0 || len(st.Problems) != 1 || !strings.Contains(st.Problems[0], "view tool_usage exists but was not created by Vario") {
				t.Fatalf("a user-owned view must be reported as a conflict: outdated %v, problems %v, %v", st.OutdatedViews, st.Problems, err)
			}
			var se *SchemaError
			p, err := c.PlanSchema(bg)
			if !errors.As(err, &se) || !p.Empty() {
				t.Fatalf("migrate must stop without a plan: %+v %v", p, err)
			}
			if res := (&Admin{c}).Apply(bg, p, nil); !res.OK() || len(res.Applied) != 0 {
				t.Fatalf("apply: %+v", res)
			}
			readOnly(t, f)
		})
	}

	// A view tagged by a user with something that is not Vario's tag.
	f.tables, f.columns, f.meta = completeSchema("vario")
	setTable(f, "tool_usage", "tool_usage|View|"+legacyToolUsage+"|my team's view")
	if st, _ := c.InspectSchema(bg); len(st.Problems) != 1 || len(st.OutdatedViews) != 0 {
		t.Fatalf("a view with a foreign comment is not Vario's: %+v", st)
	}
}
