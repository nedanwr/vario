package cli

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/nedanwr/vario/internal/config"
	"github.com/nedanwr/vario/internal/model"
	"github.com/nedanwr/vario/internal/storage/clickhouse"
)

// The commands against a real ClickHouse server: rows are deleted with a
// mutation, which keeps ClickHouse's memory of the insert tokens that
// delivered them. `vario backfill --force` must restore them, and so must
// the reconciliation after `vario destination switch` accepted a database
// that also lost its delivery checkpoints this way.
func TestIntegrationCLIReplaysRestoreRowsWithRememberedTokens(t *testing.T) {
	url := os.Getenv("VARIO_TEST_CLICKHOUSE_URL")
	if url == "" {
		t.Skip("set VARIO_TEST_CLICKHOUSE_URL to run ClickHouse integration tests")
	}
	user, pw := os.Getenv("VARIO_TEST_CLICKHOUSE_USER"), os.Getenv("VARIO_TEST_CLICKHOUSE_PASSWORD")
	if user == "" {
		user = "default"
	}
	db := fmt.Sprintf("vario_cli_replay_%d", time.Now().UnixNano())
	initFixture(t)
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
	deleteAll := func(tables ...string) {
		t.Helper()
		for _, table := range tables {
			q := fmt.Sprintf("ALTER TABLE `%s`.`%s` DELETE WHERE 1 SETTINGS mutations_sync = 2", db, table)
			if err := ch.Exec(ctx, q); err != nil {
				t.Fatalf("%s: %v", q, err)
			}
		}
	}
	type totals struct {
		Msgs   int    `json:"msgs"`
		Turns  int    `json:"turns"`
		Input  uint64 `json:"input"`
		Raw    int    `json:"raw"`
		Events int    `json:"events"`
	}
	read := func() totals {
		t.Helper()
		var out []totals
		q := fmt.Sprintf(`SELECT (SELECT count() FROM %[1]s.messages_latest) AS msgs, (SELECT count() FROM %[1]s.provider_turns_latest) AS turns,
			(SELECT sum(input_tokens) FROM %[1]s.token_usage_daily) AS input, (SELECT count() FROM %[1]s.raw_events_latest) AS raw,
			(SELECT count() FROM %[1]s.execution_events_latest) AS events`, "`"+db+"`")
		if err := ch.Query(ctx, q, &out); err != nil {
			t.Fatal(err)
		}
		return out[0]
	}
	run := func(args ...string) result {
		t.Helper()
		r := vario(t, "", args...)
		if r.code != 0 {
			t.Fatalf("%v: %+v", args, r)
		}
		return r
	}

	if r := vario(t, pw, "migrate", "--non-interactive", "--yes", "--admin-user", user, "--admin-password-stdin"); r.code != 0 {
		t.Fatalf("migrate: %+v", r)
	}
	run("backfill", "--source", "t3code")
	want := read()
	if want.Msgs != 2 || want.Turns != 1 || want.Input != 818885 || want.Raw == 0 || want.Events == 0 {
		t.Fatalf("initial delivery: %+v", want)
	}

	// `vario backfill --force` after the rows were deleted.
	for round := 1; round <= 2; round++ {
		deleteAll(model.Tables...)
		if got := read(); got.Msgs != 0 || got.Raw != 0 {
			t.Fatalf("round %d: rows were not deleted: %+v", round, got)
		}
		run("backfill", "--source", "t3code", "--force")
		if got := read(); got != want {
			t.Fatalf("round %d: backfill --force must restore every record once:\n got %+v\nwant %+v", round, got, want)
		}
	}

	// The rows and the delivery checkpoints are deleted: a rollback, accepted
	// with `vario destination switch`, then reconciled by a plain backfill.
	deleteAll(append(model.Tables, clickhouse.CheckpointTable)...)
	if r := vario(t, "", "backfill", "--source", "t3code"); r.code != ExitUsage {
		t.Fatalf("the rolled-back database must be refused: %+v", r)
	}
	run("destination", "switch")
	run("backfill", "--source", "t3code")
	if got := read(); got != want {
		t.Fatalf("the reconciliation after the switch must restore every record once:\n got %+v\nwant %+v", got, want)
	}
	run("backfill", "--source", "t3code", "--force")
	if got := read(); got != want {
		t.Fatalf("a repeated forced backfill changed results: %+v, want %+v", got, want)
	}
}
