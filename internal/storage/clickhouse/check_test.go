package clickhouse

import (
	"context"
	"fmt"
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

// The check distinguishes confirmed access, confirmed failures and checks
// that could not be performed conclusively.
func TestCheckConnectionVerdicts(t *testing.T) {
	f, srv := newFake(t)
	c := testClient(t, srv.URL)
	f.tables, f.columns, f.meta = completeSchema("vario")

	r, err := c.CheckConnection(bg)
	if err != nil || !r.Verified() || len(r.Unverified) != 0 {
		t.Fatalf("complete access must be verified: %+v %v", r, err)
	}
	// The probe uses exactly the settings of a delivery, so a profile or
	// constraint forbidding one of them fails the check as it would fail delivery.
	want := c.insertParams("x", "t")
	for k := range want {
		if k != "query" && k != "insert_deduplication_token" && f.probeParams.Get(k) != want.Get(k) {
			t.Errorf("probe setting %s = %q, delivery uses %q", k, f.probeParams.Get(k), want.Get(k))
		}
	}
	if f.probeParams.Get("insert_deduplication_token") == "" {
		t.Error("the probe must set the deduplication token like a delivery")
	}

	// Read-only profile, exhausted quota and settings constraints: confirmed
	// failures, classified as session restrictions rather than missing grants.
	f.readonly = 1
	r, _ = c.CheckConnection(bg)
	if r.Ready() || r.Issues[0].Kind != KindRestriction || !strings.HasPrefix(r.Probe, "not run") {
		t.Fatalf("readonly: %+v", r)
	}
	f.readonly = 0
	// ClickHouse 24.8: a read-only session refuses the settings of every query.
	f.strictRO = true
	r, err = c.CheckConnection(bg)
	if err != nil || r.Ready() || r.ReadOnly != 1 || r.Issues[0].Kind != KindRestriction || len(r.Unverified) == 0 {
		t.Fatalf("strict readonly: %+v %v", r, err)
	}
	f.strictRO = false
	for _, code := range []int{164, 201, 452} {
		f.insertCode = code
		r, _ = c.CheckConnection(bg)
		if r.Ready() || r.Issues[0].Kind != KindRestriction || !strings.Contains(Hint(r.Issues[0].Kind), "settings profile") {
			t.Fatalf("code %d: %+v", code, r.Issues)
		}
	}
	f.insertCode = 0
	// Grants look complete but the server rejects the insert: the probe decides.
	f.insertDenied["messages"] = true
	if r, _ = c.CheckConnection(bg); r.Ready() || r.Issues[0].Kind != KindPermission || !strings.Contains(r.Probe, "messages") {
		t.Fatalf("rejected insert despite grants: %+v", r)
	}
	f.insertDenied = map[string]bool{}

	// The readonly setting cannot be read: inconclusive, not failed.
	f.noReadonly = true
	r, _ = c.CheckConnection(bg)
	if !r.Ready() || r.Verified() || !strings.Contains(strings.Join(r.Unverified, " "), "readonly setting") {
		t.Fatalf("unreadable readonly setting: %+v", r)
	}
	f.noReadonly = false
	// Without CHECK GRANT (before 24.5) the probe still confirms inserts.
	f.noCheck = true
	if r, _ = c.CheckConnection(bg); !r.Verified() || r.PrivilegesUnknown == "" || r.Probe != "ok" {
		t.Fatalf("no CHECK GRANT: %+v", r)
	}
	f.noCheck = false
	// No schema: inserts cannot be tested.
	f.tables, f.columns, f.meta = nil, nil, nil
	if r, _ = c.CheckConnection(bg); r.Ready() || !strings.Contains(strings.Join(r.Unverified, " "), "inserts were not tested") {
		t.Fatalf("no schema: %+v", r)
	}
	readOnly(t, f)
}
