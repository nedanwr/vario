package collector

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/nedanwr/vario/internal/config"
	"github.com/nedanwr/vario/internal/privacy"
	"github.com/nedanwr/vario/internal/sources"
	"github.com/nedanwr/vario/internal/sources/t3code"
	"github.com/nedanwr/vario/internal/sources/t3code/t3codetest"
	"github.com/nedanwr/vario/internal/spool"
	"github.com/nedanwr/vario/internal/storage/clickhouse"
)

type archive struct {
	Turns  int    `json:"turns"`
	Input  uint64 `json:"input"`
	Msgs   int    `json:"msgs"`
	Events int    `json:"events"`
	Raw    int    `json:"raw"`
}

// F1: changing the endpoint or database must never send queued records to an
// unintended database, reuse another destination's fingerprints, assume that
// records exist at a newly selected (or recreated) database, leave a silent
// partial backfill, or lose data when a change is interrupted.
func TestIntegrationDestinationSafety(t *testing.T) {
	url := os.Getenv("VARIO_TEST_CLICKHOUSE_URL")
	if url == "" {
		t.Skip("set VARIO_TEST_CLICKHOUSE_URL to run ClickHouse integration tests")
	}
	ctx := context.Background()
	user, pw := os.Getenv("VARIO_TEST_CLICKHOUSE_USER"), os.Getenv("VARIO_TEST_CLICKHOUSE_PASSWORD")
	base := config.Default().ClickHouse
	base.URL, base.Username = url, user
	suffix := time.Now().UnixNano()
	dbA, dbB := fmt.Sprintf("vario_f1a_%d", suffix), fmt.Sprintf("vario_f1b_%d", suffix)

	clients := map[string]*clickhouse.Client{}
	migrate := func(db string) string {
		t.Helper()
		cfg := base
		cfg.Database = db
		admin, err := clickhouse.NewAdmin(cfg, user, pw)
		if err != nil {
			t.Fatal(err)
		}
		plan, err := admin.PlanSchema(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if res := admin.Apply(ctx, plan, nil); !res.OK() {
			t.Fatal(res.Err)
		}
		ch, err := clickhouse.New(cfg, pw)
		if err != nil {
			t.Fatal(err)
		}
		clients[db] = ch
		info, err := ch.CheckDestination(ctx)
		if err != nil || info.ServerID == "" {
			t.Fatalf("migrated destination %s: %+v %v", db, info, err)
		}
		return info.ServerID
	}
	for _, db := range []string{dbA, dbB} {
		db := db
		t.Cleanup(func() { _ = clients[dbA].Exec(context.Background(), "DROP DATABASE IF EXISTS `"+db+"`") })
	}
	migrate(dbA)
	idB := migrate(dbB)
	read := func(db string) archive {
		t.Helper()
		var out []archive
		q := fmt.Sprintf(`SELECT (SELECT sum(provider_turns) FROM %[1]s.token_usage_daily) AS turns,
			(SELECT sum(input_tokens) FROM %[1]s.token_usage_daily) AS input,
			(SELECT count() FROM %[1]s.messages_latest) AS msgs,
			(SELECT count() FROM %[1]s.execution_events_latest) AS events,
			(SELECT count() FROM %[1]s.raw_events_latest) AS raw`, "`"+db+"`")
		if err := clients[db].Query(ctx, q, &out); err != nil {
			t.Fatal(err)
		}
		return out[0]
	}
	empty := archive{}

	fix := t3codetest.New(t)
	fix.Standard()
	pol, _ := privacy.New(config.Default().Privacy)
	statePath := filepath.Join(t.TempDir(), "state.db")
	store, err := spool.Open(statePath, 64<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { store.Close() }()
	src := t3code.New(fix.Path, pol, 2)
	defer src.Close()
	endpoint := base.Endpoint()
	collectorFor := func(db string) *Collector {
		return New(store, clients[db], []sources.Source{src}, Options{BatchSize: 2, MaxInsertRows: 3, RetryInitial: time.Millisecond,
			RetryMax: time.Millisecond, PolicyFingerprint: pol.Fingerprint(), Endpoint: endpoint, Database: db},
			slog.New(slog.NewTextHandler(io.Discard, nil)))
	}
	drain := func(c *Collector) {
		t.Helper()
		for i := 0; i < 100; i++ {
			dr, err := c.Drain(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if dr.Remaining == 0 {
				return
			}
		}
		t.Fatal("spool did not drain")
	}
	pendingFor := func(db string) int64 {
		t.Helper()
		ds, err := store.Destinations(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range ds {
			if d.Database == db {
				return d.Pending
			}
		}
		return 0
	}
	// switchTo does what `vario destination switch --keep-pending` does.
	switchTo := func(db, serverID string) {
		t.Helper()
		res, err := store.Resolve(ctx, endpoint, db, serverID)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Activate(ctx, res.Configured.ID); err != nil {
			t.Fatal(err)
		}
	}

	// 1. Rows are queued for A but not delivered yet.
	a := collectorFor(dbA)
	if _, err := a.Prepare(ctx, true); err != nil {
		t.Fatal(err)
	}
	first, err := a.Snapshot(ctx, src, false, true)
	if err != nil || first.Enqueued == 0 {
		t.Fatalf("snapshot: %+v %v", first, err)
	}
	queued := pendingFor(dbA)

	// 2. The configuration now names B: the collector refuses, and nothing
	// queued for A reaches B.
	b := collectorFor(dbB)
	_, err = b.Prepare(ctx, true)
	var de *DestinationError
	if !errors.As(err, &de) || de.Previous == nil || de.Previous.Pending != queued {
		t.Fatalf("a destination change with queued rows must be refused: %v", err)
	}
	if _, err := b.Drain(ctx); err == nil {
		t.Fatal("a refused collector must not deliver")
	}
	if got := read(dbB); got != empty {
		t.Fatalf("queued rows reached the unintended database: %+v", got)
	}

	// 3. Interrupted change (the process stops before a decision): after a
	// restart nothing was lost or redirected, and the change is still refused.
	store.Close()
	if store, err = spool.Open(statePath, 64<<20); err != nil {
		t.Fatal(err)
	}
	if pendingFor(dbA) != queued {
		t.Fatalf("queued rows lost across an interrupted change: %d of %d", pendingFor(dbA), queued)
	}
	if _, err := collectorFor(dbB).Prepare(ctx, true); !errors.As(err, &de) {
		t.Fatalf("still refused after restart: %v", err)
	}

	// 4. Deliver to A, then queue a live update for A.
	a = collectorFor(dbA)
	if _, err := a.Prepare(ctx, true); err != nil {
		t.Fatal(err)
	}
	drain(a)
	before := read(dbA)
	if before.Turns != 1 || before.Input != 818885 || before.Msgs != 2 || before.Raw == 0 {
		t.Fatalf("archive A: %+v", before)
	}
	fix.ProviderTurn("turn-1", "thread-1", "run-1:attempt:1", "completed", nil,
		map[string]any{"usageScope": "main_agent", "usageStatus": "complete", "inputTokens": 900000, "outputTokens": 1}, nil)
	if err := a.syncOnce(ctx, src, a.log); err != nil {
		t.Fatal(err)
	}
	if pendingFor(dbA) == 0 {
		t.Fatal("expected the update to be queued for A")
	}

	// 5. Explicit switch to B, keeping A's queued update: B receives a
	// complete backfill (A's fingerprints and cursor are not reused), and
	// A's queued rows are not sent to B.
	switchTo(dbB, idB)
	b = collectorFor(dbB)
	if _, err := b.Prepare(ctx, true); err != nil {
		t.Fatal(err)
	}
	full, err := b.Snapshot(ctx, src, false, true)
	if err != nil || full.Enqueued < first.Enqueued {
		t.Fatalf("B must receive a full backfill: %+v (first %+v) %v", full, first, err)
	}
	drain(b)
	gotB := read(dbB)
	if gotB.Turns != 1 || gotB.Input != 900000 || gotB.Msgs != before.Msgs || gotB.Events != before.Events || gotB.Raw != before.Raw+1 {
		t.Fatalf("B's archive is incomplete: %+v (A had %+v before the update)", gotB, before)
	}
	if read(dbA) != before || pendingFor(dbA) == 0 {
		t.Fatal("A's queued rows must stay queued for A while B is active")
	}

	// 6. Switching back delivers A's queued rows to A only.
	switchTo(dbA, "")
	a = collectorFor(dbA)
	if _, err := a.Prepare(ctx, true); err != nil {
		t.Fatal(err)
	}
	drain(a)
	if got := read(dbA); got != gotB {
		t.Fatalf("A after delivering its queued rows %+v, B %+v", got, gotB)
	}
	if read(dbB) != gotB {
		t.Fatal("B must not receive anything while A is active")
	}

	// 7. B is dropped and recreated: Vario does not assume its records still
	// exist, refuses to continue silently, and after an explicit switch
	// backfills it in full.
	switchTo(dbB, idB)
	if err := clients[dbA].Exec(ctx, "DROP DATABASE `"+dbB+"`"); err != nil {
		t.Fatal(err)
	}
	newID := migrate(dbB)
	if newID == idB {
		t.Fatal("a recreated database must get a new destination id")
	}
	b = collectorFor(dbB)
	if _, err := b.Prepare(ctx, true); !errors.As(err, &de) || !de.Replaced {
		t.Fatalf("a recreated database must be detected: %v", err)
	}
	res, err := store.Resolve(ctx, endpoint, dbB, newID)
	if err != nil || !res.Replaced {
		t.Fatalf("resolve: %+v %v", res, err)
	}
	repl, err := store.Replace(ctx, res.Configured.ID, newID) // what `vario destination switch` does
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Activate(ctx, repl.ID); err != nil {
		t.Fatal(err)
	}
	b = collectorFor(dbB)
	if _, err := b.Prepare(ctx, true); err != nil {
		t.Fatal(err)
	}
	if res, err := b.Snapshot(ctx, src, false, true); err != nil || res.Enqueued < first.Enqueued {
		t.Fatalf("the recreated database must be backfilled in full: %+v %v", res, err)
	}
	drain(b)
	if got := read(dbB); got != gotB {
		t.Fatalf("recreated B: %+v, want %+v", got, gotB)
	}
}
