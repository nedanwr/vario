package spool

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Destination is a ClickHouse database that records are queued for.
//
// Identity policy: a destination is identified by the destination id that
// `vario migrate` stores in the database itself (ServerID) whenever it is
// known, and otherwise by its normalized endpoint and database name. The
// ClickHouse user and password are never part of the identity, so rotating
// credentials keeps the destination. Two endpoints that lead to the same
// database (an alias, a new host name) are recognized by their ServerID. A
// database that was dropped and recreated gets a new ServerID and is
// therefore a new destination, which must be repopulated. A database that
// was restored to an earlier state keeps its ServerID; that is detected by
// the delivery checkpoint (see Checkpoint).
type Destination struct {
	ID       int64
	Endpoint string // normalized endpoint (see config.ClickHouseConfig.Endpoint); empty for legacy state
	Database string
	ServerID string
	Retired  time.Time
	// Pending is the number of rows queued for this destination.
	Pending int64
	// Cursors is the number of sources with a cursor for this destination.
	Cursors int
}

// Legacy reports whether the destination holds state from a Vario version
// that did not record destinations. It is adopted by the next collector run.
func (d Destination) Legacy() bool { return d.Endpoint == "" && d.Database == "" }

func (d Destination) String() string {
	switch {
	case d.Legacy():
		return "the destination used before this Vario version (not recorded)"
	case !d.Retired.IsZero():
		return fmt.Sprintf("%s, database %s (replaced %s)", d.Endpoint, d.Database, d.Retired.Local().Format(time.DateTime))
	}
	return fmt.Sprintf("%s, database %s", d.Endpoint, d.Database)
}

const activeKey = "destination.active"

const destCols = `d.id, d.endpoint, d.database, d.server_id, d.retired_at,
	(SELECT COALESCE(SUM(row_count), 0) FROM queue q WHERE q.destination_id = d.id),
	(SELECT COUNT(*) FROM cursors c WHERE c.destination_id = d.id)`

type rowScanner interface{ Scan(...any) error }

func scanDest(r rowScanner) (Destination, error) {
	var d Destination
	var retired int64
	if err := r.Scan(&d.ID, &d.Endpoint, &d.Database, &d.ServerID, &retired, &d.Pending, &d.Cursors); err != nil {
		return d, err
	}
	if retired > 0 {
		d.Retired = time.UnixMilli(retired)
	}
	return d, nil
}

type querier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// Destinations lists every known destination, including retired ones.
func (s *Store) Destinations(ctx context.Context) ([]Destination, error) {
	if s.legacy {
		var pending int64
		var cursors int
		if err := s.db.QueryRowContext(ctx, `SELECT (SELECT COALESCE(SUM(row_count), 0) FROM queue), (SELECT COUNT(*) FROM cursors)`).Scan(&pending, &cursors); err != nil {
			return nil, err
		}
		if pending == 0 && cursors == 0 {
			return nil, nil
		}
		return []Destination{{ID: -1, Pending: pending, Cursors: cursors}}, nil
	}
	return listDests(ctx, s.db)
}

func listDests(ctx context.Context, q querier) ([]Destination, error) {
	rows, err := q.QueryContext(ctx, `SELECT `+destCols+` FROM destinations d ORDER BY d.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Destination
	for rows.Next() {
		d, err := scanDest(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func getDest(ctx context.Context, q querier, id int64) (Destination, bool, error) {
	d, err := scanDest(q.QueryRowContext(ctx, `SELECT `+destCols+` FROM destinations d WHERE d.id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return d, false, nil
	}
	return d, err == nil, err
}

// Destination returns a destination by id.
func (s *Store) Destination(ctx context.Context, id int64) (Destination, bool, error) {
	if s.legacy {
		return Destination{}, false, nil
	}
	return getDest(ctx, s.db, id)
}

func activeID(ctx context.Context, q querier) (int64, error) {
	var v string
	err := q.QueryRowContext(ctx, `SELECT value FROM kv WHERE key = ?`, activeKey).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	id, _ := strconv.ParseInt(v, 10, 64)
	return id, nil
}

// Active returns the destination the collector delivered to most recently, if any.
func (s *Store) Active(ctx context.Context) (Destination, bool, error) {
	if s.legacy {
		ds, err := s.Destinations(ctx)
		if err != nil || len(ds) == 0 {
			return Destination{}, false, err
		}
		return ds[0], true, nil
	}
	id, err := activeID(ctx, s.db)
	if err == nil && id == 0 {
		// Upgraded state of an earlier version that no collector has run on yet.
		id, err = legacyID(ctx, s.db)
	}
	if err != nil || id == 0 {
		return Destination{}, false, err
	}
	return getDest(ctx, s.db, id)
}

// Lookup finds the current (not retired) destination for an endpoint and
// database without changing anything. Legacy state counts as a match when
// no destination has been recorded yet, since the next collector run adopts
// it for the configured destination.
func (s *Store) Lookup(ctx context.Context, endpoint, database string) (Destination, bool, error) {
	if s.legacy {
		return s.Active(ctx)
	}
	d, err := scanDest(s.db.QueryRowContext(ctx, `SELECT `+destCols+` FROM destinations d WHERE d.endpoint = ? AND d.database = ? AND d.retired_at = 0`, endpoint, database))
	if err == nil {
		return d, true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return d, false, err
	}
	ds, err := listDests(ctx, s.db)
	if err != nil {
		return Destination{}, false, err
	}
	if len(ds) == 1 && ds[0].Legacy() {
		return ds[0], true, nil
	}
	return Destination{}, false, nil
}

// Resolution is the outcome of resolving the configured destination.
type Resolution struct {
	// Configured is the destination for the configured endpoint and database.
	Configured Destination
	// Previous is the active destination when it differs from Configured.
	Previous *Destination
	// Created, Adopted and Aliased describe how Configured was found:
	// newly recorded, taken over from legacy state, or recognized by its
	// server id under a new endpoint.
	Created, Adopted, Aliased bool
	// Replaced is set when the database at the configured endpoint carries a
	// different server id than the one recorded: it was recreated, or the
	// endpoint now leads to another database. Nothing was changed.
	Replaced bool
	// ServerID is the server id the resolution was given (may be empty).
	ServerID string
}

// Changed reports whether the configured destination differs from the active one.
func (r Resolution) Changed() bool { return r.Previous != nil }

// Resolve finds or records the destination for endpoint and database. It
// does not change which destination is active (see Activate) and never moves
// or deletes queued data.
//
// serverID is the destination id read from the database, or empty when it
// is unknown (ClickHouse unreachable, or a schema without one). Entries left
// without a destination by a collector of an earlier version are assigned to
// the legacy destination, which is adopted when no destination has been
// recorded for the configured endpoint yet.
func (s *Store) Resolve(ctx context.Context, endpoint, database, serverID string) (Resolution, error) {
	res := Resolution{ServerID: serverID}
	if endpoint == "" || database == "" {
		return res, errors.New("spool: destination endpoint and database are required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return res, err
	}
	defer tx.Rollback()
	now := nowMs()

	legacy, err := legacyID(ctx, tx)
	if err != nil {
		return res, err
	}
	var orphans int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM queue WHERE destination_id = 0`).Scan(&orphans); err != nil {
		return res, err
	}
	if orphans > 0 {
		if legacy == 0 {
			r, err := tx.ExecContext(ctx, `INSERT INTO destinations (endpoint, database, created_at, updated_at) VALUES ('', '', ?, ?)`, now, now)
			if err != nil {
				return res, err
			}
			legacy, _ = r.LastInsertId()
		}
		if _, err := tx.ExecContext(ctx, `UPDATE queue SET destination_id = ? WHERE destination_id = 0`, legacy); err != nil {
			return res, err
		}
	}

	var id int64
	err = tx.QueryRowContext(ctx, `SELECT id FROM destinations WHERE endpoint = ? AND database = ? AND retired_at = 0`, endpoint, database).Scan(&id)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return res, err
	}
	if id == 0 && serverID != "" {
		// The same database under a new endpoint (alias, renamed host).
		err := tx.QueryRowContext(ctx, `SELECT id FROM destinations WHERE server_id = ? AND retired_at = 0 AND endpoint != '' ORDER BY id DESC LIMIT 1`, serverID).Scan(&id)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return res, err
		}
		if id != 0 {
			if _, err := tx.ExecContext(ctx, `UPDATE destinations SET endpoint = ?, database = ?, updated_at = ? WHERE id = ?`, endpoint, database, now, id); err != nil {
				return res, err
			}
			res.Aliased = true
		}
	}
	if id == 0 && legacy != 0 {
		var others int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM destinations WHERE endpoint != ''`).Scan(&others); err != nil {
			return res, err
		}
		if others == 0 {
			if _, err := tx.ExecContext(ctx, `UPDATE destinations SET endpoint = ?, database = ?, updated_at = ? WHERE id = ?`, endpoint, database, now, legacy); err != nil {
				return res, err
			}
			id, res.Adopted = legacy, true
		}
	}
	if id == 0 {
		r, err := tx.ExecContext(ctx, `INSERT INTO destinations (endpoint, database, server_id, created_at, updated_at) VALUES (?, ?, ?, ?, ?)`, endpoint, database, serverID, now, now)
		if err != nil {
			return res, err
		}
		id, _ = r.LastInsertId()
		res.Created = true
	}
	d, _, err := getDest(ctx, tx, id)
	if err != nil {
		return res, err
	}
	switch {
	case serverID == "" || d.ServerID == serverID:
	case d.ServerID == "":
		if _, err := tx.ExecContext(ctx, `UPDATE destinations SET server_id = ?, updated_at = ? WHERE id = ?`, serverID, now, id); err != nil {
			return res, err
		}
		d.ServerID = serverID
	default:
		res.Replaced = true
	}
	res.Configured = d
	active, err := activeID(ctx, tx)
	if err != nil {
		return res, err
	}
	if active != 0 && active != id {
		if p, ok, err := getDest(ctx, tx, active); err != nil {
			return res, err
		} else if ok {
			res.Previous = &p
		}
	}
	return res, tx.Commit()
}

func legacyID(ctx context.Context, q querier) (int64, error) {
	var id int64
	err := q.QueryRowContext(ctx, `SELECT id FROM destinations WHERE endpoint = '' AND database = '' AND retired_at = 0 ORDER BY id LIMIT 1`).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return id, err
}

// Activate makes id the active destination.
func (s *Store) Activate(ctx context.Context, id int64) error {
	return s.SetKV(ctx, activeKey, strconv.FormatInt(id, 10))
}

// ErrIdentityMismatch is returned when a destination already has a
// different server id.
var ErrIdentityMismatch = errors.New("the ClickHouse database is not the one Vario delivered to before")

// ErrIdentityMissing is returned when a destination has a recorded server id
// but the database reports none: its vario_meta table is gone or not readable
// any more, so the database cannot be told apart from a replacement.
var ErrIdentityMissing = errors.New("the ClickHouse database no longer reports a destination id")

// BindServerID records the server id of a destination the first time it is
// learned (a database migrated from a schema without one acquires its
// identity this way). A different, already recorded id yields
// ErrIdentityMismatch; no id where one is recorded yields ErrIdentityMissing.
// The recorded id is never overwritten.
func (s *Store) BindServerID(ctx context.Context, id int64, serverID string) error {
	d, ok, err := getDest(ctx, s.db, id)
	if err != nil || !ok {
		return err
	}
	switch {
	case d.ServerID == serverID:
		return nil
	case serverID == "":
		return fmt.Errorf("%w (recorded destination id %s)", ErrIdentityMissing, d.ServerID)
	case d.ServerID == "":
		_, err := s.db.ExecContext(ctx, `UPDATE destinations SET server_id = ?, updated_at = ? WHERE id = ? AND server_id = ''`, serverID, nowMs(), id)
		return err
	}
	return fmt.Errorf("%w (recorded destination id %s, found %s)", ErrIdentityMismatch, d.ServerID, serverID)
}

const replacedKey = "destination.replaced"

// Replacement describes a detected database replacement that has not been
// resolved: the database of Destination now reports the id Found instead of
// the recorded Destination.ServerID.
type Replacement struct {
	Destination Destination
	Found       string
	// At is when the replacement was first detected.
	At time.Time
}

// MarkReplaced remembers that the database of destination id was found to
// carry another server id, so that `vario status` and `vario doctor` can
// report it without contacting ClickHouse. Nothing else changes: the recorded
// id, queued entries, cursors and fingerprints are kept.
func (s *Store) MarkReplaced(ctx context.Context, id int64, found string) error {
	v := strconv.FormatInt(id, 10) + "|" + found
	if cur, _, ok, err := s.KV(ctx, replacedKey); err != nil || (ok && cur == v) {
		return err // keep the time of the first detection
	}
	return s.SetKV(ctx, replacedKey, v)
}

// ClearReplaced forgets a detected replacement (the recorded database is
// back, or the replacement was accepted with `vario destination switch`).
func (s *Store) ClearReplaced(ctx context.Context) error {
	if cur, _, ok, err := s.KV(ctx, replacedKey); err != nil || !ok || cur == "" {
		return err
	}
	return s.SetKV(ctx, replacedKey, "")
}

// Replaced returns the detected replacement, if it still applies: the
// destination is current (not retired by a switch) and its recorded id still
// differs from the one found.
func (s *Store) Replaced(ctx context.Context) (Replacement, bool, error) {
	var r Replacement
	if s.legacy {
		return r, false, nil
	}
	v, at, ok, err := s.KV(ctx, replacedKey)
	if err != nil || !ok || v == "" {
		return r, false, err
	}
	idStr, found, _ := strings.Cut(v, "|")
	id, _ := strconv.ParseInt(idStr, 10, 64)
	d, ok, err := getDest(ctx, s.db, id)
	if err != nil || !ok || !d.Retired.IsZero() || d.ServerID == "" || d.ServerID == found {
		return r, false, err
	}
	return Replacement{Destination: d, Found: found, At: at}, true, nil
}

// Replace retires destination id, whose database was recreated or replaced,
// and records a new destination for the same endpoint and database with
// serverID. The retired destination keeps its queued entries, cursors and
// fingerprints until they are moved or discarded.
func (s *Store) Replace(ctx context.Context, id int64, serverID string) (Destination, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Destination{}, err
	}
	defer tx.Rollback()
	old, ok, err := getDest(ctx, tx, id)
	if err != nil {
		return Destination{}, err
	}
	if !ok {
		return Destination{}, fmt.Errorf("spool: unknown destination %d", id)
	}
	now := nowMs()
	if _, err := tx.ExecContext(ctx, `UPDATE destinations SET retired_at = ?, updated_at = ? WHERE id = ?`, now, now, id); err != nil {
		return Destination{}, err
	}
	r, err := tx.ExecContext(ctx, `INSERT INTO destinations (endpoint, database, server_id, created_at, updated_at) VALUES (?, ?, ?, ?, ?)`,
		old.Endpoint, old.Database, serverID, now, now)
	if err != nil {
		return Destination{}, err
	}
	nid, _ := r.LastInsertId()
	d, _, err := getDest(ctx, tx, nid)
	if err != nil {
		return Destination{}, err
	}
	return d, tx.Commit()
}

// MovePending re-binds every entry queued for destination from to
// destination to, so it is delivered there, and returns the number of rows
// moved. Cursors and fingerprints are not moved.
//
// The cursors and fingerprints of from describe those rows as sent to from,
// which they now never will be. They are therefore deleted in the same
// transaction (see invalidateSync), so that from receives a full backfill if
// it is ever used again instead of resuming past the rows it did not get.
//
// The fingerprints of to no longer describe what its database will hold
// either: a moved row may have been queued under an earlier policy
// generation than rows to already has, but under a later one than to's
// newest row of the same record (privacy settings changed back), so it would
// become the latest version, and to's fingerprints would keep the next
// re-read from queueing the record again. to is therefore reset in the same
// transaction (see requireResync): its next sync re-reads every source in
// full and queues every record again under the current generation, which
// outranks every moved row.
func (s *Store) MovePending(ctx context.Context, from, to int64) (int64, error) {
	if from == to {
		return 0, nil
	}
	if d, ok, err := getDest(ctx, s.db, to); err != nil {
		return 0, err
	} else if !ok || !d.Retired.IsZero() {
		// Rows bound to an unknown or replaced destination would never be delivered.
		return 0, fmt.Errorf("spool: destination %d cannot receive queued rows", to)
	}
	// Reset the retry schedule: earlier failures concerned the old destination.
	return s.takePending(ctx, from, func(tx *sql.Tx) error { return requireResync(ctx, tx, to) },
		`UPDATE queue SET destination_id = ?, attempts = 0, next_attempt_at = 0, last_error = '' WHERE destination_id = ?`, to, from)
}

// DiscardPending deletes every entry queued for a destination and returns
// the number of rows removed. The destination's cursors and fingerprints are
// deleted with them (see MovePending): the discarded rows were never
// delivered, so the destination is no longer synchronized up to its cursor
// and receives a full backfill if it is used again.
func (s *Store) DiscardPending(ctx context.Context, id int64) (int64, error) {
	return s.takePending(ctx, id, nil, `DELETE FROM queue WHERE destination_id = ?`, id)
}

// takePending removes the queue entries of destination id with stmt and, if
// there were any, invalidates the destination's synchronization state and
// runs also (if not nil), in one transaction. It returns the number of rows
// the entries held.
func (s *Store) takePending(ctx context.Context, id int64, also func(*sql.Tx) error, stmt string, args ...any) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var entries int
	var n sql.NullInt64
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*), SUM(row_count) FROM queue WHERE destination_id = ?`, id).Scan(&entries, &n); err != nil {
		return 0, err
	}
	if entries == 0 {
		// Nothing undelivered: the destination's state stays accurate.
		return 0, nil
	}
	if _, err := tx.ExecContext(ctx, stmt, args...); err != nil {
		return 0, err
	}
	if err := invalidateSync(ctx, tx, id); err != nil {
		return 0, err
	}
	if also != nil {
		if err := also(tx); err != nil {
			return 0, err
		}
	}
	return n.Int64, tx.Commit()
}

// invalidateSync deletes the cursors and fingerprints of a destination. A
// source without a cursor is read again in full, and without fingerprints
// every record is queued again; records the destination already has carry the
// same keys and versions, so ReplacingMergeTree keeps one row per record.
//
// That re-read is a historical replay, so the destination also gets a new
// replay identity: the database may still remember the insert tokens of the
// rows it lost (see replay.go).
func invalidateSync(ctx context.Context, q querier, id int64) error {
	for _, stmt := range []string{`DELETE FROM cursors WHERE destination_id = ?`, `DELETE FROM fingerprints WHERE destination_id = ?`} {
		if _, err := q.ExecContext(ctx, stmt, id); err != nil {
			return err
		}
	}
	_, err := newReplay(ctx, q, id)
	return err
}

// LabelLegacy assigns state of an earlier Vario version (which recorded no
// destination) to the given endpoint and database and makes it the active
// destination. It is used when the configuration changes before the
// collector has run with this version, so that the state stays bound to the
// destination it was collected for.
func (s *Store) LabelLegacy(ctx context.Context, endpoint, database string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	legacy, err := legacyID(ctx, tx)
	if err != nil || legacy == 0 {
		return err
	}
	var existing int64
	err = tx.QueryRowContext(ctx, `SELECT id FROM destinations WHERE endpoint = ? AND database = ? AND retired_at = 0`, endpoint, database).Scan(&existing)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if existing != 0 {
		return fmt.Errorf("spool: destination %s/%s already exists", endpoint, database)
	}
	now := nowMs()
	if _, err := tx.ExecContext(ctx, `UPDATE destinations SET endpoint = ?, database = ?, updated_at = ? WHERE id = ?`, endpoint, database, now, legacy); err != nil {
		return err
	}
	if active, err := activeID(ctx, tx); err != nil {
		return err
	} else if active == 0 {
		if _, err := tx.ExecContext(ctx, `INSERT INTO kv (key, value, updated_at) VALUES (?, ?, ?)
			ON CONFLICT (key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`, activeKey, strconv.FormatInt(legacy, 10), now); err != nil {
			return err
		}
	}
	return tx.Commit()
}
