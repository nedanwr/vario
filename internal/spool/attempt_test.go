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

func due(t *testing.T, s *Store) []Entry {
	t.Helper()
	es, err := s.Due(ctx, time.Now().Add(time.Hour), 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	return es
}

func sentFlags(t *testing.T, s *Store) map[int64]bool {
	t.Helper()
	rows, err := s.db.Query(`SELECT id, sent FROM queue`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[int64]bool{}
	for rows.Next() {
		var id int64
		var sent bool
		if err := rows.Scan(&id, &sent); err != nil {
			t.Fatal(err)
		}
		out[id] = sent
	}
	return out
}

// The first attempt of an entry carries the token it was queued with; every
// later attempt carries a new one, which is stored before it is returned and
// therefore survives a restart. Tokens are never reused.
func TestBeginAttemptRenewsTokensOfAttemptedEntries(t *testing.T) {
	s, path := open(t, 0)
	if _, err := s.Commit(ctx, "src", []model.Record{msg("m1", "a", 1), msg("m2", "b", 1)}, nil, CommitOptions{MaxRowsPerEntry: 1}); err != nil {
		t.Fatal(err)
	}
	queued := due(t, s)
	if len(queued) != 2 {
		t.Fatalf("queued: %+v", queued)
	}
	for id, sent := range sentFlags(t, s) {
		if sent {
			t.Fatalf("entry %d is marked as sent before any attempt", id)
		}
	}

	first, renewed, err := s.BeginAttempt(ctx, queued)
	if err != nil || renewed != 0 {
		t.Fatalf("first attempt: %d renewed, %v", renewed, err)
	}
	seen := map[string]bool{}
	for i, e := range first {
		if e.ID != queued[i].ID || e.Token != queued[i].Token {
			t.Fatalf("the first attempt must carry the queued token: %+v, queued %+v", e, queued[i])
		}
		seen[e.Token] = true
	}
	for id, sent := range sentFlags(t, s) {
		if !sent {
			t.Fatalf("entry %d is not marked as sent after BeginAttempt", id)
		}
	}

	// Only the first entry is attempted again (as after a failed checkpoint).
	second, renewed, err := s.BeginAttempt(ctx, queued[:1])
	if err != nil || renewed != 1 || len(second) != 1 {
		t.Fatalf("second attempt: %+v %d %v", second, renewed, err)
	}
	if seen[second[0].Token] || len(second[0].Token) != len(queued[0].Token) {
		t.Fatalf("a retry must carry a new token of the same format: %q", second[0].Token)
	}
	seen[second[0].Token] = true
	if got := due(t, s); got[0].Token != second[0].Token || got[1].Token != queued[1].Token {
		t.Fatalf("the new token must be stored, the untouched entry keeps its own: %+v", got)
	}

	// A restart: the stored token is the one the last attempt carried, and the
	// next attempt after the restart gets yet another one.
	s.Close()
	s, err = Open(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	use(t, s, testEndpoint, "vario")
	reopened := due(t, s)
	if reopened[0].Token != second[0].Token || reopened[1].Token != queued[1].Token {
		t.Fatalf("tokens after the restart: %+v", reopened)
	}
	third, renewed, err := s.BeginAttempt(ctx, reopened)
	if err != nil || renewed != 2 {
		t.Fatalf("attempt after the restart: %d renewed, %v", renewed, err)
	}
	for _, e := range third {
		if seen[e.Token] {
			t.Fatalf("token %q was used by an earlier attempt", e.Token)
		}
		seen[e.Token] = true
	}
	// Rows and order are untouched.
	for i, e := range third {
		if e.ID != reopened[i].ID || e.Rows != reopened[i].Rows || e.Table != reopened[i].Table {
			t.Fatalf("entry changed: %+v, was %+v", e, reopened[i])
		}
	}
}

// An entry that left the queue (acknowledged, moved, discarded) is never sent.
func TestBeginAttemptRefusesEntriesNoLongerQueued(t *testing.T) {
	s, _ := open(t, 0)
	if _, err := s.Commit(ctx, "src", []model.Record{msg("m1", "a", 1)}, nil, CommitOptions{}); err != nil {
		t.Fatal(err)
	}
	queued := due(t, s)
	if err := s.Ack(ctx, []int64{queued[0].ID}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.BeginAttempt(ctx, queued); !errors.Is(err, ErrEntryGone) {
		t.Fatalf("expected ErrEntryGone, got %v", err)
	}
	// Nor is an entry of another destination.
	if _, err := s.Commit(ctx, "src", []model.Record{msg("m2", "b", 1)}, nil, CommitOptions{}); err != nil {
		t.Fatal(err)
	}
	queued = due(t, s)
	use(t, s, testEndpoint, "other")
	if _, _, err := s.BeginAttempt(ctx, queued); !errors.Is(err, ErrEntryGone) {
		t.Fatalf("an entry of another destination: %v", err)
	}
}

// v1Spool writes a spool of schema version 1 with one destination and two
// queued entries, as Vario wrote it before queue.sent existed.
func v1Spool(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state.db")
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, q := range []string{
		`PRAGMA journal_mode = WAL`, // as every spool Open wrote
		schemaV1,
		`INSERT INTO destinations (endpoint, database, created_at, updated_at) VALUES ('` + testEndpoint + `', 'vario', 1, 1)`,
		`INSERT INTO queue (destination_id, dest_table, token, row_count, raw_bytes, body, created_at) VALUES (1, 'messages', 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa', 1, 1, x'00', 1)`,
		`INSERT INTO queue (destination_id, dest_table, token, row_count, raw_bytes, body, created_at, attempts) VALUES (1, 'messages', 'bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb', 1, 1, x'00', 1, 3)`,
		`PRAGMA user_version = 1`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	return path
}

// Entries queued by version 1 may have been inserted already, with a
// checkpoint that was never confirmed: the upgrade marks every one of them
// as sent, so that its next attempt carries a new token.
func TestUpgradeFromVersion1MarksQueuedEntriesSent(t *testing.T) {
	path := v1Spool(t)
	s, err := Open(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var version int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != schemaVersion {
		t.Fatalf("version %d %v", version, err)
	}
	flags := sentFlags(t, s)
	if len(flags) != 2 || !flags[1] || !flags[2] {
		t.Fatalf("every entry queued before the upgrade must be marked as sent: %v", flags)
	}
	s.SelectDestination(1)
	queued := due(t, s)
	got, renewed, err := s.BeginAttempt(ctx, queued)
	if err != nil || renewed != 2 {
		t.Fatalf("first attempt after the upgrade: %d renewed, %v", renewed, err)
	}
	for i, e := range got {
		if e.Token == queued[i].Token {
			t.Fatalf("entry %d is retried under its pre-upgrade token", e.ID)
		}
	}
	// New entries start unsent; reopening does not upgrade again.
	if _, err := s.Commit(ctx, "src", []model.Record{msg("m3", "c", 1)}, nil, CommitOptions{}); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if s, err = Open(path, 0); err != nil {
		t.Fatal(err)
	}
	if flags := sentFlags(t, s); len(flags) != 3 || flags[3] {
		t.Fatalf("after reopening: %v", flags)
	}
}

// A collector of version 1 that still runs would send entries without
// recording it: the upgrade waits until it is gone.
func TestUpgradeFromVersion1RefusedWhileOldCollectorRuns(t *testing.T) {
	path := v1Spool(t)
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
	db, _ = sql.Open("sqlite", "file:"+filepath.ToSlash(path))
	defer db.Close()
	var version int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != 1 {
		t.Fatalf("the spool must be left as it was: %d %v", version, err)
	}
}

// Concurrent opens of an old spool upgrade it once.
func TestConcurrentUpgradeFromVersion1(t *testing.T) {
	path := v1Spool(t)
	errs := make(chan error, 4)
	for i := 0; i < 4; i++ {
		go func() {
			s, err := Open(path, 0)
			if err == nil {
				s.Close()
			}
			errs <- err
		}()
	}
	for i := 0; i < 4; i++ {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
}
