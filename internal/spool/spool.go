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

const schema = `
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
  last_error TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS queue_due ON queue(next_attempt_at, id);
CREATE TABLE IF NOT EXISTS cursors (
  source_id TEXT PRIMARY KEY,
  cursor TEXT NOT NULL,
  updated_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS fingerprints (
  source_id TEXT NOT NULL,
  record_key TEXT NOT NULL,
  hash TEXT NOT NULL,
  PRIMARY KEY (source_id, record_key)
) WITHOUT ROWID;
CREATE TABLE IF NOT EXISTS kv (
  key TEXT PRIMARY KEY,
  value TEXT NOT NULL,
  updated_at INTEGER NOT NULL
);
`

type Store struct {
	db       *sql.DB
	path     string
	maxBytes int64
}

// Open opens or creates the spool database at path.
func Open(path string, maxBytes int64) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
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
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("initialize spool %s: %w", path, err)
	}
	_ = os.Chmod(path, 0o600)
	return &Store{db: db, path: path, maxBytes: maxBytes}, nil
}

// OpenReadOnly opens an existing spool for inspection without creating it.
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
	return &Store{db: db, path: path}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) Path() string { return s.path }

func nowMs() int64 { return time.Now().UnixMilli() }

// Cursor returns the stored cursor for a source.
func (s *Store) Cursor(ctx context.Context, sourceID string) (string, bool, error) {
	var c string
	err := s.db.QueryRowContext(ctx, `SELECT cursor FROM cursors WHERE source_id = ?`, sourceID).Scan(&c)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return c, err == nil, err
}

type CommitOptions struct {
	// PolicyFingerprint is mixed into record fingerprints so that changing
	// privacy settings re-emits affected records.
	PolicyFingerprint string
	// Force enqueues records even when their content is unchanged.
	Force bool
	// MaxRowsPerEntry bounds queue entry size.
	MaxRowsPerEntry int
}

type CommitResult struct {
	Enqueued int
	Skipped  int
}

// Commit durably enqueues records and, when cursor is non-nil, advances the
// source cursor in the same transaction. Records whose content is identical to
// the last enqueued version are skipped unless opts.Force is set.
func (s *Store) Commit(ctx context.Context, sourceID string, recs []model.Record, cursor *string, opts CommitOptions) (CommitResult, error) {
	var res CommitResult
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

	getFP, err := tx.PrepareContext(ctx, `SELECT hash FROM fingerprints WHERE source_id = ? AND record_key = ?`)
	if err != nil {
		return res, err
	}
	defer getFP.Close()
	putFP, err := tx.PrepareContext(ctx, `INSERT INTO fingerprints (source_id, record_key, hash) VALUES (?, ?, ?)
		ON CONFLICT (source_id, record_key) DO UPDATE SET hash = excluded.hash`)
	if err != nil {
		return res, err
	}
	defer putFP.Close()

	maxRows := opts.MaxRowsPerEntry
	if maxRows <= 0 {
		maxRows = 1000
	}
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
				err := getFP.QueryRowContext(ctx, sourceID, p.key).Scan(&old)
				if err != nil && !errors.Is(err, sql.ErrNoRows) {
					return res, err
				}
				if old == p.hash {
					res.Skipped++
					continue
				}
			}
			if table != model.TableRawEvents {
				if _, err := putFP.ExecContext(ctx, sourceID, p.key, p.hash); err != nil {
					return res, err
				}
			}
			lines = append(lines, p.line)
		}
		for start := 0; start < len(lines); start += maxRows {
			end := min(start+maxRows, len(lines))
			if err := insertEntry(ctx, tx, table, lines[start:end]); err != nil {
				return res, err
			}
			res.Enqueued += end - start
		}
	}
	if cursor != nil {
		if _, err := tx.ExecContext(ctx, `INSERT INTO cursors (source_id, cursor, updated_at) VALUES (?, ?, ?)
			ON CONFLICT (source_id) DO UPDATE SET cursor = excluded.cursor, updated_at = excluded.updated_at`,
			sourceID, *cursor, nowMs()); err != nil {
			return res, err
		}
	}
	return res, tx.Commit()
}

func insertEntry(ctx context.Context, tx *sql.Tx, table string, lines [][]byte) error {
	var raw bytes.Buffer
	for _, l := range lines {
		raw.Write(l)
		raw.WriteByte('\n')
	}
	sum := sha256.Sum256(append([]byte(table+"\x00"), raw.Bytes()...))
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	if _, err := zw.Write(raw.Bytes()); err != nil {
		return err
	}
	if err := zw.Close(); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO queue (dest_table, token, row_count, raw_bytes, body, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		table, hex.EncodeToString(sum[:16]), len(lines), raw.Len(), gz.Bytes(), nowMs())
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

// Due returns entries ready for delivery, oldest first, up to maxRows rows in total.
func (s *Store) Due(ctx context.Context, now time.Time, maxRows int) ([]Entry, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, dest_table, token, row_count, attempts, body FROM queue
		WHERE next_attempt_at <= ? ORDER BY id LIMIT 200`, now.UnixMilli())
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

// NextDue returns when the earliest pending entry becomes due, or false if the queue is empty.
func (s *Store) NextDue(ctx context.Context) (time.Time, bool, error) {
	var ms sql.NullInt64
	if err := s.db.QueryRowContext(ctx, `SELECT MIN(next_attempt_at) FROM queue`).Scan(&ms); err != nil {
		return time.Time{}, false, err
	}
	if !ms.Valid {
		return time.Time{}, false, nil
	}
	return time.UnixMilli(ms.Int64), true, nil
}

type Stats struct {
	Entries        int
	Rows           int64
	StoredBytes    int64
	MaxBytes       int64
	Oldest         time.Time
	FailingEntries int
	LastError      string
	ByTable        map[string]int64
}

func (s *Store) Stats(ctx context.Context) (Stats, error) {
	st := Stats{MaxBytes: s.maxBytes, ByTable: map[string]int64{}}
	var rows, bytes, oldest sql.NullInt64
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*), SUM(row_count), SUM(length(body)), MIN(created_at) FROM queue`).
		Scan(&st.Entries, &rows, &bytes, &oldest); err != nil {
		return st, err
	}
	st.Rows, st.StoredBytes = rows.Int64, bytes.Int64
	if oldest.Valid {
		st.Oldest = time.UnixMilli(oldest.Int64)
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM queue WHERE attempts > 0`).Scan(&st.FailingEntries); err != nil {
		return st, err
	}
	_ = s.db.QueryRowContext(ctx, `SELECT last_error FROM queue WHERE attempts > 0 ORDER BY id LIMIT 1`).Scan(&st.LastError)
	r, err := s.db.QueryContext(ctx, `SELECT dest_table, SUM(row_count) FROM queue GROUP BY dest_table`)
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

// ResetSource clears a source's cursor and fingerprints so the next sync re-reads everything.
func (s *Store) ResetSource(ctx context.Context, sourceID string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM cursors WHERE source_id = ?`, sourceID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM fingerprints WHERE source_id = ?`, sourceID); err != nil {
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
		if json.Unmarshal([]byte(raw), &cur) == nil && cur.Owner != owner && time.Now().Before(cur.Expires) {
			return fmt.Errorf("%w (%s, pid %d)", ErrLeaseHeld, cur.Command, cur.PID)
		}
	}
	b, _ := json.Marshal(Lease{Owner: owner, PID: os.Getpid(), Command: command, Expires: time.Now().Add(ttl)})
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
	return l, time.Now().Before(l.Expires), nil
}
