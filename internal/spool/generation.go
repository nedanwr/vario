package spool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"

	"github.com/nedanwr/vario/internal/model"
)

// Policy generations.
//
// Every row is queued under the spool's current policy generation, which is
// part of its version (see model.WithGeneration). The spool starts a new,
// higher generation in the transaction of the first commit under privacy
// settings that differ from the recorded ones. A row produced under newer
// settings therefore always has a higher version than every row queued
// before the change: whatever order deliveries, retries (with renewed
// tokens), moves between destinations and restarts put them in, a row
// queued under earlier settings can never again become the latest version
// of its record in ClickHouse.
//
// Starting a generation is not enough by itself: records whose newest row is
// from an earlier generation keep it as their latest version until they are
// queued again. After every new generation each destination therefore needs
// a complete re-read of every source, including its event history
// (NeedsResync). The re-read is recorded per destination and source, in the
// transaction of its final cursor, as the generation it started under
// (CommitOptions.SnapshotGeneration); a re-read during which another
// generation started does not count.
//
// The generation lives in the spool, so deleting the spool, or restoring it
// from an older copy, would lose generations that ClickHouse already holds
// rows of. The collector therefore compares the generation with the highest
// one the database holds for its sources before it delivers (VerifyLineage).
//
// State (in the kv table, no schema change):
//
//	policy.generation               {"fingerprint": ..., "generation": n}
//	policy.synced.<dest>.<source>   the generation the last complete re-read started under
//	policy.lineage.<dest>           present while the generation was verified against the database

const generationKey = "policy.generation"

func syncedKey(dest int64, sourceID string) string {
	return "policy.synced." + strconv.FormatInt(dest, 10) + "." + sourceID
}

func lineageKey(dest int64) string { return "policy.lineage." + strconv.FormatInt(dest, 10) }

// ErrGenerationsExhausted is returned when no further policy generation can
// be started (see model.MaxGeneration).
var ErrGenerationsExhausted = errors.New("spool: the privacy settings changed too often; no further policy generation is available")

type generationState struct {
	Fingerprint string `json:"fingerprint"`
	Generation  uint64 `json:"generation"`
}

func getGeneration(ctx context.Context, q querier) (generationState, bool, error) {
	v, _, ok, err := kvGet(ctx, q, generationKey)
	if err != nil || !ok {
		return generationState{}, false, err
	}
	var st generationState
	if err := json.Unmarshal([]byte(v), &st); err != nil {
		return generationState{}, false, fmt.Errorf("spool: invalid policy generation %q: %w", v, err)
	}
	return st, true, nil
}

func putGeneration(ctx context.Context, q querier, st generationState) error {
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	return kvPut(ctx, q, generationKey, string(b))
}

// currentGeneration returns the generation of the privacy settings with
// fingerprint fp, starting a new generation if fp differs from the recorded
// one. The first commit of a spool (or of one written before generations
// existed) starts generation 1, so its rows outrank rows of earlier
// versions, which carry generation 0.
//
// verified is the destination whose generation this process verified
// against the database (0 if none). Verifications of other destinations are
// withdrawn when a generation starts, because the new generation was not
// compared with their databases (see VerifyLineage).
func currentGeneration(ctx context.Context, q querier, fp string, verified int64) (uint64, error) {
	st, ok, err := getGeneration(ctx, q)
	if err != nil {
		return 0, err
	}
	if ok && st.Fingerprint == fp {
		return st.Generation, nil
	}
	next := st.Generation + 1
	if next > model.MaxGeneration {
		return 0, ErrGenerationsExhausted
	}
	if err := putGeneration(ctx, q, generationState{Fingerprint: fp, Generation: next}); err != nil {
		return 0, err
	}
	if _, err := q.ExecContext(ctx, `DELETE FROM kv WHERE key LIKE 'policy.lineage.%' AND key != ?`, lineageKey(verified)); err != nil {
		return 0, err
	}
	return next, nil
}

// EnsureGeneration returns the policy generation that rows committed with
// opts are queued under, starting it if opts.PolicyFingerprint differs from
// the recorded settings.
func (s *Store) EnsureGeneration(ctx context.Context, opts CommitOptions) (uint64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	gen, err := currentGeneration(ctx, tx, opts.PolicyFingerprint, opts.VerifiedDestination)
	if err != nil {
		return 0, err
	}
	return gen, tx.Commit()
}

// Generation returns the recorded policy generation and the fingerprint of
// the privacy settings it belongs to. It only reads.
func (s *Store) Generation(ctx context.Context) (gen uint64, fingerprint string, err error) {
	st, _, err := getGeneration(ctx, s.db)
	return st.Generation, st.Fingerprint, err
}

// NeedsResync reports whether the selected destination needs a complete
// re-read of source sourceID, including its event history, before rows of
// the privacy settings with fingerprint fp can be the latest of every
// record: the settings differ from the recorded ones, or a generation
// started after the last complete re-read.
//
// force is set when no complete re-read was recorded for the destination and
// source at all (a spool of a Vario version without generations). The
// records' fingerprints then cannot tell whether the newest queued row of a
// record is also its latest in ClickHouse, so the re-read must queue every
// record again.
func (s *Store) NeedsResync(ctx context.Context, sourceID, fp string) (need, force bool, err error) {
	if s.dest == 0 {
		return false, false, ErrNoDestination
	}
	st, ok, err := getGeneration(ctx, s.db)
	if err != nil {
		return false, false, err
	}
	v, _, found, err := kvGet(ctx, s.db, syncedKey(s.dest, sourceID))
	if err != nil {
		return false, false, err
	}
	if !found {
		return true, true, nil
	}
	synced, err := strconv.ParseUint(v, 10, 64)
	if err != nil {
		return true, true, nil
	}
	return !ok || st.Fingerprint != fp || synced < st.Generation, false, nil
}

// VerifyLineage compares the spool's policy generation with dbGen, the
// highest generation the selected destination's database holds rows of for
// the configured sources, and records the destination as verified.
//
// Rows of a higher generation than the spool's, or of the spool's own
// generation where the destination was not verified before, were not
// written by this spool as it is now: it was deleted (`state.db` reset) or
// restored from an older copy, and those rows may be of other privacy
// settings than the current ones. They would outrank or tie with every row
// the spool queues, so a new generation above them is started and the
// destination's fingerprints are deleted; the next sync of each source then
// re-reads it completely and queues every record again under the new
// generation (see NeedsResync). raised reports this.
func (s *Store) VerifyLineage(ctx context.Context, dbGen uint64) (raised bool, gen uint64, err error) {
	if s.dest == 0 {
		return false, 0, ErrNoDestination
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, 0, err
	}
	defer tx.Rollback()
	st, _, err := getGeneration(ctx, tx)
	if err != nil {
		return false, 0, err
	}
	_, _, verified, err := kvGet(ctx, tx, lineageKey(s.dest))
	if err != nil {
		return false, 0, err
	}
	if dbGen > st.Generation || (!verified && dbGen >= 1 && dbGen == st.Generation) {
		if dbGen+1 > model.MaxGeneration {
			return false, 0, ErrGenerationsExhausted
		}
		st.Generation = dbGen + 1
		if err := putGeneration(ctx, tx, st); err != nil {
			return false, 0, err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM fingerprints WHERE destination_id = ?`, s.dest); err != nil {
			return false, 0, err
		}
		raised = true
	}
	if err := kvPut(ctx, tx, lineageKey(s.dest), strconv.FormatUint(st.Generation, 10)); err != nil {
		return false, 0, err
	}
	return raised, st.Generation, tx.Commit()
}
