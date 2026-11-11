package spool

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nedanwr/vario/internal/model"
)

func queuedIDs(t *testing.T, s *Store) []int64 {
	t.Helper()
	entries, err := s.Due(ctx, time.Now().Add(time.Hour), 1000)
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]int64, len(entries))
	for i, e := range entries {
		ids[i] = e.ID
	}
	return ids
}

// The confirmed checkpoint and the removal of the entries it covers are one
// transaction, the checkpoint only ever advances, and the state survives a
// restart.
func TestCheckpointAdvancesWithAcknowledgedEntriesOnly(t *testing.T) {
	s, path := open(t, 0)
	dest := s.Selected()

	// No state yet: reading creates nothing.
	if cp, err := s.Checkpoint(ctx, dest); err != nil || cp != (Checkpoint{}) {
		t.Fatalf("fresh destination: %+v %v", cp, err)
	}
	if err := s.AckCheckpoint(ctx, dest, nil, 1); err == nil {
		t.Fatal("a checkpoint cannot be confirmed for a destination without a writer")
	}
	cp, err := s.CheckpointWriter(ctx, dest)
	if err != nil || len(cp.Writer) != 32 || cp.Confirmed != 0 {
		t.Fatalf("writer: %+v %v", cp, err)
	}
	if again, _ := s.CheckpointWriter(ctx, dest); again.Writer != cp.Writer {
		t.Fatalf("the writer id must be stable: %s then %s", cp.Writer, again.Writer)
	}

	if _, err := s.Commit(ctx, "src", []model.Record{msg("m1", "a", 1), msg("m2", "b", 1)}, ptr("c1"), CommitOptions{MaxRowsPerEntry: 1}); err != nil {
		t.Fatal(err)
	}
	ids := queuedIDs(t, s)
	if len(ids) != 2 {
		t.Fatalf("queued entries: %v", ids)
	}
	// One entry is covered by checkpoint 1; the other stays queued.
	if err := s.AckCheckpoint(ctx, dest, ids[:1], 1); err != nil {
		t.Fatal(err)
	}
	if cp, _ = s.Checkpoint(ctx, dest); cp.Confirmed != 1 || cp.At.IsZero() {
		t.Fatalf("confirmed: %+v", cp)
	}
	if left := queuedIDs(t, s); len(left) != 1 || left[0] != ids[1] {
		t.Fatalf("only the covered entry leaves the queue: %v", left)
	}
	// A checkpoint that does not advance is refused, and then nothing is removed.
	for _, seq := range []uint64{0, 1} {
		if err := s.AckCheckpoint(ctx, dest, ids[1:], seq); err == nil || !strings.Contains(err.Error(), "does not advance") {
			t.Fatalf("checkpoint %d must be refused: %v", seq, err)
		}
	}
	if left := queuedIDs(t, s); len(left) != 1 {
		t.Fatalf("a refused checkpoint must not remove entries: %v", left)
	}
	// Numbers may skip (an earlier write reached the database unnoticed).
	if err := s.AckCheckpoint(ctx, dest, ids[1:], 5); err != nil {
		t.Fatal(err)
	}

	// Entries of another destination are never removed by this one's checkpoint.
	other := use(t, s, "http://other:8123", "vario").Configured.ID
	if _, err := s.Commit(ctx, "src", []model.Record{msg("m3", "c", 1)}, nil, CommitOptions{}); err != nil {
		t.Fatal(err)
	}
	foreign := queuedIDs(t, s)
	if err := s.AckCheckpoint(ctx, dest, foreign, 6); err != nil {
		t.Fatal(err)
	}
	if left := queuedIDs(t, s); len(left) != 1 {
		t.Fatalf("an entry of another destination was removed: %v", left)
	}
	if cp, _ := s.Checkpoint(ctx, other); cp.Writer != "" {
		t.Fatalf("checkpoint state is per destination: %+v", cp)
	}

	// The state is durable and readable without write access.
	s.Close()
	ro, err := OpenReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	if got, err := ro.Checkpoint(ctx, dest); err != nil || got.Confirmed != 6 || got.Writer != cp.Writer {
		t.Fatalf("after reopening: %+v %v", got, err)
	}
}

// A detected rollback is remembered with the time of its first detection,
// applies while the destination still has the checkpoint it was detected
// against, and is resolved by accepting it: the synchronization state is
// reset, queued rows are kept, and a new checkpoint history starts.
func TestRollbackIsRememberedUntilAccepted(t *testing.T) {
	s, _ := open(t, 0)
	dest := s.Selected()
	if _, err := s.Commit(ctx, "src", []model.Record{msg("m1", "a", 1)}, ptr("c1"), CommitOptions{}); err != nil {
		t.Fatal(err)
	}
	cp, _ := s.CheckpointWriter(ctx, dest)
	if err := s.AckCheckpoint(ctx, dest, queuedIDs(t, s), 4); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := s.RolledBack(ctx); ok || err != nil {
		t.Fatalf("no rollback yet: %v %v", ok, err)
	}
	// A row that is still queued when the rollback is found.
	if _, err := s.Commit(ctx, "src", []model.Record{msg("m2", "b", 1)}, ptr("c2"), CommitOptions{}); err != nil {
		t.Fatal(err)
	}

	if err := s.MarkRolledBack(ctx, dest, 2); err != nil {
		t.Fatal(err)
	}
	rb, ok, err := s.RolledBack(ctx)
	if err != nil || !ok || rb.Destination.ID != dest || rb.Confirmed != 4 || rb.Found != 2 || rb.Destination.Pending != 1 {
		t.Fatalf("rollback: %+v %v %v", rb, ok, err)
	}
	first := rb.At
	time.Sleep(5 * time.Millisecond)
	if err := s.MarkRolledBack(ctx, dest, 2); err != nil {
		t.Fatal(err)
	}
	if rb, _, _ = s.RolledBack(ctx); !rb.At.Equal(first) {
		t.Fatalf("the time of the first detection must be kept: %s then %s", first, rb.At)
	}
	// Marking changes nothing else.
	if c, ok, _ := s.Cursor(ctx, "src"); !ok || c != "c2" {
		t.Fatalf("cursor: %q %v", c, ok)
	}
	if got, _ := s.Checkpoint(ctx, dest); got.Confirmed != 4 || got.Writer != cp.Writer {
		t.Fatalf("the confirmed checkpoint must be kept: %+v", got)
	}

	// Withdrawn (the database holds the checkpoint again) and found again.
	if err := s.ClearRolledBack(ctx); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.RolledBack(ctx); ok {
		t.Fatal("a cleared rollback must not be reported")
	}
	if err := s.MarkRolledBack(ctx, dest, 0); err != nil {
		t.Fatal(err)
	}
	if rb, ok, _ = s.RolledBack(ctx); !ok || rb.Found != 0 {
		t.Fatalf("rollback to a state without a checkpoint: %+v %v", rb, ok)
	}

	// Accepted: no cursor, no fingerprints, a new writer, the queue untouched.
	if err := s.AcceptRollback(ctx, dest); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.RolledBack(ctx); ok {
		t.Fatal("an accepted rollback must not be reported")
	}
	if _, ok, _ := s.Cursor(ctx, "src"); ok {
		t.Fatal("the cursor must be reset so that the source is read again in full")
	}
	if res, err := s.Commit(ctx, "src", []model.Record{msg("m1", "a", 1)}, nil, CommitOptions{}); err != nil || res.Enqueued != 1 {
		t.Fatalf("an unchanged record must be queued again after the reset: %+v %v", res, err)
	}
	if d, _, _ := s.Destination(ctx, dest); d.Pending != 2 || !d.Retired.IsZero() {
		t.Fatalf("queued rows must be kept for the same destination: %+v", d)
	}
	if got, _ := s.Checkpoint(ctx, dest); got != (Checkpoint{}) {
		t.Fatalf("the abandoned checkpoint history must be discarded: %+v", got)
	}
	next, err := s.CheckpointWriter(ctx, dest)
	if err != nil || next.Writer == cp.Writer || next.Confirmed != 0 {
		t.Fatalf("a new history needs a new writer: %+v %v", next, err)
	}
	// Repeating it changes nothing more.
	if err := s.AcceptRollback(ctx, dest); err != nil {
		t.Fatal(err)
	}
	if d, _, _ := s.Destination(ctx, dest); d.Pending != 2 {
		t.Fatalf("repeated acceptance: %+v", d)
	}
}

// A rollback report does not outlive the destination it is about: once the
// database is recorded as replaced (a new destination), it no longer applies,
// and a retired destination cannot be accepted.
func TestRollbackDoesNotApplyToAReplacedDestination(t *testing.T) {
	s, _ := open(t, 0)
	dest := s.Selected()
	if _, err := s.CheckpointWriter(ctx, dest); err != nil {
		t.Fatal(err)
	}
	if err := s.AckCheckpoint(ctx, dest, nil, 3); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkRolledBack(ctx, dest, 1); err != nil {
		t.Fatal(err)
	}
	nd, err := s.Replace(ctx, dest, "id-new")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.RolledBack(ctx); ok {
		t.Fatal("a retired destination must not be reported as rolled back")
	}
	if cp, _ := s.Checkpoint(ctx, nd.ID); cp != (Checkpoint{}) {
		t.Fatalf("a new destination starts without a checkpoint: %+v", cp)
	}
	if err := s.AcceptRollback(ctx, dest); err == nil {
		t.Fatal("a retired destination cannot be accepted")
	}
}

// Spools of earlier versions carry no checkpoint: opening one read-only
// reports none, and upgrading keeps it that way until a collector writes one.
func TestLegacySpoolHasNoCheckpoint(t *testing.T) {
	path := legacySpool(t)
	ro, err := OpenReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	if cp, err := ro.Checkpoint(ctx, 1); err != nil || cp != (Checkpoint{}) {
		t.Fatalf("legacy spool: %+v %v", cp, err)
	}
	if _, ok, err := ro.RolledBack(ctx); ok || err != nil {
		t.Fatalf("legacy spool: %v %v", ok, err)
	}
	ro.Close()

	s, err := Open(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	res := use(t, s, testEndpoint, "vario")
	if !res.Adopted {
		t.Fatalf("legacy state must be adopted: %+v", res)
	}
	if cp, err := s.Checkpoint(ctx, res.Configured.ID); err != nil || cp != (Checkpoint{}) {
		t.Fatalf("upgraded spool: %+v %v", cp, err)
	}
	// The upgraded queue and cursor are intact and usable with checkpoints.
	if _, err := s.CheckpointWriter(ctx, res.Configured.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.AckCheckpoint(ctx, res.Configured.ID, queuedIDs(t, s), 1); err != nil {
		t.Fatal(err)
	}
	if d, _, _ := s.Destination(ctx, res.Configured.ID); d.Pending != 0 || d.Cursors != 1 {
		t.Fatalf("after the first checkpoint: %+v", d)
	}
}

// The checkpoint state lives in the kv table only: a spool written by this
// version has the tables and schema version of the previous one.
func TestCheckpointNeedsNoSpoolSchemaChange(t *testing.T) {
	s, path := open(t, 0)
	if _, err := s.CheckpointWriter(ctx, s.Selected()); err != nil {
		t.Fatal(err)
	}
	if err := s.AckCheckpoint(ctx, s.Selected(), nil, 1); err != nil {
		t.Fatal(err)
	}
	s.Close()
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var version, tables int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != 1 {
		t.Fatalf("spool schema version: %d %v", version, err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%'`).Scan(&tables); err != nil || tables != 5 {
		t.Fatalf("tables: %d %v", tables, err)
	}
}
