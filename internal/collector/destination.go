package collector

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/nedanwr/vario/internal/spool"
	"github.com/nedanwr/vario/internal/storage/clickhouse"
)

// DestinationError explains why the collector does not start with the
// configured destination. Queued data is never sent to a destination other
// than the one it was collected for, and never deleted, without an explicit
// decision (`vario destination switch`).
type DestinationError struct {
	Configured spool.Destination
	// Previous is the destination the collector delivered to before, which
	// still has queued rows.
	Previous *spool.Destination
	// Replaced is set when the database at the configured endpoint is not the
	// one Vario delivered to before (it was recreated, or the URL now leads to
	// another server).
	Replaced bool
	// Found is the destination id the database carries now (Replaced only).
	Found string
	// RolledBack is set when the database still carries the recorded
	// destination id but holds a lower delivery checkpoint than it had
	// acknowledged: it was restored to an earlier state. Checkpoint is the
	// acknowledged checkpoint, FoundCheckpoint the one found (0: none).
	RolledBack      bool
	Checkpoint      uint64
	FoundCheckpoint uint64
}

func (e *DestinationError) Error() string {
	var b strings.Builder
	if e.RolledBack {
		same := "It is the database Vario delivered to"
		if e.Configured.ServerID != "" {
			same = "It carries the recorded destination id " + e.Configured.ServerID + ", so it is the same database"
		}
		fmt.Fprintf(&b, "the ClickHouse database at %s was restored to an earlier state: it acknowledged delivery checkpoint %d, but now holds %s. %s, without the rows delivered after that point (restored from a backup, or the checkpoint table %s was emptied or is no longer readable).\n",
			e.Configured, e.Checkpoint, foundCheckpoint(e.FoundCheckpoint), same, clickhouse.CheckpointTable)
		b.WriteString("Delivery is suspended and nothing was changed: Vario does not treat the database as synchronized")
		if e.Configured.Pending > 0 {
			fmt.Fprintf(&b, ", and keeps the %d queued rows in the local spool", e.Configured.Pending)
		}
		b.WriteString(".\n")
		b.WriteString(RollbackRecovery)
		return b.String()
	}
	if e.Replaced {
		fmt.Fprintf(&b, "the ClickHouse database at %s was replaced: Vario delivered to destination id %s there, but the database now carries %s. It was dropped and recreated, restored from elsewhere, or the URL now leads to another database.\n",
			e.Configured, orUnknown(e.Configured.ServerID), orUnknown(e.Found))
		b.WriteString("Delivery is suspended and nothing was changed: Vario does not assume that previously delivered data exists there")
		if e.Configured.Pending > 0 {
			fmt.Fprintf(&b, ", and keeps the %d queued rows in the local spool", e.Configured.Pending)
		}
		b.WriteString(".\n")
		b.WriteString(ReplacedRecovery)
		return b.String()
	}
	fmt.Fprintf(&b, "the ClickHouse destination changed:\n  previous: %s (%d rows queued, not yet delivered)\n  current:  %s\n", e.Previous, e.Previous.Pending, e.Configured)
	b.WriteString("Vario will not send data queued for the previous destination to the current one.\n")
	b.WriteString(`Run "vario destination" for details, then one of:
  vario destination switch --keep-pending      use the configured destination; queued rows stay with the previous one (decide later with "vario destination pending")
  vario destination switch --move-pending      also deliver the queued rows to the configured destination
  vario destination switch --discard-pending   delete the queued rows (the source is re-read for the new destination anyway)`)
	b.WriteString("\nOr restore the previous clickhouse.url / clickhouse.database to deliver the queued rows there first.")
	return b.String()
}

// ReplacedRecovery tells how to recover from a replaced database. Recovery is
// always an explicit decision: Vario neither accepts the new database nor
// migrates it on its own.
const ReplacedRecovery = `To recover:
  - If this is unintended (wrong URL, the original database will be restored), fix that: the collector starts and delivers normally again once the database reports the recorded id.
  - To accept the database as a new, empty destination, stop the collector (` + "`vario service uninstall`" + ` for the background service) and run one of:
      vario destination switch --move-pending      deliver the queued rows to the new database as well (recommended)
      vario destination switch --discard-pending   delete the queued rows
      vario destination switch --keep-pending      leave the queued rows in the spool, bound to the replaced database (not delivered; decide later with ` + "`vario destination pending`" + `)
    The new destination starts without cursors, so the next ` + "`vario run`" + ` (or ` + "`vario service install`" + `) reads every source again in full and delivers the complete history.
Vario never creates or migrates the schema during recovery: a database without it needs ` + "`vario migrate`" + ` first. See ` + "`vario destination`" + ` and ` + "`vario doctor`" + `.`

// RollbackRecovery tells how to recover from a database that was restored to
// an earlier state. Recovery is an explicit decision: Vario does not resume
// incremental delivery into such a database on its own.
const RollbackRecovery = `To recover:
  - If this is unintended (the newer state will be restored, or read access to the checkpoint table was lost), fix that: the collector starts and delivers normally again once the database holds the acknowledged checkpoint.
  - To keep the restored database and deliver what it is missing, stop the collector (` + "`vario service uninstall`" + ` for the background service) and run:
      vario destination switch
    This resets the synchronization state of this destination (cursors and fingerprints); queued rows are kept and delivered. The next ` + "`vario run`" + ` (or ` + "`vario service install`" + `) then reads every source again in full and delivers the complete history. Rows the database still has are sent again with the same keys and versions, so the *_latest and usage views count each record once.
Vario cannot tell which rows are missing, so recovery is always the complete reconciliation. See ` + "`vario destination`" + ` and ` + "`vario doctor`" + `.`

func foundCheckpoint(seq uint64) string {
	if seq == 0 {
		return "none"
	}
	return "only checkpoint " + strconv.FormatUint(seq, 10)
}

func orUnknown(id string) string {
	if id == "" {
		return "an unknown id"
	}
	return id
}

// identityCheckInterval bounds how long a running collector trusts its last
// destination check. Rows delivered within one interval after a database was
// recreated are not lost: the collector then stops, and the recreated
// database is a new destination that receives a full backfill.
var identityCheckInterval = time.Minute

// A checkpoint below the confirmed one is read again before it is reported
// as a rollback, rollbackRereads times, rollbackRereadDelay apart: a replica
// that has not caught up with an acknowledged insert must not suspend
// delivery.
var (
	rollbackRereads     = 2
	rollbackRereadDelay = time.Second
)

// probeTimeout bounds the destination identity lookup at startup, so that an
// unreachable ClickHouse delays an offline start only briefly.
var probeTimeout = 5 * time.Second

// Prepare selects the configured destination in the spool. It must be
// called before Run, Snapshot or Drain.
//
// When probe is set, the destination is checked first (read-only) to learn
// its destination id, which recognizes the same database under a new URL and
// detects a replaced database. If ClickHouse is unreachable, the destination
// is identified by its endpoint and database alone.
//
// A destination change is accepted automatically when the previous
// destination has no queued rows: the new destination has its own cursors,
// so every source is read again in full for it, while the previous
// destination's state is kept for switching back. Otherwise, and for a
// replaced database, a *DestinationError is returned and nothing changes.
//
// A database that carries the recorded id is then checked for its delivery
// checkpoint (verifyCheckpoint): if it was restored to an earlier state, a
// *DestinationError with RolledBack is returned as well.
func (c *Collector) Prepare(ctx context.Context, probe bool) (spool.Resolution, error) {
	serverID, checked := "", false
	var info clickhouse.DestinationInfo
	if probe {
		pctx, cancel := context.WithTimeout(ctx, probeTimeout)
		var err error
		info, err = c.sink.CheckDestination(pctx)
		cancel()
		if err == nil {
			serverID, checked = info.ServerID, true
			c.noteWarnings(info.Warnings)
			c.schemaReady, c.checkedAt = true, time.Now()
		} else if ctx.Err() == nil {
			c.log.Warn("destination check failed; data is collected into the spool and delivered once it succeeds", "error", err.Error())
		}
	}
	res, err := c.store.Resolve(ctx, c.opts.Endpoint, c.opts.Database, serverID)
	if err != nil {
		return res, err
	}
	switch {
	case res.Replaced:
		c.schemaReady = false
		_ = c.store.MarkReplaced(ctx, res.Configured.ID, serverID)
		return res, &DestinationError{Configured: res.Configured, Replaced: true, Found: serverID}
	case res.Previous != nil && res.Previous.Pending > 0:
		c.schemaReady = false
		return res, &DestinationError{Configured: res.Configured, Previous: res.Previous}
	}
	if err := c.store.Activate(ctx, res.Configured.ID); err != nil {
		return res, err
	}
	c.store.SelectDestination(res.Configured.ID)
	c.dest = res.Configured
	switch {
	case checked && serverID == "" && res.Configured.ServerID != "":
		// The database lost the id recorded for it: leave it to the check
		// before the first delivery, which keeps everything spooled.
		c.schemaReady = false
	case checked && serverID != "":
		c.identityConfirmed(ctx)
	}
	if checked && c.schemaReady {
		// Same identity; now the delivery checkpoint, which reveals a database
		// restored to an earlier state.
		vctx, cancel := context.WithTimeout(ctx, probeTimeout+time.Duration(rollbackRereads)*rollbackRereadDelay)
		err := c.verifyCheckpoint(vctx, res.Configured, info, true)
		cancel()
		var de *DestinationError
		switch {
		case errors.As(err, &de):
			c.schemaReady = false
			return res, de
		case err != nil && ctx.Err() == nil:
			// Leave it to the check before the first delivery.
			c.schemaReady = false
			c.log.Warn("the delivery checkpoint could not be verified; nothing is delivered until it is", "error", err.Error())
		}
	}
	switch {
	case res.Adopted:
		c.log.Info("local state from the previous Vario version assigned to the configured destination", "destination", res.Configured.String())
	case res.Aliased:
		c.log.Info("destination recognized under a new endpoint by its destination id", "destination", res.Configured.String())
	}
	if res.Previous != nil {
		c.log.Warn("ClickHouse destination changed; sources are read again in full for the new destination",
			"previous", res.Previous.String(), "current", res.Configured.String())
	}
	return res, nil
}

// Destination returns the destination selected by Prepare.
func (c *Collector) Destination() spool.Destination { return c.dest }

func (c *Collector) noteWarnings(ws []string) {
	for _, w := range ws {
		if !c.warned[w] {
			c.warned[w] = true
			c.log.Warn(w)
		}
	}
}

// checkDestination runs the read-only destination check before delivery and
// binds the destination id. An identity mismatch (the database was recreated
// or replaced) is returned as a *fatalError, which stops the collector before
// anything else is delivered or collected. Nothing is changed or discarded:
// queued rows, cursors and the recorded id stay as they are, the replacement
// is remembered for `vario status` and `vario doctor`, and every later start
// refuses the destination until `vario destination switch` records it as a
// new one, which has no cursors and is therefore backfilled in full.
//
// A database that reports no id although one is recorded cannot be verified
// either. That is not treated as a replacement (the id may only be unreadable
// for the moment), but nothing is delivered until the id is back or `vario
// migrate` gave the database a new one, which is then a replacement.
func (c *Collector) checkDestination(ctx context.Context) error {
	info, err := c.sink.CheckDestination(ctx)
	if err != nil {
		_ = c.store.SetKV(ctx, "delivery.last_error", "destination: "+err.Error())
		return fmt.Errorf("destination check: %w", err)
	}
	err = c.store.BindServerID(ctx, c.dest.ID, info.ServerID)
	switch {
	case errors.Is(err, spool.ErrIdentityMismatch):
		d := c.dest
		if cur, ok, derr := c.store.Destination(ctx, d.ID); derr == nil && ok {
			d = cur // current queue size and recorded id
		}
		_ = c.store.MarkReplaced(ctx, d.ID, info.ServerID)
		_ = c.store.SetKV(ctx, "delivery.last_error", "destination: "+err.Error()+"; delivery is suspended until `vario destination switch`")
		c.log.Error("ClickHouse database was replaced; delivery is suspended and queued rows are kept",
			"destination", d.String(), "recorded_destination_id", d.ServerID, "found_destination_id", info.ServerID, "queued_rows", d.Pending)
		return &fatalError{&DestinationError{Configured: d, Replaced: true, Found: info.ServerID}}
	case errors.Is(err, spool.ErrIdentityMissing):
		err = fmt.Errorf("%w: the table %s is missing or not readable for the collector's user, so the database cannot be verified and nothing is delivered. "+
			"Restore read access to it, or run `vario migrate` if the database was recreated", err, clickhouse.MetaTable)
		_ = c.store.SetKV(ctx, "delivery.last_error", "destination: "+err.Error())
		return fmt.Errorf("destination check: %w", err)
	case err != nil:
		return err
	}
	if info.ServerID != "" {
		c.identityConfirmed(ctx)
	}
	if err := c.verifyCheckpoint(ctx, c.dest, info, true); err != nil {
		var de *DestinationError
		if errors.As(err, &de) {
			return &fatalError{de}
		}
		_ = c.store.SetKV(ctx, "delivery.last_error", "destination: "+err.Error())
		return fmt.Errorf("destination check: %w", err)
	}
	c.noteWarnings(info.Warnings)
	return nil
}

// verifyCheckpoint compares the delivery checkpoint the database holds for
// this spool with the one it acknowledged (see spool.Checkpoint). It runs
// after the destination id was confirmed, at every destination check.
//
//   - A lower checkpoint, or none where one was acknowledged, means the
//     database was restored to an earlier state: a *DestinationError
//     (RolledBack) is returned. With mark, the rollback is also recorded for
//     `vario status` and `vario doctor`.
//   - A database without the checkpoint table (a schema older than version 3)
//     that never acknowledged a checkpoint is delivered to as before, with a
//     warning that a rollback cannot be detected.
//   - A destination without a confirmed checkpoint gets its first one, so
//     that from here on the database must hold it. If the user lacks the
//     privileges for that, delivery continues without checkpoints, with the
//     same warning.
//   - A higher checkpoint than the confirmed one is fine: an earlier write
//     reached the database without the spool learning of it.
//
// Any other error means the checkpoint could not be verified now; nothing
// may be delivered until it is.
func (c *Collector) verifyCheckpoint(ctx context.Context, d spool.Destination, info clickhouse.DestinationInfo, mark bool) error {
	cp, err := c.store.Checkpoint(ctx, d.ID)
	if err != nil {
		return err
	}
	if !info.Checkpoints {
		if cp.Confirmed > 0 {
			return c.rolledBack(ctx, d, cp, 0, mark)
		}
		c.checkpoints = false
		c.noteWarnings([]string{fmt.Sprintf("the database has no delivery checkpoint table (%s) that the collector's user can use: a database restored to an earlier state cannot be detected; run `vario migrate`", clickhouse.CheckpointTable)})
		return nil
	}
	if cp, err = c.store.CheckpointWriter(ctx, d.ID); err != nil {
		return err
	}
	// unusable handles a user that may not read or write the checkpoint table
	// before any checkpoint was acknowledged (for example after a migration
	// whose grants were not applied): delivery continues as it did before the
	// table existed. Once a checkpoint was acknowledged, the same failure
	// stops delivery instead, because the database could not be verified.
	unusable := func(err error) bool {
		if cp.Confirmed > 0 || clickhouse.Classify(err) != clickhouse.KindPermission {
			return false
		}
		c.checkpoints = false
		c.noteWarnings([]string{fmt.Sprintf("the collector's user cannot use the delivery checkpoint table %s (%v): a database restored to an earlier state cannot be detected; run `vario migrate` to grant SELECT and INSERT on it", clickhouse.CheckpointTable, err)})
		return true
	}
	seq, err := c.readCheckpoint(ctx, cp)
	if err != nil {
		if unusable(err) {
			return nil
		}
		return fmt.Errorf("read the delivery checkpoint: %w", err)
	}
	if seq < cp.Confirmed {
		return c.rolledBack(ctx, d, cp, seq, mark)
	}
	if cp.Confirmed == 0 {
		// The first checkpoint of this spool for the destination. It vouches
		// for nothing delivered earlier; from now on the database must hold it.
		if err := c.sink.WriteCheckpoint(ctx, cp.Writer, seq+1); err != nil {
			if unusable(err) {
				return nil
			}
			return fmt.Errorf("write the first delivery checkpoint: %w", err)
		}
		if err := c.store.AckCheckpoint(context.WithoutCancel(ctx), d.ID, nil, seq+1); err != nil {
			return err
		}
		c.log.Info("delivery checkpoints enabled: a database restored to an earlier state is detected from now on", "destination", d.String(), "checkpoint", seq+1)
	}
	c.checkpoints = true
	if mark {
		c.rollbackWithdrawn(ctx, d)
	}
	return nil
}

// readCheckpoint returns the highest checkpoint the database holds for cp's
// writer. A value below the confirmed checkpoint is read again a few times
// before it is believed.
func (c *Collector) readCheckpoint(ctx context.Context, cp spool.Checkpoint) (uint64, error) {
	for attempt := 0; ; attempt++ {
		seq, _, err := c.sink.ReadCheckpoint(ctx, cp.Writer)
		if err != nil || seq >= cp.Confirmed || attempt >= rollbackRereads {
			return seq, err
		}
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-time.After(rollbackRereadDelay):
		}
	}
}

// rolledBack reports a database that holds checkpoint found, below the
// confirmed one. Nothing is changed or discarded: queued rows, cursors and
// the confirmed checkpoint stay as they are, so every later check keeps
// refusing the database until it holds the checkpoint again or the rollback
// is accepted with `vario destination switch`, which resets the cursors so
// that the complete history is delivered again.
func (c *Collector) rolledBack(ctx context.Context, d spool.Destination, cp spool.Checkpoint, found uint64, mark bool) *DestinationError {
	if cur, ok, err := c.store.Destination(ctx, d.ID); err == nil && ok {
		d = cur // current queue size
	}
	de := &DestinationError{Configured: d, RolledBack: true, Checkpoint: cp.Confirmed, FoundCheckpoint: found}
	if mark {
		_ = c.store.MarkRolledBack(ctx, d.ID, found)
		_ = c.store.SetKV(ctx, "delivery.last_error", fmt.Sprintf("destination: the database was restored to an earlier state (acknowledged checkpoint %d, found %s); delivery is suspended until it holds the checkpoint again or `vario destination switch`",
			cp.Confirmed, foundCheckpoint(found)))
		c.log.Error("ClickHouse database was restored to an earlier state; delivery is suspended and queued rows are kept",
			"destination", d.String(), "destination_id", d.ServerID, "acknowledged_checkpoint", cp.Confirmed, "found_checkpoint", found, "queued_rows", d.Pending)
	}
	return de
}

// rollbackWithdrawn withdraws an earlier rollback report once the database
// holds the confirmed checkpoint again.
func (c *Collector) rollbackWithdrawn(ctx context.Context, d spool.Destination) {
	if rb, ok, err := c.store.RolledBack(ctx); err != nil || !ok || rb.Destination.ID != d.ID {
		return
	}
	_ = c.store.ClearRolledBack(ctx)
	_ = c.store.SetKV(ctx, "delivery.last_error", "")
	c.log.Info("the ClickHouse database holds the acknowledged delivery checkpoint again; delivery resumes", "destination", d.String())
}

// identityConfirmed withdraws an earlier replacement report once the
// database carries the recorded destination id again.
func (c *Collector) identityConfirmed(ctx context.Context) {
	if _, ok, err := c.store.Replaced(ctx); err != nil || !ok {
		return
	}
	_ = c.store.ClearReplaced(ctx)
	_ = c.store.SetKV(ctx, "delivery.last_error", "")
	c.log.Info("the ClickHouse database carries the recorded destination id again; delivery resumes", "destination", c.dest.String())
}

// fatalError stops the collector instead of being retried.
type fatalError struct{ err error }

func (e *fatalError) Error() string { return e.err.Error() }
func (e *fatalError) Unwrap() error { return e.err }
