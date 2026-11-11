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

// recoveryEnv is a synthetic T3 Code source, a spool and throwaway ClickHouse
// databases for the destination recovery tests.
type recoveryEnv struct {
	t        *testing.T
	base     config.ClickHouseConfig
	user, pw string
	endpoint string
	clients  map[string]*clickhouse.Client
	fix      *t3codetest.Fixture
	src      *t3code.Source
	store    *spool.Store
	pol      *privacy.Policy
}

func newRecoveryEnv(t *testing.T) *recoveryEnv {
	t.Helper()
	url := os.Getenv("VARIO_TEST_CLICKHOUSE_URL")
	if url == "" {
		t.Skip("set VARIO_TEST_CLICKHOUSE_URL to run ClickHouse integration tests")
	}
	e := &recoveryEnv{t: t, user: os.Getenv("VARIO_TEST_CLICKHOUSE_USER"), pw: os.Getenv("VARIO_TEST_CLICKHOUSE_PASSWORD"), clients: map[string]*clickhouse.Client{}}
	e.base = config.Default().ClickHouse
	e.base.URL, e.base.Username = url, e.user
	e.endpoint = e.base.Endpoint()
	e.fix = t3codetest.New(t)
	e.fix.Standard()
	e.pol, _ = privacy.New(config.Default().Privacy)
	store, err := spool.Open(filepath.Join(t.TempDir(), "state.db"), 64<<20)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	e.store = store
	e.src = t3code.New(e.fix.Path, e.pol, 2)
	t.Cleanup(func() { e.src.Close() })
	return e
}

// migrate creates (or recreates) Vario's schema in db and returns its destination id.
func (e *recoveryEnv) migrate(db string) string {
	e.t.Helper()
	cfg := e.base
	cfg.Database = db
	admin, err := clickhouse.NewAdmin(cfg, e.user, e.pw)
	if err != nil {
		e.t.Fatal(err)
	}
	if _, ok := e.clients[db]; !ok {
		e.t.Cleanup(func() { _ = admin.Exec(context.Background(), "DROP DATABASE IF EXISTS `"+db+"`") })
	}
	plan, err := admin.PlanSchema(ctx)
	if err != nil {
		e.t.Fatal(err)
	}
	if res := admin.Apply(ctx, plan, nil); !res.OK() {
		e.t.Fatal(res.Err)
	}
	ch, err := clickhouse.New(cfg, e.pw)
	if err != nil {
		e.t.Fatal(err)
	}
	e.clients[db] = ch
	info, err := ch.CheckDestination(ctx)
	if err != nil || info.ServerID == "" {
		e.t.Fatalf("migrated destination %s: %+v %v", db, info, err)
	}
	return info.ServerID
}

func (e *recoveryEnv) collector(db string) *Collector {
	return New(e.store, e.clients[db], []sources.Source{e.src}, Options{PollInterval: 20 * time.Millisecond, FlushInterval: 20 * time.Millisecond,
		BatchSize: 2, MaxInsertRows: 3, RetryInitial: time.Millisecond, RetryMax: 20 * time.Millisecond,
		PolicyFingerprint: e.pol.Fingerprint(), Endpoint: e.endpoint, Database: db}, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func (e *recoveryEnv) prepared(db string) *Collector {
	e.t.Helper()
	c := e.collector(db)
	if _, err := c.Prepare(ctx, true); err != nil {
		e.t.Fatal(err)
	}
	return c
}

// syncAndDrain does what a running collector does: one source sync (never
// forced) and delivery of everything queued.
func (e *recoveryEnv) syncAndDrain(c *Collector) {
	e.t.Helper()
	if err := c.syncOnce(ctx, e.src, c.log); err != nil {
		e.t.Fatal(err)
	}
	for i := 0; i < 100; i++ {
		dr, err := c.Drain(ctx)
		if err != nil {
			e.t.Fatal(err)
		}
		if dr.Remaining == 0 {
			return
		}
	}
	e.t.Fatal("spool did not drain")
}

// logical is the archive as its users see it (the deduplicating views).
type logical struct {
	Turns  int    `json:"turns"`
	Input  uint64 `json:"input"`
	Msgs   int    `json:"msgs"`
	Events int    `json:"events"`
	Raw    int    `json:"raw"`
	Convs  int    `json:"convs"`
}

func (e *recoveryEnv) read(db string) (logical, error) {
	var out []logical
	q := fmt.Sprintf(`SELECT (SELECT count() FROM %[1]s.provider_turns_latest) AS turns,
		(SELECT sum(input_tokens) FROM %[1]s.token_usage_daily) AS input,
		(SELECT count() FROM %[1]s.messages_latest) AS msgs,
		(SELECT count() FROM %[1]s.execution_events_latest) AS events,
		(SELECT count() FROM %[1]s.raw_events_latest) AS raw,
		(SELECT count() FROM %[1]s.conversations_latest) AS convs`, "`"+db+"`")
	if err := e.clients[db].Query(ctx, q, &out); err != nil {
		return logical{}, err
	}
	return out[0], nil
}

func (e *recoveryEnv) mustRead(db string) logical {
	e.t.Helper()
	got, err := e.read(db)
	if err != nil {
		e.t.Fatal(err)
	}
	return got
}

// moreHistory adds a second conversation with a provider turn and a message.
func (e *recoveryEnv) moreHistory(n int) {
	thread, run := fmt.Sprintf("thread-x%d", n), fmt.Sprintf("run-x%d", n)
	e.fix.Thread(thread, "proj-1", "Follow-up", "2026-03-02T09:00:00.000Z")
	e.fix.Run(run, thread, 1, "claude-test-1")
	e.fix.Message(fmt.Sprintf("msg-x%d", n), thread, run, "user", "another question", false)
	e.fix.ProviderTurn(fmt.Sprintf("turn-x%d", n), thread, run+":attempt:1", "completed", nil,
		map[string]any{"usageScope": "main_agent", "usageStatus": "complete", "inputTokens": 1000, "outputTokens": 10}, nil)
}

// C-1: after `vario destination switch --move-pending` or --discard-pending,
// the previous destination never received the rows that were queued for it.
// Returning to it must restore the complete history by normal operation
// (no `backfill --force`), keep the queued rows according to the chosen
// policy, and count every logical record once.
func TestIntegrationSwitchBackRestoresCompleteHistory(t *testing.T) {
	for _, policy := range []string{"move", "discard"} {
		t.Run(policy, func(t *testing.T) {
			e := newRecoveryEnv(t)
			suffix := time.Now().UnixNano()
			dbA, dbB := fmt.Sprintf("vario_c1a_%s_%d", policy, suffix), fmt.Sprintf("vario_c1b_%s_%d", policy, suffix)
			idA, idB := e.migrate(dbA), e.migrate(dbB)

			// A holds the history so far; newer records are queued for A but not delivered.
			a := e.prepared(dbA)
			e.syncAndDrain(a)
			delivered := e.mustRead(dbA)
			e.moreHistory(1)
			if err := a.syncOnce(ctx, e.src, a.log); err != nil {
				t.Fatal(err)
			}
			queued, err := e.store.Stats(ctx)
			if err != nil || queued.Rows == 0 {
				t.Fatalf("expected rows queued for A: %+v %v", queued, err)
			}

			// What `vario destination switch --move-pending / --discard-pending` does.
			res, err := e.store.Resolve(ctx, e.endpoint, dbB, idB)
			if err != nil || res.Previous == nil || res.Previous.Pending != queued.Rows {
				t.Fatalf("resolve B: %+v %v", res, err)
			}
			var n int64
			if policy == "move" {
				n, err = e.store.MovePending(ctx, res.Previous.ID, res.Configured.ID)
			} else {
				n, err = e.store.DiscardPending(ctx, res.Previous.ID)
			}
			if err != nil || n != queued.Rows {
				t.Fatalf("%s: %d of %d rows, %v", policy, n, queued.Rows, err)
			}
			if err := e.store.Activate(ctx, res.Configured.ID); err != nil {
				t.Fatal(err)
			}
			b := e.prepared(dbB)
			if st, _ := e.store.Stats(ctx); (policy == "move") != (st.Rows == queued.Rows) {
				t.Fatalf("%s: %d rows queued for B, A had %d", policy, st.Rows, queued.Rows)
			}
			e.syncAndDrain(b)
			complete := e.mustRead(dbB)
			if complete.Turns != delivered.Turns+1 || complete.Msgs != delivered.Msgs+1 || complete.Convs != delivered.Convs+1 || complete.Raw <= delivered.Raw {
				t.Fatalf("B must be complete, with each record once: %+v (A had %+v)", complete, delivered)
			}
			if got := e.mustRead(dbA); got != delivered {
				t.Fatalf("A must not change while B is active: %+v", got)
			}

			// Back to A: nothing is queued for B, so the collector switches by itself.
			a = e.prepared(dbA)
			if a.Destination().ServerID != idA {
				t.Fatalf("destination A: %+v", a.Destination())
			}
			e.syncAndDrain(a)
			if got := e.mustRead(dbA); got != complete {
				t.Fatalf("switching back left a gap in A: %+v, source has %+v", got, complete)
			}
			// It stays complete and consistent in normal operation.
			e.moreHistory(2)
			e.syncAndDrain(a)
			if got := e.mustRead(dbA); got.Turns != complete.Turns+1 || got.Msgs != complete.Msgs+1 || got.Convs != complete.Convs+1 {
				t.Fatalf("A after further changes: %+v", got)
			}
			if got := e.mustRead(dbB); got != complete {
				t.Fatalf("B must not receive anything while A is active: %+v", got)
			}
		})
	}
}

// C-2: the database is dropped and recreated while an idle collector keeps
// running. The collector must notice the new incarnation, stop, and after
// the explicit switch the recreated database must hold the complete history,
// not just the changes that happened afterwards.
func TestIntegrationRecreatedDatabaseWhileRunning(t *testing.T) {
	old := identityCheckInterval
	identityCheckInterval = 200 * time.Millisecond
	t.Cleanup(func() { identityCheckInterval = old })

	e := newRecoveryEnv(t)
	db := fmt.Sprintf("vario_c2_%d", time.Now().UnixNano())
	firstID := e.migrate(db)
	e.moreHistory(1)

	c := e.prepared(db)
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()
	var complete logical
	waitFor(t, func() bool {
		got, err := e.read(db)
		complete = got
		return err == nil && got.Convs == 2 && got.Turns == 2 && got.Msgs == 3 && got.Events == 4 && got.Raw > 0
	})
	waitFor(t, func() bool { st, err := e.store.Stats(ctx); return err == nil && st.Rows == 0 }) // idle

	// Dropped and recreated behind the running collector.
	if err := e.clients[db].Exec(ctx, "DROP DATABASE `"+db+"`"); err != nil {
		t.Fatal(err)
	}
	newID := e.migrate(db)
	if newID == firstID {
		t.Fatal("a recreated database must get a new destination id")
	}
	e.moreHistory(2) // a later change, which must not be all the new database gets

	var de *DestinationError
	select {
	case err := <-done:
		if !errors.As(err, &de) || !de.Replaced {
			t.Fatalf("Run should stop with a replaced-destination error: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the running collector did not notice the recreated database")
	}
	if d, _, _ := e.store.Active(ctx); d.ServerID != firstID {
		t.Fatalf("the recorded identity must not be overwritten: %+v", d)
	}
	if _, err := e.collector(db).Prepare(ctx, true); !errors.As(err, &de) || !de.Replaced {
		t.Fatalf("a restart must be refused until the switch: %v", err)
	}

	// `vario destination switch`: the recreated database is a new destination.
	res, err := e.store.Resolve(ctx, e.endpoint, db, newID)
	if err != nil || !res.Replaced {
		t.Fatalf("resolve: %+v %v", res, err)
	}
	repl, err := e.store.Replace(ctx, res.Configured.ID, newID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.MovePending(ctx, res.Configured.ID, repl.ID); err != nil {
		t.Fatal(err)
	}
	if err := e.store.Activate(ctx, repl.ID); err != nil {
		t.Fatal(err)
	}
	fresh := e.prepared(db)
	e.syncAndDrain(fresh)
	got := e.mustRead(db)
	if got.Convs != complete.Convs+1 || got.Turns != complete.Turns+1 || got.Msgs != complete.Msgs+1 || got.Events != complete.Events || got.Raw <= complete.Raw {
		t.Fatalf("the recreated database is incomplete: %+v, before the drop %+v plus one conversation", got, complete)
	}
}
