package spool

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/nedanwr/vario/internal/model"
)

// queuedVersions returns the exact version of every queued row of the
// selected destination, by message id, in queue order (later rows win).
func queuedVersions(t *testing.T, s *Store) map[string]uint64 {
	t.Helper()
	out := map[string]uint64{}
	entries, err := s.Due(ctx, time.Now().Add(time.Hour), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		body, err := e.Body()
		if err != nil {
			t.Fatal(err)
		}
		sc := bufio.NewScanner(bytes.NewReader(body))
		for sc.Scan() {
			var r struct {
				MessageID string `json:"message_id"`
				Version   uint64 `json:"version"`
			}
			if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
				t.Fatal(err)
			}
			out[r.MessageID] = r.Version
		}
	}
	return out
}

func gen(t *testing.T, s *Store) uint64 {
	t.Helper()
	g, _, err := s.Generation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func TestGenerationFollowsPrivacySettings(t *testing.T) {
	s, path := open(t, 0)
	commit := func(fp, id, text string, v uint64) {
		t.Helper()
		if _, err := s.Commit(ctx, "src", []model.Record{msg(id, text, v)}, nil, CommitOptions{PolicyFingerprint: fp}); err != nil {
			t.Fatal(err)
		}
	}
	// The first commit starts generation 1, above rows of Vario versions
	// without generations (generation 0).
	commit("A", "m1", "a", 7)
	if g := gen(t, s); g != 1 {
		t.Fatalf("generation %d, want 1", g)
	}
	commit("A", "m2", "b", 8)
	if g := gen(t, s); g != 1 {
		t.Fatalf("unchanged settings must keep the generation: %d", g)
	}
	// The same source version under other settings is queued with a higher
	// version; returning to earlier settings starts another generation.
	commit("B", "m1", "", 7)
	commit("A", "m3", "c", 7)
	if g := gen(t, s); g != 3 {
		t.Fatalf("generation %d, want 3", g)
	}
	v := queuedVersions(t, s)
	want := map[string]uint64{"m1": 2<<model.GenerationShift | 7, "m2": 1<<model.GenerationShift | 8, "m3": 3<<model.GenerationShift | 7}
	for id, w := range want {
		if v[id] != w {
			t.Fatalf("%s queued with version %d, want %d (all: %v)", id, v[id], w, v)
		}
	}
	// The record passed in keeps its source version.
	r := msg("m4", "d", 9)
	commit("A", "m4", "d", 9)
	if _, err := s.Commit(ctx, "src", []model.Record{r}, nil, CommitOptions{PolicyFingerprint: "A", Force: true}); err != nil || r.Row.Env().Version != 9 {
		t.Fatalf("record version changed to %d (%v)", r.Row.Env().Version, err)
	}
	// The generation is durable.
	s.Close()
	s2, err := Open(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if g, fp, err := s2.Generation(ctx); err != nil || g != 3 || fp != "A" {
		t.Fatalf("after reopening: generation %d %q %v", g, fp, err)
	}
}

func TestGenerationLimits(t *testing.T) {
	s, _ := open(t, 0)
	if _, err := s.Commit(ctx, "src", []model.Record{msg("m1", "a", model.MaxSourceVersion+1)}, nil, CommitOptions{PolicyFingerprint: "A"}); err == nil {
		t.Fatal("a source version that overlaps the generation bits must be refused")
	}
	if err := s.SetKV(ctx, generationKey, `{"fingerprint":"A","generation":`+strconv.Itoa(model.MaxGeneration)+`}`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Commit(ctx, "src", []model.Record{msg("m1", "a", 1)}, nil, CommitOptions{PolicyFingerprint: "A"}); err != nil {
		t.Fatalf("the last generation is usable: %v", err)
	}
	if _, err := s.Commit(ctx, "src", []model.Record{msg("m1", "b", 1)}, nil, CommitOptions{PolicyFingerprint: "B"}); !errors.Is(err, ErrGenerationsExhausted) {
		t.Fatalf("expected ErrGenerationsExhausted, got %v", err)
	}
	if st, _ := s.Stats(ctx); st.Rows != 1 {
		t.Fatalf("the refused commit must queue nothing: %d rows", st.Rows)
	}
}

func TestNeedsResync(t *testing.T) {
	s, _ := open(t, 0)
	need := func(fp string) (bool, bool) {
		t.Helper()
		n, f, err := s.NeedsResync(ctx, "src", fp)
		if err != nil {
			t.Fatal(err)
		}
		return n, f
	}
	// Never re-read under generations: everything must be queued again.
	if n, f := need("A"); !n || !f {
		t.Fatalf("fresh: need %v force %v", n, f)
	}
	g, err := s.EnsureGeneration(ctx, CommitOptions{PolicyFingerprint: "A"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Commit(ctx, "src", nil, ptr("c1"), CommitOptions{PolicyFingerprint: "A", SnapshotGeneration: g}); err != nil {
		t.Fatal(err)
	}
	if n, _ := need("A"); n {
		t.Fatal("re-read complete")
	}
	if n, f := need("B"); !n || f {
		t.Fatalf("other settings: need %v force %v", n, f)
	}
	// A re-read that started before a new generation does not count.
	g2, _ := s.EnsureGeneration(ctx, CommitOptions{PolicyFingerprint: "B"})
	if _, err := s.Commit(ctx, "src", nil, ptr("c2"), CommitOptions{PolicyFingerprint: "B", SnapshotGeneration: g2 - 1}); err != nil {
		t.Fatal(err)
	}
	if n, _ := need("B"); !n {
		t.Fatal("a re-read under an earlier generation must not count")
	}
	// Commits without SnapshotGeneration (incremental changes) do not count either.
	if _, err := s.Commit(ctx, "src", nil, ptr("c3"), CommitOptions{PolicyFingerprint: "B"}); err != nil {
		t.Fatal(err)
	}
	if n, _ := need("B"); !n {
		t.Fatal("an incremental commit must not complete the re-read")
	}
	// Per destination.
	if _, err := s.Commit(ctx, "src", nil, ptr("c4"), CommitOptions{PolicyFingerprint: "B", SnapshotGeneration: g2}); err != nil {
		t.Fatal(err)
	}
	if n, _ := need("B"); n {
		t.Fatal("re-read complete")
	}
	use(t, s, testEndpoint, "other")
	if n, f := need("B"); !n || !f {
		t.Fatalf("another destination: need %v force %v", n, f)
	}
}

func TestVerifyLineage(t *testing.T) {
	verify := func(t *testing.T, s *Store, dbGen uint64) (bool, uint64) {
		t.Helper()
		raised, g, err := s.VerifyLineage(ctx, dbGen)
		if err != nil {
			t.Fatal(err)
		}
		return raised, g
	}
	fingerprints := func(t *testing.T, s *Store) int {
		t.Helper()
		var n int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM fingerprints WHERE destination_id = ?`, s.dest).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	t.Run("fresh spool, database of an earlier Vario version", func(t *testing.T) {
		s, _ := open(t, 0)
		if raised, g := verify(t, s, 0); raised || g != 0 {
			t.Fatalf("raised %v generation %d", raised, g)
		}
	})
	t.Run("fresh spool, database of a deleted spool", func(t *testing.T) {
		s, _ := open(t, 0)
		if raised, g := verify(t, s, 3); !raised || g != 4 {
			t.Fatalf("raised %v generation %d", raised, g)
		}
		// The next commit under the (unrecorded) settings starts yet another one.
		if _, err := s.Commit(ctx, "src", []model.Record{msg("m1", "a", 1)}, nil, CommitOptions{PolicyFingerprint: "A"}); err != nil {
			t.Fatal(err)
		}
		if g := gen(t, s); g != 5 {
			t.Fatalf("generation %d", g)
		}
	})
	t.Run("fresh spool that queued offline under the database's generation", func(t *testing.T) {
		// The spool's own generation 1 equals that of the database's rows,
		// which another spool wrote: their versions could tie.
		s, _ := open(t, 0)
		if _, err := s.Commit(ctx, "src", []model.Record{msg("m1", "a", 1)}, nil, CommitOptions{PolicyFingerprint: "A"}); err != nil {
			t.Fatal(err)
		}
		if raised, g := verify(t, s, 1); !raised || g != 2 || fingerprints(t, s) != 0 {
			t.Fatalf("raised %v generation %d fingerprints %d", raised, g, fingerprints(t, s))
		}
		if n, _, _ := s.NeedsResync(ctx, "src", "A"); !n {
			t.Fatal("a raised generation needs a re-read")
		}
	})
	t.Run("verified destination", func(t *testing.T) {
		s, _ := open(t, 0)
		if _, err := s.Commit(ctx, "src", []model.Record{msg("m1", "a", 1)}, nil, CommitOptions{PolicyFingerprint: "A"}); err != nil {
			t.Fatal(err)
		}
		if raised, _ := verify(t, s, 0); raised {
			t.Fatal("nothing to raise")
		}
		// The spool's own rows reached the database.
		if raised, g := verify(t, s, 1); raised || g != 1 || fingerprints(t, s) != 1 {
			t.Fatalf("own generation: raised %v generation %d", raised, g)
		}
		// Settings changed by a process that verified this destination.
		if _, err := s.Commit(ctx, "src", []model.Record{msg("m1", "", 1)}, nil, CommitOptions{PolicyFingerprint: "B", VerifiedDestination: s.dest}); err != nil {
			t.Fatal(err)
		}
		if raised, g := verify(t, s, 2); raised || g != 2 {
			t.Fatalf("own new generation: raised %v generation %d", raised, g)
		}
		// The spool was restored from an older copy: the database is ahead.
		if raised, g := verify(t, s, 5); !raised || g != 6 || fingerprints(t, s) != 0 {
			t.Fatalf("database ahead: raised %v generation %d", raised, g)
		}
	})
	t.Run("settings changed without verification", func(t *testing.T) {
		// A process that could not reach the database started a generation:
		// it cannot know whether the database already holds rows of a
		// generation with that number, so the destination is verified again
		// as if it never was.
		s, _ := open(t, 0)
		if _, err := s.Commit(ctx, "src", []model.Record{msg("m1", "a", 1)}, nil, CommitOptions{PolicyFingerprint: "A"}); err != nil {
			t.Fatal(err)
		}
		if raised, _ := verify(t, s, 0); raised {
			t.Fatal("nothing to raise")
		}
		if _, err := s.Commit(ctx, "src", []model.Record{msg("m1", "", 1)}, nil, CommitOptions{PolicyFingerprint: "B"}); err != nil {
			t.Fatal(err)
		}
		if raised, g := verify(t, s, 1); raised || g != 2 {
			t.Fatalf("database behind: raised %v generation %d", raised, g)
		}
		if _, err := s.Commit(ctx, "src", []model.Record{msg("m1", "a", 1)}, nil, CommitOptions{PolicyFingerprint: "C"}); err != nil {
			t.Fatal(err)
		}
		if raised, g := verify(t, s, 3); !raised || g != 4 {
			t.Fatalf("database at the unverified generation: raised %v generation %d", raised, g)
		}
	})
	t.Run("exhausted", func(t *testing.T) {
		s, _ := open(t, 0)
		if _, _, err := s.VerifyLineage(ctx, model.MaxGeneration); !errors.Is(err, ErrGenerationsExhausted) {
			t.Fatalf("expected ErrGenerationsExhausted, got %v", err)
		}
	})
}

// fingerprintCount is the number of fingerprints of destination dest.
func fingerprintCount(t *testing.T, s *Store, dest int64) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM fingerprints WHERE destination_id = ?`, dest).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// syncedUnder completes a re-read of "src" for the selected destination
// under fingerprint fp, as the final commit of a snapshot does.
func syncedUnder(t *testing.T, s *Store, fp string, force bool) {
	t.Helper()
	g, err := s.EnsureGeneration(ctx, CommitOptions{PolicyFingerprint: fp})
	if err != nil {
		t.Fatal(err)
	}
	cur := "c"
	if _, err := s.Commit(ctx, "src", []model.Record{msg("m1", "", 1)}, &cur, CommitOptions{PolicyFingerprint: fp, Force: force, SnapshotGeneration: g}); err != nil {
		t.Fatal(err)
	}
}

// Moving queued rows into a synchronized destination resets it: rows of an
// earlier generation could otherwise become the latest of records its
// fingerprints keep the next re-read from queueing again.
func TestMovePendingResetsTarget(t *testing.T) {
	s, _ := open(t, 0)
	b := s.dest
	syncedUnder(t, s, "metrics", false) // generation 1
	if _, _, err := s.VerifyLineage(ctx, 0); err != nil {
		t.Fatal(err)
	}
	a := use(t, s, testEndpoint, "other").Configured.ID
	if _, err := s.Commit(ctx, "src", []model.Record{msg("m1", "secret", 1)}, nil, CommitOptions{PolicyFingerprint: "full"}); err != nil {
		t.Fatal(err)
	}
	use(t, s, testEndpoint, "vario")
	syncedUnder(t, s, "metrics", false) // generation 3; its fingerprint is unchanged, so m1 is skipped
	if need, _, _ := s.NeedsResync(ctx, "src", "metrics"); need {
		t.Fatal("B is synchronized")
	}
	replay, _ := s.Replay(ctx, b)
	cursor, _, _ := s.Cursor(ctx, "src")

	if n, err := s.MovePending(ctx, a, b); err != nil || n != 1 {
		t.Fatalf("move: %d %v", n, err)
	}
	if need, force, _ := s.NeedsResync(ctx, "src", "metrics"); !need || !force {
		t.Fatalf("B needs a forced re-read: %v %v", need, force)
	}
	if fingerprintCount(t, s, b) != 0 {
		t.Fatal("B's fingerprints must be deleted")
	}
	if cur, ok, _ := s.Cursor(ctx, "src"); !ok || cur != cursor {
		t.Fatal("B keeps its cursor")
	}
	if r, _ := s.Replay(ctx, b); r == replay {
		t.Fatal("B's re-read is a replay and needs a new replay identity")
	}
	// The forced re-read queues m1 again under generation 3, above the moved row.
	syncedUnder(t, s, "metrics", true)
	vs := queuedVersions(t, s)
	if model.Generation(vs["m1"]) != 3 {
		t.Fatalf("latest queued m1 of generation %d", model.Generation(vs["m1"]))
	}
	if need, _, _ := s.NeedsResync(ctx, "src", "metrics"); need {
		t.Fatal("the forced re-read completes it")
	}

	// Nothing to move: B is left alone.
	if n, err := s.MovePending(ctx, a, b); err != nil || n != 0 {
		t.Fatalf("empty move: %d %v", n, err)
	}
	if need, _, _ := s.NeedsResync(ctx, "src", "metrics"); need {
		t.Fatal("an empty move must not reset B")
	}
}

// A re-read that was running when its destination was reset (and may have
// skipped records by the deleted fingerprints) does not complete it; only a
// forced one does.
func TestResetDuringRereadNeedsForcedReread(t *testing.T) {
	s, _ := open(t, 0)
	syncedUnder(t, s, "A", false)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := requireResync(ctx, tx, s.dest); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	syncedUnder(t, s, "A", false)
	if need, force, _ := s.NeedsResync(ctx, "src", "A"); !need || !force {
		t.Fatalf("a non-forced re-read must not complete the reset: %v %v", need, force)
	}
	syncedUnder(t, s, "A", true)
	if need, _, _ := s.NeedsResync(ctx, "src", "A"); need {
		t.Fatal("a forced re-read completes it")
	}
}

// F2: when one destination's check starts a generation (the local state was
// restored from an older copy), the other destinations' verifications are
// withdrawn, and a destination checked without a verification is reset.
func TestVerifyLineageAcrossDestinations(t *testing.T) {
	setup := func(t *testing.T) (s *Store, a, b int64) {
		s, _ = open(t, 0)
		a = s.dest
		syncedUnder(t, s, "metrics", false)
		if _, _, err := s.VerifyLineage(ctx, 0); err != nil {
			t.Fatal(err)
		}
		b = use(t, s, testEndpoint, "b").Configured.ID
		syncedUnder(t, s, "metrics", false)
		if _, _, err := s.VerifyLineage(ctx, 0); err != nil {
			t.Fatal(err)
		}
		use(t, s, testEndpoint, "vario")
		return s, a, b
	}
	t.Run("tie", func(t *testing.T) {
		s, _, b := setup(t)
		// A's database holds generation 2 of the lost history; B's holds 3.
		if raised, g, err := s.VerifyLineage(ctx, 2); err != nil || !raised || g != 3 {
			t.Fatalf("A: %v %d %v", raised, g, err)
		}
		s.SelectDestination(b)
		if raised, g, err := s.VerifyLineage(ctx, 3); err != nil || !raised || g != 4 {
			t.Fatalf("B's verification is stale: the tie must start a generation: %v %d %v", raised, g, err)
		}
		if fingerprintCount(t, s, b) != 0 {
			t.Fatal("B's fingerprints must be deleted")
		}
	})
	t.Run("gap", func(t *testing.T) {
		s, _, b := setup(t)
		if raised, g, err := s.VerifyLineage(ctx, 3); err != nil || !raised || g != 4 {
			t.Fatalf("A: %v %d %v", raised, g, err)
		}
		s.SelectDestination(b)
		// B's database holds generation 2 of the lost history, below the spool's 4.
		if raised, g, err := s.VerifyLineage(ctx, 2); err != nil || raised || g != 4 {
			t.Fatalf("B: %v %d %v", raised, g, err)
		}
		if fingerprintCount(t, s, b) != 0 {
			t.Fatal("B's fingerprints must be deleted")
		}
		if need, force, _ := s.NeedsResync(ctx, "src", "metrics"); !need || !force {
			t.Fatalf("B needs a forced re-read: %v %v", need, force)
		}
		// Verified now: a later check of B keeps its state.
		syncedUnder(t, s, "metrics", true)
		if _, _, err := s.VerifyLineage(ctx, 4); err != nil {
			t.Fatal(err)
		}
		if need, _, _ := s.NeedsResync(ctx, "src", "metrics"); need || fingerprintCount(t, s, b) == 0 {
			t.Fatal("a verified destination keeps its fingerprints and re-read")
		}
	})
	t.Run("own generation", func(t *testing.T) {
		// Without a restore nothing changes for a verified destination.
		s, a, b := setup(t)
		if raised, _, err := s.VerifyLineage(ctx, 1); err != nil || raised || fingerprintCount(t, s, a) == 0 {
			t.Fatalf("A: %v %v", raised, err)
		}
		s.SelectDestination(b)
		if raised, _, err := s.VerifyLineage(ctx, 1); err != nil || raised || fingerprintCount(t, s, b) == 0 {
			t.Fatalf("B: %v %v", raised, err)
		}
	})
}
