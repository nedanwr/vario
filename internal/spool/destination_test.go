package spool

import (
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/nedanwr/vario/internal/model"
)

// legacySchema is the spool schema of Vario 0.1 (spool version 0).
const legacySchema = `
CREATE TABLE queue (
  id INTEGER PRIMARY KEY AUTOINCREMENT, dest_table TEXT NOT NULL, token TEXT NOT NULL,
  row_count INTEGER NOT NULL, raw_bytes INTEGER NOT NULL, body BLOB NOT NULL, created_at INTEGER NOT NULL,
  attempts INTEGER NOT NULL DEFAULT 0, next_attempt_at INTEGER NOT NULL DEFAULT 0, last_error TEXT NOT NULL DEFAULT '');
CREATE INDEX queue_due ON queue(next_attempt_at, id);
CREATE TABLE cursors (source_id TEXT PRIMARY KEY, cursor TEXT NOT NULL, updated_at INTEGER NOT NULL);
CREATE TABLE fingerprints (source_id TEXT NOT NULL, record_key TEXT NOT NULL, hash TEXT NOT NULL,
  PRIMARY KEY (source_id, record_key)) WITHOUT ROWID;
CREATE TABLE kv (key TEXT PRIMARY KEY, value TEXT NOT NULL, updated_at INTEGER NOT NULL);
`

// legacySpool writes a version 0 spool with one queued entry, a cursor and
// a fingerprint, as Vario 0.1 would have left them.
func legacySpool(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state.db")
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, q := range []string{legacySchema,
		`INSERT INTO queue (dest_table, token, row_count, raw_bytes, body, created_at) VALUES ('messages', 'tok', 3, 10, x'1f8b', 1)`,
		`INSERT INTO cursors VALUES ('src', 'legacy-cursor', 1)`,
		`INSERT INTO fingerprints VALUES ('src', 'messages/m1', 'h1')`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	return path
}

func TestLegacySpoolIsUpgradedWithoutLoss(t *testing.T) {
	path := legacySpool(t)

	// Read-only inspection works before the upgrade and changes nothing.
	ro, err := OpenReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	if !ro.Legacy() {
		t.Fatal("expected a legacy spool")
	}
	if cur, ok, _ := ro.Cursor(ctx, "src"); !ok || cur != "legacy-cursor" {
		t.Fatalf("legacy cursor: %q %v", cur, ok)
	}
	if st, err := ro.Stats(ctx); err != nil || st.Rows != 3 {
		t.Fatalf("legacy stats: %+v %v", st, err)
	}
	if d, ok, _ := ro.Active(ctx); !ok || !d.Legacy() || d.Pending != 3 {
		t.Fatalf("legacy active: %+v %v", d, ok)
	}
	ro.Close()

	s, err := Open(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ds, err := s.Destinations(ctx)
	if err != nil || len(ds) != 1 || !ds[0].Legacy() || ds[0].Pending != 3 || ds[0].Cursors != 1 {
		t.Fatalf("legacy state must be kept in a legacy destination: %+v %v", ds, err)
	}
	// The first resolution adopts the legacy state for the configured destination.
	res, err := s.Resolve(ctx, testEndpoint, "vario", "")
	if err != nil || !res.Adopted || res.Changed() || res.Configured.Pending != 3 {
		t.Fatalf("adoption: %+v %v", res, err)
	}
	s.SelectDestination(res.Configured.ID)
	if cur, ok, _ := s.Cursor(ctx, "src"); !ok || cur != "legacy-cursor" {
		t.Fatalf("the cursor must survive the upgrade: %q %v", cur, ok)
	}
	if got, _ := s.Commit(ctx, "src", nil, nil, CommitOptions{}); got.Enqueued != 0 {
		t.Fatal("unexpected commit result")
	}
	var fp string
	if err := s.db.QueryRow(`SELECT hash FROM fingerprints WHERE destination_id = ? AND record_key = 'messages/m1'`, res.Configured.ID).Scan(&fp); err != nil || fp != "h1" {
		t.Fatalf("fingerprints must survive the upgrade: %q %v", fp, err)
	}
	// Reopening does not upgrade again.
	s.Close()
	if s, err = Open(path, 0); err != nil {
		t.Fatal(err)
	}
	if ds, _ := s.Destinations(ctx); len(ds) != 1 {
		t.Fatalf("repeat open changed destinations: %+v", ds)
	}
}

func TestLegacySpoolLabelledBeforeDestinationChange(t *testing.T) {
	path := legacySpool(t)
	s, err := Open(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	// The configuration changes before the collector ran with this version:
	// the legacy state is bound to the previous destination.
	if err := s.LabelLegacy(ctx, testEndpoint, "old_db"); err != nil {
		t.Fatal(err)
	}
	res, err := s.Resolve(ctx, testEndpoint, "new_db", "")
	if err != nil || res.Adopted || !res.Changed() || res.Previous.Pending != 3 || res.Previous.Database != "old_db" {
		t.Fatalf("the legacy state must stay with the previous destination: %+v %v", res, err)
	}
}

func TestUpgradeRefusedWhileOldCollectorRuns(t *testing.T) {
	path := legacySpool(t)
	db, _ := sql.Open("sqlite", "file:"+filepath.ToSlash(path))
	host, _ := os.Hostname()
	b, _ := json.Marshal(Lease{Owner: "old", Host: host, PID: os.Getppid(), Command: "run", Expires: time.Now().Add(time.Minute)})
	if _, err := db.Exec(`INSERT INTO kv VALUES (?, ?, 1)`, leaseKey, string(b)); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if _, err := Open(path, 0); !errors.Is(err, ErrUpgradeBlocked) {
		t.Fatalf("expected ErrUpgradeBlocked, got %v", err)
	}
	ro, err := OpenReadOnly(path)
	if err != nil || !ro.Legacy() {
		t.Fatalf("the spool must be left as it was: %v", err)
	}
	ro.Close()
}

func TestPendingIsBoundToDestination(t *testing.T) {
	s, _ := open(t, 0)
	a := s.Selected()
	if _, err := s.Commit(ctx, "src", []model.Record{msg("m1", "a", 1), msg("m2", "b", 1)}, ptr("ca"), CommitOptions{}); err != nil {
		t.Fatal(err)
	}
	res := use(t, s, testEndpoint, "other")
	b := res.Configured.ID
	if res.Previous == nil || res.Previous.ID != a || res.Previous.Pending != 2 {
		t.Fatalf("the change must report the previous destination's queue: %+v", res)
	}
	if due, _ := s.Due(ctx, time.Now(), 100); len(due) != 0 {
		t.Fatalf("entries of another destination must not be due: %+v", due)
	}
	if _, ok, _ := s.Cursor(ctx, "src"); ok {
		t.Fatal("cursors are per destination")
	}
	// Identical content is queued again for the new destination.
	if r, _ := s.Commit(ctx, "src", []model.Record{msg("m1", "a", 1)}, nil, CommitOptions{}); r.Enqueued != 1 {
		t.Fatalf("fingerprints are per destination: %+v", r)
	}
	st, _ := s.Stats(ctx)
	if st.Rows != 1 || st.OtherRows != 2 {
		t.Fatalf("stats by destination: %+v", st)
	}
	// Explicit move and discard.
	if n, err := s.MovePending(ctx, a, b); err != nil || n != 2 {
		t.Fatalf("move: %d %v", n, err)
	}
	if due, _ := s.Due(ctx, time.Now(), 100); len(due) != 2 {
		t.Fatalf("moved entries are due for the new destination: %+v", due)
	}
	if n, err := s.DiscardPending(ctx, b); err != nil || n != 3 {
		t.Fatalf("discard: %d %v", n, err)
	}
	// The previous destination keeps its cursor for switching back.
	use(t, s, testEndpoint, "vario")
	if cur, ok, _ := s.Cursor(ctx, "src"); !ok || cur != "ca" {
		t.Fatalf("switching back restores the cursor: %q %v", cur, ok)
	}
}

func TestResolveByServerID(t *testing.T) {
	s, _ := open(t, 0)
	first := s.Selected()
	if err := s.BindServerID(ctx, first, "id-1"); err != nil {
		t.Fatal(err)
	}
	if err := s.BindServerID(ctx, first, "id-2"); !errors.Is(err, ErrIdentityMismatch) {
		t.Fatalf("a recorded destination id must not change: %v", err)
	}
	// Same database, new endpoint: recognized and re-keyed.
	res, err := s.Resolve(ctx, "https://ch.example.com:443", "vario", "id-1")
	if err != nil || !res.Aliased || res.Configured.ID != first || res.Changed() {
		t.Fatalf("alias: %+v %v", res, err)
	}
	// Same endpoint, recreated database: reported, nothing changed.
	res, err = s.Resolve(ctx, "https://ch.example.com:443", "vario", "id-9")
	if err != nil || !res.Replaced || res.Configured.ID != first {
		t.Fatalf("replaced: %+v %v", res, err)
	}
	nd, err := s.Replace(ctx, first, "id-9")
	if err != nil || nd.ID == first || nd.ServerID != "id-9" {
		t.Fatalf("replace: %+v %v", nd, err)
	}
	res, err = s.Resolve(ctx, "https://ch.example.com:443", "vario", "id-9")
	if err != nil || res.Replaced || res.Configured.ID != nd.ID {
		t.Fatalf("after replace: %+v %v", res, err)
	}
	old, _, _ := s.Destination(ctx, first)
	if old.Retired.IsZero() {
		t.Fatal("the replaced destination must be retired")
	}
}
