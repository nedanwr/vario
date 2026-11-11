package cli

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/nedanwr/vario/internal/config"
	"github.com/nedanwr/vario/internal/storage/clickhouse"
)

// The commands themselves against a real ClickHouse server (set
// VARIO_TEST_CLICKHOUSE_URL, see the Makefile): migrate, deliver, BACKUP
// DATABASE, deliver more, DROP and RESTORE DATABASE. Every collector command
// must refuse the restored database, status and doctor must report it, and
// `vario destination switch` followed by a normal run must leave the database
// with the complete history, each record counted once by the views.
func TestIntegrationCLIRollbackIsReportedAndRecovered(t *testing.T) {
	url := os.Getenv("VARIO_TEST_CLICKHOUSE_URL")
	if url == "" {
		t.Skip("set VARIO_TEST_CLICKHOUSE_URL to run ClickHouse integration tests")
	}
	user, pw := os.Getenv("VARIO_TEST_CLICKHOUSE_USER"), os.Getenv("VARIO_TEST_CLICKHOUSE_PASSWORD")
	if user == "" {
		user = "default"
	}
	db := fmt.Sprintf("vario_cli_rb_%d", time.Now().UnixNano())
	fix, _ := initFixture(t)
	for _, kv := range [][2]string{{"clickhouse.url", url}, {"clickhouse.username", user}, {"clickhouse.database", db}} {
		if r := vario(t, "", "config", "set", kv[0], kv[1]); r.code != 0 {
			t.Fatal(r)
		}
	}
	if r := vario(t, pw+"\n", "config", "set-password", "--stdin"); r.code != 0 {
		t.Fatal(r)
	}
	cc := config.Default().ClickHouse
	cc.URL, cc.Username, cc.Database = url, user, db
	ch, err := clickhouse.New(cc, pw)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	t.Cleanup(func() { _ = ch.Exec(ctx, "DROP DATABASE IF EXISTS `"+db+"` SYNC") })
	exec := func(q string) {
		t.Helper()
		if err := ch.Exec(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	type totals struct {
		Msgs  int    `json:"msgs"`
		Turns int    `json:"turns"`
		Input uint64 `json:"input"`
		Raw   int    `json:"raw"`
	}
	read := func() totals {
		t.Helper()
		var out []totals
		q := fmt.Sprintf(`SELECT (SELECT count() FROM %[1]s.messages_latest) AS msgs, (SELECT count() FROM %[1]s.provider_turns_latest) AS turns,
			(SELECT sum(input_tokens) FROM %[1]s.token_usage_daily) AS input, (SELECT count() FROM %[1]s.raw_events_latest) AS raw`, "`"+db+"`")
		if err := ch.Query(ctx, q, &out); err != nil {
			t.Fatal(err)
		}
		return out[0]
	}
	backfill := func(what string) result {
		t.Helper()
		r := vario(t, "", "backfill", "--source", "t3code")
		if r.code != 0 {
			t.Fatalf("%s: %+v", what, r)
		}
		return r
	}

	if r := vario(t, pw, "migrate", "--non-interactive", "--yes", "--admin-user", user, "--admin-password-stdin"); r.code != 0 ||
		!strings.Contains(r.stdout, fmt.Sprintf("✓ schema verified: version %d", clickhouse.SchemaVersion)) {
		t.Fatalf("migrate: %+v", r)
	}
	backfill("initial delivery")
	atBackup := read()
	if atBackup.Msgs != 2 || atBackup.Turns != 1 {
		t.Fatalf("initial delivery: %+v", atBackup)
	}
	if r := vario(t, "", "doctor"); r.code != 0 || !strings.Contains(r.stdout, "is held by the database: no rollback detected") {
		t.Fatalf("doctor before: %+v", r)
	}
	name := db + "_backup"
	if err := ch.Exec(ctx, fmt.Sprintf("BACKUP DATABASE `%s` TO File('%s')", db, name)); err != nil {
		t.Skipf("the test server does not allow BACKUP ... TO File: %v", err)
	}

	fix.Message("msg-after-backup", "thread-1", "run-1", "assistant", "delivered after the backup", false)
	fix.ProviderTurn("turn-2", "thread-1", "run-1:attempt:1", "completed", nil,
		map[string]any{"usageScope": "main_agent", "usageStatus": "complete", "inputTokens": 1000, "outputTokens": 10}, nil)
	backfill("delivery after the backup")
	want := read()
	if want.Msgs != 3 || want.Turns != 2 || want.Input != atBackup.Input+1000 {
		t.Fatalf("delivery after the backup: %+v", want)
	}
	confirmed := acknowledged(t)

	exec("DROP DATABASE `" + db + "` SYNC")
	exec(fmt.Sprintf("RESTORE DATABASE `%s` FROM File('%s')", db, name))
	if got := read(); got != atBackup {
		t.Fatalf("the restored database holds the backup: %+v, want %+v", got, atBackup)
	}

	// Found by doctor before any collector ran, then refused by the collector.
	r := vario(t, "", "doctor")
	if r.code != ExitFailure {
		t.Fatalf("doctor must fail: %+v", r)
	}
	wantAll(t, "doctor", r.stdout, "✗ delivery checkpoint: the database acknowledged checkpoint "+confirmed, "restored to an earlier state", "vario destination switch")
	for _, args := range [][]string{{"backfill", "--source", "t3code"}, {"run"}} {
		r := vario(t, "", args...)
		if r.code != ExitUsage {
			t.Fatalf("%v must refuse the restored database: %+v", args, r)
		}
		wantAll(t, strings.Join(args, " "), r.stderr, "was restored to an earlier state", "acknowledged delivery checkpoint "+confirmed, "vario destination switch")
	}
	if got := read(); got != atBackup {
		t.Fatalf("rows were delivered into the restored database: %+v", got)
	}
	wantAll(t, "status", vario(t, "", "status").stdout, "! DATABASE RESTORED TO AN EARLIER STATE", "acknowledged delivery checkpoint "+confirmed)

	// The explicit decision, then the complete reconciliation.
	r = vario(t, "", "destination", "switch")
	if r.code != 0 {
		t.Fatalf("switch: %+v", r)
	}
	wantAll(t, "switch", r.stdout, "✓ accepted the restored database", "reads every source again in full")
	if got := read(); got != atBackup {
		t.Fatalf("the switch itself must not deliver: %+v", got)
	}
	backfill("reconciliation")
	if got := read(); got != want {
		t.Fatalf("after the reconciliation the database must hold the complete history once:\n got %+v\nwant %+v", got, want)
	}
	backfill("repeated reconciliation")
	if r := vario(t, "", "backfill", "--source", "t3code", "--force"); r.code != 0 {
		t.Fatalf("forced reconciliation: %+v", r)
	}
	if got := read(); got != want {
		t.Fatalf("repeated reconciliation changed results: %+v, want %+v", got, want)
	}
	r = vario(t, "", "doctor")
	if r.code != 0 || !strings.Contains(r.stdout, "no rollback detected") {
		t.Fatalf("doctor after the recovery: %+v", r)
	}
	if s := vario(t, "", "status").stdout; strings.Contains(s, "RESTORED TO AN EARLIER STATE") || strings.Contains(s, "last error") {
		t.Fatalf("status after the recovery:\n%s", s)
	}
}
