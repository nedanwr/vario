// Package spool is Vario's local durable state: the outbound delivery queue,
// source cursors, record fingerprints and the single-collector lease.
//
// Records and the cursor that produced them are committed in one SQLite
// transaction, so a cursor never advances past data that is not durably queued.
package spool

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"time"

	_ "modernc.org/sqlite"

	"github.com/nedanwr/vario/internal/model"
)

// ErrFull is returned when the queue has reached its configured size limit.
var ErrFull = errors.New("spool is full; waiting for delivery to drain it")

// ErrLeaseHeld is returned when another Vario process owns the collector lease.
var ErrLeaseHeld = errors.New("another vario collector is running")

// Spool schema versions (PRAGMA user_version):
//
//	0: queue, cursors and fingerprints without a destination (Vario <= 0.1).
//	1: every queue entry, cursor and fingerprint belongs to a destination.
//	2: queue entries record whether an insert carrying their current token
//	   may have reached ClickHouse (queue.sent, see BeginAttempt).
const schemaVersion = 2

const schemaV1 = `
CREATE TABLE IF NOT EXISTS destinations (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  endpoint TEXT NOT NULL,
  database TEXT NOT NULL,
  server_id TEXT NOT NULL DEFAULT '',
  retired_at INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS destinations_key ON destinations(endpoint, database) WHERE retired_at = 0;
CREATE TABLE IF NOT EXISTS queue (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  dest_table TEXT NOT NULL,
  token TEXT NOT NULL,
  row_count INTEGER NOT NULL,
  raw_bytes INTEGER NOT NULL,
  body BLOB NOT NULL,
  created_at INTEGER NOT NULL,
  attempts INTEGER NOT NULL DEFAULT 0,
  next_attempt_at INTEGER NOT NULL DEFAULT 0,
  last_error TEXT NOT NULL DEFAULT '',
  destination_id INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS queue_due ON queue(next_attempt_at, id);
CREATE INDEX IF NOT EXISTS queue_destination ON queue(destination_id, next_attempt_at, id);
CREATE TABLE IF NOT EXISTS cursors (
  destination_id INTEGER NOT NULL,
  source_id TEXT NOT NULL,
  cursor TEXT NOT NULL,
  updated_at INTEGER NOT NULL,
  PRIMARY KEY (destination_id, source_id)
);
CREATE TABLE IF NOT EXISTS fingerprints (
  destination_id INTEGER NOT NULL,
  source_id TEXT NOT NULL,
  record_key TEXT NOT NULL,
  hash TEXT NOT NULL,
  PRIMARY KEY (destination_id, source_id, record_key)
) WITHOUT ROWID;
CREATE TABLE IF NOT EXISTS kv (
  key TEXT PRIMARY KEY,
  value TEXT NOT NULL,
  updated_at INTEGER NOT NULL
);
`

// ErrNoDestination is returned by operations that need a destination before
// SelectDestination was called.
var ErrNoDestination = errors.New("spool: no destination selected")

type Store struct {
	db       *sql.DB
	path     string
	maxBytes int64
	// dest is the destination that cursors, fingerprints, commits and
	// delivery apply to (see SelectDestination).
	dest int64
	// legacy is set for read-only views of a version 0 spool that has not
	// been upgraded yet; it has a single, implicit destination.
	legacy bool
}

// Open opens or creates the spool database at path, upgrading the files of
// earlier Vario versions in place (see upgrade).
func Open(path string, maxBytes int64) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	// SQLite creates the -wal and -shm files with the database file's mode, so
	// create the database privately before SQLite does (umask would give 0644).
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	f.Close()
	if err := os.Chmod(path, 0o600); err != nil {
		return nil, err
	}
	q := url.Values{}
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "synchronous(FULL)")
	q.Add("_pragma", "busy_timeout(15000)")
	q.Set("_txlock", "immediate")
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path)+"?"+q.Encode())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(4)
	if err := upgrade(context.Background(), db); err != nil {
		db.Close()
		return nil, fmt.Errorf("initialize spool %s: %w", path, err)
	}
	// Files left by older versions may still be world-readable.
	for _, p := range []string{path + "-wal", path + "-shm"} {
		_ = os.Chmod(p, 0o600)
	}
	return &Store{db: db, path: path, maxBytes: maxBytes}, nil
}

// ErrUpgradeBlocked is returned when the spool of an earlier Vario version
// cannot be upgraded because a collector of that version is still running.
var ErrUpgradeBlocked = errors.New("the local state was created by an earlier Vario version whose collector is still running")

// upgrade creates the current schema, or converts the spool of an earlier
// version. Converting a version 0 spool keeps every queued entry, cursor and
// fingerprint: they are assigned to a "legacy" destination (empty endpoint),
// which the collector adopts as the configured destination on its next start
// (see Resolve). Converting to version 2 marks every queued entry as sent.
func upgrade(ctx context.Context, db *sql.DB) error {
	var version int
	if err := db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		return err
	}
	if version >= schemaVersion {
		return nil
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Read again under the write lock: another process may have upgraded the
	// spool meanwhile.
	if err := tx.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		return err
	}
	if version >= schemaVersion {
		return nil
	}
	var legacyTables int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'cursors'`).Scan(&legacyTables); err != nil {
		return err
	}
	if legacyTables > 0 {
		// A collector of an earlier version would keep working with the old
		// schema: one of version 0 writes rows without a destination (and its
		// cursor statements fail against the new tables), one of version 1
		// sends queued entries without recording it (queue.sent), so that a
		// later retry could reuse a token ClickHouse already consumed. Refuse
		// while it holds a live lease.
		var raw string
		if err := tx.QueryRowContext(ctx, `SELECT value FROM kv WHERE key = ?`, leaseKey).Scan(&raw); err == nil {
			var l Lease
			if json.Unmarshal([]byte(raw), &l) == nil && l.live() && l.PID != os.Getpid() {
				return fmt.Errorf("%w (%s, pid %d); restart it with the new version (`vario service restart`) or stop it, then try again", ErrUpgradeBlocked, l.Command, l.PID)
			}
		}
	}
	switch {
	case legacyTables == 0:
		if _, err := tx.ExecContext(ctx, schemaV1); err != nil {
			return err
		}
	case version < 1:
		var rows int
		if err := tx.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM queue) + (SELECT COUNT(*) FROM cursors) + (SELECT COUNT(*) FROM fingerprints)`).Scan(&rows); err != nil {
			return err
		}
		stmts := []string{
			`ALTER TABLE queue ADD COLUMN destination_id INTEGER NOT NULL DEFAULT 0`,
			`ALTER TABLE cursors RENAME TO cursors_v0`,
			`ALTER TABLE fingerprints RENAME TO fingerprints_v0`,
		}
		for _, q := range stmts {
			if _, err := tx.ExecContext(ctx, q); err != nil {
				return fmt.Errorf("upgrade spool: %w", err)
			}
		}
		if _, err := tx.ExecContext(ctx, schemaV1); err != nil {
			return err
		}
		if rows > 0 {
			now := nowMs()
			res, err := tx.ExecContext(ctx, `INSERT INTO destinations (endpoint, database, created_at, updated_at) VALUES ('', '', ?, ?)`, now, now)
			if err != nil {
				return err
			}
			legacy, _ := res.LastInsertId()
			for _, q := range []string{
				`UPDATE queue SET destination_id = ?`,
				`INSERT INTO cursors (destination_id, source_id, cursor, updated_at) SELECT ?, source_id, cursor, updated_at FROM cursors_v0`,
				`INSERT INTO fingerprints (destination_id, source_id, record_key, hash) SELECT ?, source_id, record_key, hash FROM fingerprints_v0`,
			} {
				if _, err := tx.ExecContext(ctx, q, legacy); err != nil {
					return fmt.Errorf("upgrade spool: %w", err)
				}
			}
		}
		for _, q := range []string{`DROP TABLE cursors_v0`, `DROP TABLE fingerprints_v0`} {
			if _, err := tx.ExecContext(ctx, q); err != nil {
				return err
			}
		}
	}
	// Version 2. Entries queued by an earlier version may have been sent
	// already (their insert acknowledged, the checkpoint after it not), so
	// each of them is retried under a new token (see BeginAttempt).
	if _, err := tx.ExecContext(ctx, `ALTER TABLE queue ADD COLUMN sent INTEGER NOT NULL DEFAULT 0`); err != nil {
		return fmt.Errorf("upgrade spool: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE queue SET sent = 1`); err != nil {
		return fmt.Errorf("upgrade spool: %w", err)
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(`PRAGMA user_version = %d`, schemaVersion)); err != nil {
		return err
	}
	return tx.Commit()
}

// OpenReadOnly opens an existing spool for inspection without creating or
// upgrading it.
func OpenReadOnly(path string) (*Store, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, err
	}
	q := url.Values{}
	q.Set("mode", "ro")
	q.Add("_pragma", "busy_timeout(5000)")
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path)+"?"+q.Encode())
	if err != nil {
		return nil, err
	}
	s := &Store{db: db, path: path}
	var version int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		db.Close()
		return nil, err
	}
	s.legacy = version == 0
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) Path() string { return s.path }

// Legacy reports whether this is a read-only view of a spool written by an
// earlier Vario version that has not been upgraded yet.
func (s *Store) Legacy() bool { return s.legacy }

// SelectDestination sets the destination that cursors, fingerprints,
// commits, delivery and statistics apply to.
func (s *Store) SelectDestination(id int64) { s.dest = id }

// Selected returns the selected destination id (0 if none).
func (s *Store) Selected() int64 { return s.dest }

func nowMs() int64 { return time.Now().UnixMilli() }

// Cursor returns the stored cursor for a source.
// Cursors belong to the selected destination: a source that was delivered to
// one destination has no cursor for another, so switching destinations
// re-reads the source for the new one.
func (s *Store) Cursor(ctx context.Context, sourceID string) (string, bool, error) {
	var c string
	var err error
	if s.legacy {
		err = s.db.QueryRowContext(ctx, `SELECT cursor FROM cursors WHERE source_id = ?`, sourceID).Scan(&c)
	} else {
		if s.dest == 0 {
			return "", false, ErrNoDestination
		}
		err = s.db.QueryRowContext(ctx, `SELECT cursor FROM cursors WHERE destination_id = ? AND source_id = ?`, s.dest, sourceID).Scan(&c)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return c, err == nil, err
}

type CommitOptions struct {
	// PolicyFingerprint is mixed into record fingerprints so that changing
	// privacy settings re-emits affected records.
	PolicyFingerprint string
	// Force enqueues records even when their content is unchanged. A forced
	// re-send is a historical replay: start it with BeginReplay, so that
	// ClickHouse does not take it for a retry of the earlier delivery.
	Force bool
	// MaxRowsPerEntry bounds queue entry size.
	MaxRowsPerEntry int
}

type CommitResult struct {
	Enqueued int
	Skipped  int
}

// Commit durably enqueues records for the selected destination and, when
// cursor is non-nil, advances the source cursor in the same transaction.
// Records whose content is identical to the last version enqueued for the
// same destination are skipped unless opts.Force is set.
func (s *Store) Commit(ctx context.Context, sourceID string, recs []model.Record, cursor *string, opts CommitOptions) (CommitResult, error) {
	var res CommitResult
	if s.dest == 0 {
		return res, ErrNoDestination
	}
	if len(recs) > 0 && s.maxBytes > 0 {
		used, err := s.queuedBytes(ctx)
		if err != nil {
			return res, err
		}
		if used >= s.maxBytes {
			return res, ErrFull
		}
	}

	type prepared struct {
		key, hash string
		line      []byte
	}
	byTable := map[string][]prepared{}
	for _, r := range recs {
		env := r.Row.Env()
		version := env.Version
		env.Version = 0
		plain, err := json.Marshal(r.Row)
		env.Version = version
		if err != nil {
			return res, fmt.Errorf("encode %s/%s: %w", r.Table, r.Key, err)
		}
		h := sha256.New()
		h.Write([]byte(opts.PolicyFingerprint))
		h.Write([]byte{0})
		h.Write(plain)
		line, err := json.Marshal(r.Row)
		if err != nil {
			return res, err
		}
		key := r.Table + "/" + r.Key
		byTable[r.Table] = append(byTable[r.Table], prepared{key: key, hash: hex.EncodeToString(h.Sum(nil)[:16]), line: line})
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return res, err
	}
	defer tx.Rollback()

	getFP, err := tx.PrepareContext(ctx, `SELECT hash FROM fingerprints WHERE destination_id = ? AND source_id = ? AND record_key = ?`)
	if err != nil {
		return res, err
	}
	defer getFP.Close()
	putFP, err := tx.PrepareContext(ctx, `INSERT INTO fingerprints (destination_id, source_id, record_key, hash) VALUES (?, ?, ?, ?)
		ON CONFLICT (destination_id, source_id, record_key) DO UPDATE SET hash = excluded.hash`)
	if err != nil {
		return res, err
	}
	defer putFP.Close()

	maxRows := opts.MaxRowsPerEntry
	if maxRows <= 0 {
		maxRows = 1000
	}
	replay := ""
	tables := make([]string, 0, len(byTable))
	for t := range byTable {
		tables = append(tables, t)
	}
	sort.Strings(tables)
	for _, table := range tables {
		var lines [][]byte
		for _, p := range byTable[table] {
			if !opts.Force && table != model.TableRawEvents {
				var old string
				err := getFP.QueryRowContext(ctx, s.dest, sourceID, p.key).Scan(&old)
				if err != nil && !errors.Is(err, sql.ErrNoRows) {
					return res, err
				}
				if old == p.hash {
					res.Skipped++
					continue
				}
			}
			if table != model.TableRawEvents {
				if _, err := putFP.ExecContext(ctx, s.dest, sourceID, p.key, p.hash); err != nil {
					return res, err
				}
			}
			lines = append(lines, p.line)
		}
		if len(lines) > 0 && replay == "" {
			if replay, err = replayIdentity(ctx, tx, s.dest); err != nil {
				return res, err
			}
		}
		for start := 0; start < len(lines); start += maxRows {
			end := min(start+maxRows, len(lines))
			if err := insertEntry(ctx, tx, s.dest, replay, table, lines[start:end]); err != nil {
				return res, err
			}
			res.Enqueued += end - start
		}
	}
	if cursor != nil {
		if _, err := tx.ExecContext(ctx, `INSERT INTO cursors (destination_id, source_id, cursor, updated_at) VALUES (?, ?, ?, ?)
			ON CONFLICT (destination_id, source_id) DO UPDATE SET cursor = excluded.cursor, updated_at = excluded.updated_at`,
			s.dest, sourceID, *cursor, nowMs()); err != nil {
			return res, err
		}
	}
	return res, tx.Commit()
}

// insertEntry queues lines for table. The entry's token is the
// insert_deduplication_token of its first delivery attempt: it is derived from
// the table, the destination's replay identity and the rows, and stored with
// the entry. Once an insert carrying it may have reached ClickHouse, every
// later attempt gets a new token first (see BeginAttempt).
func insertEntry(ctx context.Context, tx *sql.Tx, dest int64, replay, table string, lines [][]byte) error {
	var raw bytes.Buffer
	for _, l := range lines {
		raw.Write(l)
		raw.WriteByte('\n')
	}
	sum := sha256.Sum256(append([]byte(table+"\x00"+replay+"\x00"), raw.Bytes()...))
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	if _, err := zw.Write(raw.Bytes()); err != nil {
		return err
	}
	if err := zw.Close(); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO queue (destination_id, dest_table, token, row_count, raw_bytes, body, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		dest, table, hex.EncodeToString(sum[:16]), len(lines), raw.Len(), gz.Bytes(), nowMs())
	return err
}

func (s *Store) queuedBytes(ctx context.Context) (int64, error) {
	var n sql.NullInt64
	err := s.db.QueryRowContext(ctx, `SELECT SUM(length(body)) FROM queue`).Scan(&n)
	return n.Int64, err
}

// Entry is a queued batch of rows for one table.
type Entry struct {
	ID       int64
	Table    string
	Token    string
	Rows     int
	Attempts int
	body     []byte
}

// Body returns the decompressed JSONEachRow payload.
func (e Entry) Body() ([]byte, error) {
	zr, err := gzip.NewReader(bytes.NewReader(e.body))
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	return io.ReadAll(zr)
}

// Due returns entries of the selected destination that are ready for
// delivery, oldest first, up to maxRows rows in total. Entries queued for
// other destinations are never returned.
func (s *Store) Due(ctx context.Context, now time.Time, maxRows int) ([]Entry, error) {
	if s.dest == 0 {
		return nil, ErrNoDestination
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id, dest_table, token, row_count, attempts, body FROM queue
		WHERE destination_id = ? AND next_attempt_at <= ? ORDER BY id LIMIT 200`, s.dest, now.UnixMilli())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Entry
	total := 0
	for rows.Next() {
		var e Entry
		if err := rows.Scan(&e.ID, &e.Table, &e.Token, &e.Rows, &e.Attempts, &e.body); err != nil {
			return nil, err
		}
		if len(out) > 0 && total+e.Rows > maxRows {
			break
		}
		total += e.Rows
		out = append(out, e)
	}
	return out, rows.Err()
}

// ErrEntryGone is returned by BeginAttempt when an entry is no longer queued
// for the selected destination (it was delivered, moved or discarded).
var ErrEntryGone = errors.New("spool: queue entry is no longer queued")

// BeginAttempt must be called before every insert of entries, and the insert
// must carry the tokens of the returned entries (same order). In one durable
// transaction it records that an insert with each entry's token may reach
// ClickHouse from now on (queue.sent), and first gives every entry for which
// that was already recorded a new, random token. It returns how many entries
// got a new token.
//
// ClickHouse acknowledges an insert whose insert_deduplication_token it
// remembers without writing anything, and it remembers tokens after their
// rows are gone (partitions restored from a backup, REPLACE or DROP
// PARTITION, ALTER ... DELETE). An entry stays queued after an attempt when
// the insert failed, its outcome is unknown (a lost response), or the insert
// was acknowledged but the delivery checkpoint after it was not; a retry
// under the same token could then be acknowledged without storing a row
// although the database no longer holds the rows of the earlier attempt, and
// the checkpoint that follows would remove the entry from the queue. A retry
// under a new token always stores the rows: if the earlier attempt's rows
// are still there, ClickHouse holds them twice with the same keys and
// versions, which ReplacingMergeTree and the *_latest views count once.
//
// Recording before sending makes this hold across crashes and restarts: an
// insert can only have reached ClickHouse with a token whose entry was
// already marked as sent, so no restart retries an entry under a token that
// may have been used. Entries whose attempt never left the process are
// retried under a new token as well, which costs nothing.
func (s *Store) BeginAttempt(ctx context.Context, entries []Entry) ([]Entry, int, error) {
	if s.dest == 0 {
		return nil, 0, ErrNoDestination
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, 0, err
	}
	defer tx.Rollback()
	out := make([]Entry, len(entries))
	renewed := 0
	for i, e := range entries {
		var token string
		var sent bool
		err := tx.QueryRowContext(ctx, `SELECT token, sent FROM queue WHERE id = ? AND destination_id = ?`, e.ID, s.dest).Scan(&token, &sent)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, 0, fmt.Errorf("%w (entry %d)", ErrEntryGone, e.ID)
		}
		if err != nil {
			return nil, 0, err
		}
		if sent {
			if token, err = newToken(); err != nil {
				return nil, 0, err
			}
			renewed++
		}
		if _, err := tx.ExecContext(ctx, `UPDATE queue SET token = ?, sent = 1 WHERE id = ?`, token, e.ID); err != nil {
			return nil, 0, err
		}
		e.Token = token
		out[i] = e
	}
	if err := tx.Commit(); err != nil {
		return nil, 0, err
	}
	return out, renewed, nil
}

// newToken returns a random insert token, in the format of the derived ones.
func newToken() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// Ack removes delivered entries.
func (s *Store) Ack(ctx context.Context, ids []int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, id := range ids {
		if _, err := tx.ExecContext(ctx, `DELETE FROM queue WHERE id = ?`, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Fail records a failed delivery attempt and schedules the next one.
func (s *Store) Fail(ctx context.Context, ids []int64, next time.Time, msg string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, id := range ids {
		if _, err := tx.ExecContext(ctx, `UPDATE queue SET attempts = attempts + 1, next_attempt_at = ?, last_error = ? WHERE id = ?`,
			next.UnixMilli(), msg, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// NextDue returns when the selected destination's earliest pending entry
// becomes due, or false if it has none.
func (s *Store) NextDue(ctx context.Context) (time.Time, bool, error) {
	var ms sql.NullInt64
	if err := s.db.QueryRowContext(ctx, `SELECT MIN(next_attempt_at) FROM queue WHERE destination_id = ?`, s.dest).Scan(&ms); err != nil {
		return time.Time{}, false, err
	}
	if !ms.Valid {
		return time.Time{}, false, nil
	}
	return time.UnixMilli(ms.Int64), true, nil
}

// Stats describes the queue. Entries, Rows, Oldest, FailingEntries,
// LastError and ByTable cover the selected destination (every destination if
// none is selected); TotalBytes and OtherRows cover the rest of the spool.
type Stats struct {
	Entries        int
	Rows           int64
	StoredBytes    int64
	MaxBytes       int64
	Oldest         time.Time
	FailingEntries int
	LastError      string
	ByTable        map[string]int64
	// TotalBytes is the size of every queued entry, which the spool limit applies to.
	TotalBytes int64
	// OtherRows are queued for destinations other than the selected one.
	OtherRows int64
}

func (s *Store) destFilter() (string, []any) {
	if s.legacy || s.dest == 0 {
		return "1 = 1", nil
	}
	return "destination_id = ?", []any{s.dest}
}

func (s *Store) Stats(ctx context.Context) (Stats, error) {
	st := Stats{MaxBytes: s.maxBytes, ByTable: map[string]int64{}}
	where, args := s.destFilter()
	var rows, bytes, oldest sql.NullInt64
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*), SUM(row_count), SUM(length(body)), MIN(created_at) FROM queue WHERE `+where, args...).
		Scan(&st.Entries, &rows, &bytes, &oldest); err != nil {
		return st, err
	}
	st.Rows, st.StoredBytes = rows.Int64, bytes.Int64
	if oldest.Valid {
		st.Oldest = time.UnixMilli(oldest.Int64)
	}
	var total, all sql.NullInt64
	if err := s.db.QueryRowContext(ctx, `SELECT SUM(length(body)), SUM(row_count) FROM queue`).Scan(&total, &all); err != nil {
		return st, err
	}
	st.TotalBytes, st.OtherRows = total.Int64, all.Int64-st.Rows
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM queue WHERE attempts > 0 AND `+where, args...).Scan(&st.FailingEntries); err != nil {
		return st, err
	}
	_ = s.db.QueryRowContext(ctx, `SELECT last_error FROM queue WHERE attempts > 0 AND `+where+` ORDER BY id LIMIT 1`, args...).Scan(&st.LastError)
	r, err := s.db.QueryContext(ctx, `SELECT dest_table, SUM(row_count) FROM queue WHERE `+where+` GROUP BY dest_table`, args...)
	if err != nil {
		return st, err
	}
	defer r.Close()
	for r.Next() {
		var t string
		var n int64
		if err := r.Scan(&t, &n); err != nil {
			return st, err
		}
		st.ByTable[t] = n
	}
	return st, r.Err()
}

// ResetSource clears a source's cursor and fingerprints for the selected
// destination so the next sync re-reads everything.
func (s *Store) ResetSource(ctx context.Context, sourceID string) error {
	if s.dest == 0 {
		return ErrNoDestination
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM cursors WHERE destination_id = ? AND source_id = ?`, s.dest, sourceID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM fingerprints WHERE destination_id = ? AND source_id = ?`, s.dest, sourceID); err != nil {
		return err
	}
	// The re-read is a replay (see replay.go).
	if _, err := newReplay(ctx, tx, s.dest); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) SetKV(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO kv (key, value, updated_at) VALUES (?, ?, ?)
		ON CONFLICT (key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`, key, value, nowMs())
	return err
}

// KV returns a stored value and when it was last written.
func (s *Store) KV(ctx context.Context, key string) (string, time.Time, bool, error) {
	var v string
	var at int64
	err := s.db.QueryRowContext(ctx, `SELECT value, updated_at FROM kv WHERE key = ?`, key).Scan(&v, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return "", time.Time{}, false, nil
	}
	if err != nil {
		return "", time.Time{}, false, err
	}
	return v, time.UnixMilli(at), true, nil
}

type Lease struct {
	Owner   string    `json:"owner"`
	Host    string    `json:"host,omitempty"`
	PID     int       `json:"pid"`
	Command string    `json:"command"`
	Expires time.Time `json:"expires"`
}

const leaseKey = "collector.lease"

// AcquireLease takes or renews the collector lease for owner.
func (s *Store) AcquireLease(ctx context.Context, owner, command string, ttl time.Duration) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var raw string
	err = tx.QueryRowContext(ctx, `SELECT value FROM kv WHERE key = ?`, leaseKey).Scan(&raw)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if err == nil {
		var cur Lease
		if json.Unmarshal([]byte(raw), &cur) == nil && cur.Owner != owner && cur.live() {
			return fmt.Errorf("%w (%s, pid %d)", ErrLeaseHeld, cur.Command, cur.PID)
		}
	}
	host, _ := os.Hostname()
	b, _ := json.Marshal(Lease{Owner: owner, Host: host, PID: os.Getpid(), Command: command, Expires: time.Now().Add(ttl)})
	if _, err := tx.ExecContext(ctx, `INSERT INTO kv (key, value, updated_at) VALUES (?, ?, ?)
		ON CONFLICT (key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`, leaseKey, string(b), nowMs()); err != nil {
		return err
	}
	return tx.Commit()
}

// ReleaseLease drops the lease if owner holds it.
func (s *Store) ReleaseLease(ctx context.Context, owner string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM kv WHERE key = ? AND json_extract(value, '$.owner') = ?`, leaseKey, owner)
	return err
}

// CurrentLease reports the active lease, if any.
func (s *Store) CurrentLease(ctx context.Context) (Lease, bool, error) {
	v, _, ok, err := s.KV(ctx, leaseKey)
	if err != nil || !ok {
		return Lease{}, false, err
	}
	var l Lease
	if err := json.Unmarshal([]byte(v), &l); err != nil {
		return Lease{}, false, nil
	}
	return l, l.live(), nil
}

// live reports whether the lease still binds other processes: it has not
// expired and, when it was taken on this host, its process still exists. A
// collector that crashed is therefore replaced immediately rather than after
// the TTL. A reused PID only delays takeover until expiry.
func (l Lease) live() bool {
	if !time.Now().Before(l.Expires) {
		return false
	}
	if host, err := os.Hostname(); err == nil && l.Host != "" && l.Host == host && l.PID > 0 && l.PID != os.Getpid() {
		return processExists(l.PID)
	}
	return true
}
