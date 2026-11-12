package spool

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"strconv"
)

// Replay identity.
//
// Every queue entry carries the insert_deduplication_token of its delivery
// (see insertEntry). ClickHouse remembers the tokens of recent inserts per
// table partition and acknowledges an insert with a known token without
// writing anything. That is what makes a retried delivery safe, and the
// token must therefore stay the same for every retry of an entry: it is
// stored with the entry, never recomputed.
//
// The memory outlives the rows, though: ALTER ... DELETE, REPLACE PARTITION
// and RESTORE into existing tables remove or replace rows and keep the
// tokens. If a token were derived from the rows alone, a historical replay
// (`vario backfill --force`, or the complete re-read after a destination's
// synchronization state was reset) would re-send the same batches under the
// same tokens, ClickHouse would acknowledge them without restoring a row,
// and the replay would report success while records stay missing.
//
// The token therefore also covers the destination's replay identity: a
// random value kept in the kv table, created with the destination's first
// queued entry and replaced whenever a replay starts (BeginReplay, and every
// reset of the destination's cursors and fingerprints). Rows queued by a
// replay can never carry the token of an earlier delivery, while entries
// already queued keep theirs. Being persisted, the identity survives
// interruptions and restarts: a replay that is continued after a restart
// queues its remaining records under the same identity, and nothing that was
// queued before is re-tokenized. Logical deduplication is unaffected: rows
// sent again carry the same keys and versions, so ReplacingMergeTree and the
// *_latest views count each record once.

func replayKey(dest int64) string { return "replay." + strconv.FormatInt(dest, 10) }

// replayIdentity returns the replay identity of dest, creating it first if
// the destination has none.
func replayIdentity(ctx context.Context, q querier, dest int64) (string, error) {
	v, _, ok, err := kvGet(ctx, q, replayKey(dest))
	if err != nil || (ok && v != "") {
		return v, err
	}
	return newReplay(ctx, q, dest)
}

// newReplay gives dest a new replay identity.
func newReplay(ctx context.Context, q querier, dest int64) (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	id := hex.EncodeToString(b[:])
	return id, kvPut(ctx, q, replayKey(dest), id)
}

// BeginReplay starts an intentional historical replay for the selected
// destination: records queued from now on get insert tokens that no earlier
// delivery used, so that ClickHouse stores them even where it still
// remembers the tokens of the rows they restore. Entries already queued keep
// their tokens (their retries stay idempotent).
func (s *Store) BeginReplay(ctx context.Context) error {
	if s.dest == 0 {
		return ErrNoDestination
	}
	_, err := newReplay(ctx, s.db, s.dest)
	return err
}

// Replay returns the replay identity of a destination (empty if it has none
// yet). It only reads.
func (s *Store) Replay(ctx context.Context, dest int64) (string, error) {
	v, _, _, err := kvGet(ctx, s.db, replayKey(dest))
	return v, err
}
