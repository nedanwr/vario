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

// ambiguousSink commits every other insert but reports a timeout, like a
// connection that drops after ClickHouse accepted the data.
type ambiguousSink struct {
	*clickhouse.Client
	n int
}

func (s *ambiguousSink) Insert(ctx context.Context, table string, body []byte, token string) error {
	if err := s.Client.Insert(ctx, table, body, token); err != nil {
		return err
	}
	s.n++
	if s.n%2 == 1 {
		return &clickhouse.Error{Message: "read: connection reset by peer", Transient: true}
	}
	return nil
}

// Repeated backfills and ambiguous deliveries must not change logical results.
func TestIntegrationRedeliveryDoesNotInflateResults(t *testing.T) {
	url := os.Getenv("VARIO_TEST_CLICKHOUSE_URL")
	if url == "" {
		t.Skip("set VARIO_TEST_CLICKHOUSE_URL to run ClickHouse integration tests")
	}
	cfg := config.Default().ClickHouse
	cfg.URL = url
	cfg.Username = os.Getenv("VARIO_TEST_CLICKHOUSE_USER")
	cfg.Database = fmt.Sprintf("vario_e2e_%d", time.Now().UnixNano())
	ch, err := clickhouse.New(cfg, os.Getenv("VARIO_TEST_CLICKHOUSE_PASSWORD"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ch.Exec(context.Background(), "DROP DATABASE IF EXISTS `"+cfg.Database+"`") })

	fix := t3codetest.New(t)
	fix.Standard()
	pol, _ := privacy.New(config.Default().Privacy)
	store, err := spool.Open(filepath.Join(t.TempDir(), "state.db"), 64<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	src := t3code.New(fix.Path, pol, 2)
	defer src.Close()
	sink := &ambiguousSink{Client: ch}
	c := New(store, sink, []sources.Source{src}, Options{BatchSize: 2, MaxInsertRows: 3, RetryInitial: time.Millisecond,
		RetryMax: time.Millisecond, PolicyFingerprint: pol.Fingerprint()}, slog.New(slog.NewTextHandler(io.Discard, nil)))

	drain := func() {
		t.Helper()
		for i := 0; i < 200; i++ {
			dr, err := c.Drain(ctx)
			if err == nil && dr.Remaining == 0 {
				return
			}
			var chErr *clickhouse.Error
			if err != nil && !errors.As(err, &chErr) {
				t.Fatal(err)
			}
		}
		t.Fatal("spool did not drain")
	}
	type totals struct {
		Turns  int    `json:"turns"`
		Input  uint64 `json:"input"`
		Msgs   int    `json:"msgs"`
		Events int    `json:"events"`
		Raw    int    `json:"raw"`
	}
	read := func() totals {
		t.Helper()
		var out []totals
		db := "`" + cfg.Database + "`"
		q := fmt.Sprintf(`SELECT (SELECT sum(provider_turns) FROM %[1]s.token_usage_daily) AS turns,
			(SELECT sum(input_tokens) FROM %[1]s.token_usage_daily) AS input,
			(SELECT count() FROM %[1]s.messages_latest) AS msgs,
			(SELECT count() FROM %[1]s.execution_events_latest) AS events,
			(SELECT count() FROM %[1]s.raw_events_latest) AS raw`, db)
		if err := ch.Query(ctx, q, &out); err != nil {
			t.Fatal(err)
		}
		return out[0]
	}

	if _, err := c.Snapshot(ctx, src, false, true); err != nil {
		t.Fatal(err)
	}
	drain()
	first := read()
	if first.Turns != 1 || first.Input != 818885 || first.Msgs != 2 || first.Events != 4 || first.Raw == 0 {
		t.Fatalf("unexpected initial totals: %+v", first)
	}
	// Forced re-backfill re-sends everything with the same versions.
	if _, err := c.Snapshot(ctx, src, true, true); err != nil {
		t.Fatal(err)
	}
	drain()
	// A live update produces a new version of one turn.
	fix.ProviderTurn("turn-1", "thread-1", "run-1:attempt:1", "completed", nil,
		map[string]any{"usageScope": "main_agent", "usageStatus": "complete", "inputTokens": 900000, "outputTokens": 1}, nil)
	if err := c.syncOnce(ctx, src, c.log); err != nil {
		t.Fatal(err)
	}
	drain()
	got := read()
	if got.Turns != 1 || got.Input != 900000 || got.Msgs != first.Msgs || got.Events != first.Events || got.Raw != first.Raw+1 {
		t.Fatalf("redelivery inflated or lost results: first %+v, now %+v", first, got)
	}
}
