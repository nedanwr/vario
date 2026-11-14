package spool

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"
)

// Checkpoint is the local half of a destination's delivery checkpoint.
//
// The destination id tells databases apart, but a database restored from an
// older backup keeps its id while it loses everything delivered after the
// backup. The checkpoint closes that gap. After the inserts of a delivery
// round were acknowledged, the collector writes a row with the next sequence
// number to the database's checkpoint table and, once that insert was
// acknowledged too, removes the delivered entries from the queue and records
// the number here, in one transaction (AckCheckpoint). So:
//
//   - every entry that left the queue is covered by a checkpoint the database
//     acknowledged after the entry's rows;
//   - Confirmed never exceeds what the database acknowledged. A checkpoint
//     whose insert failed or whose outcome is unknown is not recorded, and
//     the entries it would have covered stay queued and are sent again.
//
// A database that holds a lower number than Confirmed for Writer therefore no
// longer holds what was delivered to it.
//
// The state is kept in the kv table, which every spool version has, so it
// needs no schema change and earlier Vario versions ignore it.
type Checkpoint struct {
	// Writer identifies this spool's checkpoints for the destination in the
	// database, where several collectors may keep theirs. It is random and
	// replaced when a rollback is accepted (AcceptRollback), so sequence
	// numbers of the abandoned history can never be mistaken for current ones.
	Writer string
	// Confirmed is the highest sequence number whose insert the database
	// acknowledged (0: none yet).
	Confirmed uint64
	// At is when the state last changed.
	At time.Time
}

type checkpointState struct {
	Writer    string `json:"writer"`
	Confirmed uint64 `json:"confirmed"`
}

func checkpointKey(dest int64) string { return "checkpoint." + strconv.FormatInt(dest, 10) }

func kvGet(ctx context.Context, q querier, key string) (string, time.Time, bool, error) {
	var v string
	var at int64
	err := q.QueryRowContext(ctx, `SELECT value, updated_at FROM kv WHERE key = ?`, key).Scan(&v, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return "", time.Time{}, false, nil
	}
	if err != nil {
		return "", time.Time{}, false, err
	}
	return v, time.UnixMilli(at), true, nil
}

func kvPut(ctx context.Context, q querier, key, value string) error {
	_, err := q.ExecContext(ctx, `INSERT INTO kv (key, value, updated_at) VALUES (?, ?, ?)
		ON CONFLICT (key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`, key, value, nowMs())
	return err
}

func getCheckpoint(ctx context.Context, q querier, dest int64) (Checkpoint, error) {
	v, at, ok, err := kvGet(ctx, q, checkpointKey(dest))
	if err != nil || !ok || v == "" {
		return Checkpoint{}, err
	}
	var st checkpointState
	if err := json.Unmarshal([]byte(v), &st); err != nil {
		return Checkpoint{}, fmt.Errorf("spool: unreadable checkpoint state of destination %d: %w", dest, err)
	}
	return Checkpoint{Writer: st.Writer, Confirmed: st.Confirmed, At: at}, nil
}

func putCheckpoint(ctx context.Context, q querier, dest int64, cp Checkpoint) error {
	b, err := json.Marshal(checkpointState{Writer: cp.Writer, Confirmed: cp.Confirmed})
	if err != nil {
		return err
	}
	return kvPut(ctx, q, checkpointKey(dest), string(b))
}

// Checkpoint returns the checkpoint state of a destination; the zero value
// if it has none. It only reads.
func (s *Store) Checkpoint(ctx context.Context, dest int64) (Checkpoint, error) {
	return getCheckpoint(ctx, s.db, dest)
}

// CheckpointWriter returns the checkpoint state of a destination, giving it
// a writer id first if it has none.
func (s *Store) CheckpointWriter(ctx context.Context, dest int64) (Checkpoint, error) {
	if dest <= 0 {
		return Checkpoint{}, ErrNoDestination
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Checkpoint{}, err
	}
	defer tx.Rollback()
	cp, err := getCheckpoint(ctx, tx, dest)
	if err != nil || cp.Writer != "" {
		return cp, err
	}
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return cp, err
	}
	cp = Checkpoint{Writer: hex.EncodeToString(b[:]), At: time.Now()}
	if err := putCheckpoint(ctx, tx, dest, cp); err != nil {
		return cp, err
	}
	return cp, tx.Commit()
}

// AckCheckpoint removes delivered entries from the queue and records seq as
// the destination's confirmed checkpoint, in one transaction. It must only be
// called after the database acknowledged the insert of checkpoint seq, which
// in turn must have been sent after the inserts of these entries were
// acknowledged. seq has to advance the confirmed checkpoint.
func (s *Store) AckCheckpoint(ctx context.Context, dest int64, ids []int64, seq uint64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	cp, err := getCheckpoint(ctx, tx, dest)
	if err != nil {
		return err
	}
	if cp.Writer == "" {
		return fmt.Errorf("spool: destination %d has no checkpoint writer", dest)
	}
	if seq <= cp.Confirmed {
		return fmt.Errorf("spool: checkpoint %d does not advance the confirmed checkpoint %d", seq, cp.Confirmed)
	}
	for _, id := range ids {
		if _, err := tx.ExecContext(ctx, `DELETE FROM queue WHERE id = ? AND destination_id = ?`, id, dest); err != nil {
			return err
		}
	}
	cp.Confirmed = seq
	if err := putCheckpoint(ctx, tx, dest, cp); err != nil {
		return err
	}
	return tx.Commit()
}

const rolledBackKey = "destination.rolledback"

// Rollback describes a detected database rollback that has not been
// resolved: the database of Destination, which still carries the recorded
// destination id, holds a lower delivery checkpoint than it had acknowledged.
type Rollback struct {
	Destination Destination
	// Confirmed is the checkpoint the database acknowledged.
	Confirmed uint64
	// Found is the highest checkpoint the database held when the rollback
	// was detected; 0 if it held none (or no checkpoint table).
	Found uint64
	// At is when the rollback was first detected.
	At time.Time
}

type rollbackState struct {
	Destination int64  `json:"destination"`
	Writer      string `json:"writer"`
	Found       uint64 `json:"found"`
}

// MarkRolledBack remembers that the database of destination id was found
// with checkpoint found, below the confirmed one, so that `vario status` and
// `vario doctor` can report it without contacting ClickHouse. Nothing else
// changes: queued entries, cursors, fingerprints and the confirmed checkpoint
// are kept.
func (s *Store) MarkRolledBack(ctx context.Context, id int64, found uint64) error {
	cp, err := getCheckpoint(ctx, s.db, id)
	if err != nil {
		return err
	}
	b, err := json.Marshal(rollbackState{Destination: id, Writer: cp.Writer, Found: found})
	if err != nil {
		return err
	}
	if cur, _, ok, err := s.KV(ctx, rolledBackKey); err != nil || (ok && cur == string(b)) {
		return err // keep the time of the first detection
	}
	return s.SetKV(ctx, rolledBackKey, string(b))
}

// ClearRolledBack forgets a detected rollback (the database holds the
// confirmed checkpoint again, or the rollback was accepted).
func (s *Store) ClearRolledBack(ctx context.Context) error {
	if cur, _, ok, err := s.KV(ctx, rolledBackKey); err != nil || !ok || cur == "" {
		return err
	}
	return s.SetKV(ctx, rolledBackKey, "")
}

// RolledBack returns the detected rollback, if it still applies: the
// destination is current (not retired by a switch) and still has the
// checkpoint state the rollback was detected against.
func (s *Store) RolledBack(ctx context.Context) (Rollback, bool, error) {
	var r Rollback
	if s.legacy {
		return r, false, nil
	}
	v, at, ok, err := s.KV(ctx, rolledBackKey)
	if err != nil || !ok || v == "" {
		return r, false, err
	}
	var st rollbackState
	if json.Unmarshal([]byte(v), &st) != nil {
		return r, false, nil
	}
	d, ok, err := getDest(ctx, s.db, st.Destination)
	if err != nil || !ok || !d.Retired.IsZero() {
		return r, false, err
	}
	cp, err := getCheckpoint(ctx, s.db, d.ID)
	if err != nil || cp.Writer != st.Writer || cp.Confirmed <= st.Found {
		return r, false, err
	}
	return Rollback{Destination: d, Confirmed: cp.Confirmed, Found: st.Found, At: at}, true, nil
}

// AcceptRollback accepts that the database of destination id was restored to
// an earlier state and no longer holds everything delivered to it. In one
// transaction it
//
//   - deletes the destination's cursors and fingerprints, so that every source
//     is read again in full and every record is queued again (records the
//     database still has carry the same keys and versions, so
//     ReplacingMergeTree keeps one row per record);
//   - discards the checkpoint state, so that the next collector run starts a
//     new checkpoint history under a new writer id, against which a later
//     restore of any earlier backup is detected again;
//   - forgets the detected rollback.
//
// Queued entries are kept: they belong to this database and are delivered to
// it. Accepting twice changes nothing more.
func (s *Store) AcceptRollback(ctx context.Context, id int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	d, ok, err := getDest(ctx, tx, id)
	if err != nil {
		return err
	}
	if !ok || !d.Retired.IsZero() {
		return fmt.Errorf("spool: destination %d is not in use", id)
	}
	if err := invalidateSync(ctx, tx, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM kv WHERE key IN (?, ?)`, checkpointKey(id), rolledBackKey); err != nil {
		return err
	}
	return tx.Commit()
}
