package spool

import (
	"errors"
	"testing"
	"time"

	"github.com/nedanwr/vario/internal/model"
)

// tokens returns the token of every queued entry of the selected destination, by id.
func tokens(t *testing.T, s *Store) map[int64]string {
	t.Helper()
	es, err := s.Due(ctx, time.Now().Add(time.Hour), 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	m := map[int64]string{}
	for _, e := range es {
		m[e.ID] = e.Token
	}
	return m
}

// commitOne queues recs and returns the token of the single new entry.
func commitOne(t *testing.T, s *Store, recs []model.Record, opts CommitOptions) string {
	t.Helper()
	before := tokens(t, s)
	if _, err := s.Commit(ctx, "src", recs, nil, opts); err != nil {
		t.Fatal(err)
	}
	var tok string
	n := 0
	for id, tk := range tokens(t, s) {
		if _, ok := before[id]; !ok {
			tok = tk
			n++
		}
	}
	if n != 1 {
		t.Fatalf("expected one new entry, got %d", n)
	}
	return tok
}

func replayOf(t *testing.T, s *Store) string {
	t.Helper()
	r, err := s.Replay(ctx, s.Selected())
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// A retry keeps the token of its entry; a replay of the same content gets a
// token no earlier delivery used.
func TestReplayGetsNewTokensRetriesKeepTheirs(t *testing.T) {
	s, path := open(t, 0)
	if _, err := s.Commit(ctx, "src", nil, ptr("c0"), CommitOptions{}); err != nil {
		t.Fatal(err)
	}
	if r := replayOf(t, s); r != "" {
		t.Fatalf("a commit without rows needs no replay identity: %q", r)
	}
	recs := []model.Record{msg("m1", "a", 1), msg("m2", "b", 1)}
	first := commitOne(t, s, recs, CommitOptions{})
	identity := replayOf(t, s)
	if identity == "" {
		t.Fatal("the first queued entry must create the replay identity")
	}

	// Transport retries: failures and a restart leave the token unchanged.
	ids := queuedIDs(t, s)
	for i := 0; i < 3; i++ {
		if err := s.Fail(ctx, ids, time.Now(), "connection reset by peer"); err != nil {
			t.Fatal(err)
		}
	}
	s.Close()
	s, err := Open(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	use(t, s, testEndpoint, "vario")
	if got := tokens(t, s); len(got) != 1 || got[ids[0]] != first {
		t.Fatalf("a retried entry must keep its token: %v, want %s", got, first)
	}
	if r := replayOf(t, s); r != identity {
		t.Fatalf("the replay identity must survive a restart: %q, was %q", r, identity)
	}

	// The same content queued again without a replay (a forced commit
	// outside a replay) keeps the destination's identity: same token.
	if again := commitOne(t, s, recs, CommitOptions{Force: true}); again != first {
		t.Fatalf("same content under the same identity must get the same token: %s, want %s", again, first)
	}

	// An explicit replay: a new identity and a new token for the same content.
	if err := s.BeginReplay(ctx); err != nil {
		t.Fatal(err)
	}
	replayed := commitOne(t, s, recs, CommitOptions{Force: true})
	if replayed == first || replayOf(t, s) == identity {
		t.Fatalf("a replay must not reuse the token of the earlier delivery: %s", replayed)
	}
	// Entries queued before the replay keep their tokens.
	if got := tokens(t, s)[ids[0]]; got != first {
		t.Fatalf("an entry queued before the replay changed its token: %s, want %s", got, first)
	}
	// The replay is continued after a restart under the same identity.
	identity = replayOf(t, s)
	s.Close()
	s, err = Open(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	use(t, s, testEndpoint, "vario")
	if again := commitOne(t, s, recs, CommitOptions{Force: true}); again != replayed || replayOf(t, s) != identity {
		t.Fatalf("a continued replay must keep its identity: %s, want %s", again, replayed)
	}
}

// Every reset of a destination's synchronization state is followed by a
// complete re-read, a replay: each gives the destination a new identity.
func TestSyncResetsStartAReplay(t *testing.T) {
	s, _ := open(t, 0)
	a := s.Selected()
	if _, err := s.Commit(ctx, "src", []model.Record{msg("m1", "a", 1)}, ptr("c1"), CommitOptions{}); err != nil {
		t.Fatal(err)
	}
	b := use(t, s, testEndpoint, "other").Configured.ID
	steps := []struct {
		name string
		dest int64
		do   func() error
	}{
		{"reset source", a, func() error { s.SelectDestination(a); return s.ResetSource(ctx, "src") }},
		{"accept rollback", a, func() error { return s.AcceptRollback(ctx, a) }},
		{"move pending", a, func() error { _, err := s.MovePending(ctx, a, b); return err }},
	}
	for _, st := range steps {
		s.SelectDestination(a)
		if _, err := s.Commit(ctx, "src", []model.Record{msg("m1", "a", 1)}, ptr("c1"), CommitOptions{Force: true}); err != nil {
			t.Fatal(err)
		}
		before, _ := s.Replay(ctx, st.dest)
		if before == "" {
			t.Fatalf("%s: no identity before", st.name)
		}
		if err := st.do(); err != nil {
			t.Fatalf("%s: %v", st.name, err)
		}
		if after, _ := s.Replay(ctx, st.dest); after == "" || after == before {
			t.Fatalf("%s must start a replay: %q, was %q", st.name, after, before)
		}
	}
	s.SelectDestination(a)
	if _, err := s.Commit(ctx, "src", []model.Record{msg("m1", "a", 1)}, nil, CommitOptions{Force: true}); err != nil {
		t.Fatal(err)
	}
	before, _ := s.Replay(ctx, a)
	if _, err := s.DiscardPending(ctx, a); err != nil {
		t.Fatal(err)
	}
	if after, _ := s.Replay(ctx, a); after == before {
		t.Fatal("discarding pending rows must start a replay")
	}
	if err := (&Store{db: s.db}).BeginReplay(ctx); !errors.Is(err, ErrNoDestination) {
		t.Fatalf("a replay needs a destination: %v", err)
	}
}

// Destinations never share insert tokens, even for the same rows.
func TestDestinationsHaveTheirOwnTokens(t *testing.T) {
	s, _ := open(t, 0)
	recs := []model.Record{msg("m1", "a", 1)}
	ta := commitOne(t, s, recs, CommitOptions{})
	use(t, s, testEndpoint, "other")
	if tb := commitOne(t, s, recs, CommitOptions{}); tb == ta {
		t.Fatal("two destinations must not share a token for the same rows")
	}
}
