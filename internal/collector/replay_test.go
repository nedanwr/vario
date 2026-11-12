package collector

import (
	"testing"
)

// loseRowsKeepTokens removes rows like ALTER ... DELETE, REPLACE PARTITION or
// RESTORE into existing tables do: the rows go back to what b holds (all
// rows go if b is nil), the deduplication memory keeps every token it had.
func (s *fakeSink) loseRowsKeepTokens(b *sinkBackup) {
	s.mu.Lock()
	tokens := s.tokens
	s.mu.Unlock()
	if b != nil {
		s.restore(*b)
	} else {
		s.mu.Lock()
		s.rows = map[string][]map[string]any{}
		s.mu.Unlock()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for k := range tokens {
		s.tokens[k] = true
	}
}

func (h *harness) drainAll(t *testing.T, c *Collector) {
	t.Helper()
	if dr, err := c.Drain(ctx); err != nil || dr.Remaining != 0 {
		t.Fatalf("drain: %+v %v", dr, err)
	}
}

// `vario backfill --force` restores deleted rows although the database
// remembers the tokens of the batches that delivered them, and repeating it
// counts every record once.
func TestForcedBackfillRestoresRowsWhoseTokensAreRemembered(t *testing.T) {
	h := setup(t)
	h.syncDrain(t, h.c)
	want := h.sink.logical()
	if want.Messages == 0 || want.Turns == 0 || want.Input == 0 {
		t.Fatalf("initial delivery: %+v", want)
	}
	for round := 1; round <= 2; round++ {
		h.sink.loseRowsKeepTokens(nil)
		if _, err := h.c.Snapshot(ctx, h.src, true, true); err != nil {
			t.Fatal(err)
		}
		h.drainAll(t, h.c)
		if got := h.sink.logical(); got != want {
			t.Fatalf("round %d: the forced backfill must restore everything once:\n got %+v\nwant %+v", round, got, want)
		}
	}
	// Without a loss, a forced backfill changes no result.
	if _, err := h.c.Snapshot(ctx, h.src, true, true); err != nil {
		t.Fatal(err)
	}
	h.drainAll(t, h.c)
	if got := h.sink.logical(); got != want {
		t.Fatalf("a repeated forced backfill changed results: %+v, want %+v", got, want)
	}
}

// A database restored to a state taken before the initial backfill, keeping
// its deduplication memory: after the rollback is accepted, the
// reconciliation re-sends exactly the batches of the initial backfill and
// must restore them all.
func TestReconciliationRestoresRowsWhoseTokensAreRemembered(t *testing.T) {
	h := setup(t)
	h.setServerID("id-A")
	c := h.collector(t, h.opts)
	destID := c.Destination().ID
	empty := h.sink.backup()
	h.syncDrain(t, c)
	want := h.sink.logical()

	for round := 1; round <= 2; round++ {
		h.sink.loseRowsKeepTokens(&empty)
		due(c)
		if _, err := c.Drain(ctx); err == nil {
			t.Fatalf("round %d: delivery must stop at the restored database", round)
		} else if _, ok := isRollback(err); !ok {
			t.Fatalf("round %d: %v", round, err)
		}
		if err := h.store.AcceptRollback(ctx, destID); err != nil {
			t.Fatal(err)
		}
		c = h.collector(t, h.opts)
		h.syncDrain(t, c)
		if got := h.sink.logical(); got != want {
			t.Fatalf("round %d: the reconciliation must restore everything once:\n got %+v\nwant %+v", round, got, want)
		}
	}
}

// Ambiguous acknowledgements during a replay: every retry carries the token
// of its first attempt, so the database keeps one copy, and the replay
// restores everything.
func TestReplayRetriesKeepTheirTokens(t *testing.T) {
	h := setup(t)
	h.syncDrain(t, h.c)
	want := h.sink.logical()
	h.sink.loseRowsKeepTokens(nil)
	if _, err := h.c.Snapshot(ctx, h.src, true, true); err != nil {
		t.Fatal(err)
	}
	h.sink.mu.Lock()
	h.sink.ambiguousData = 3
	h.sink.mu.Unlock()
	for i := 0; i < 20; i++ {
		if dr, err := h.c.Drain(ctx); err == nil && dr.Remaining == 0 {
			break
		}
	}
	if got := h.sink.logical(); got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	// Physically one copy per record: no retry was stored twice.
	for table, n := range map[string]int{"messages": want.Messages, "provider_turns": want.Turns, "raw_events": want.Raw} {
		if got := h.sink.count(table); got != n {
			t.Fatalf("%s: %d rows stored for %d records", table, got, n)
		}
	}
}
