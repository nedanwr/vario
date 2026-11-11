package collector

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/nedanwr/vario/internal/spool"
	"github.com/nedanwr/vario/internal/storage/clickhouse"
)

// deliverLoop delivers until ctx is cancelled. It returns an error only for
// failures that must stop the collector (a replaced destination, or one that
// was restored to an earlier state).
func (c *Collector) deliverLoop(ctx context.Context) error {
	failures := 0
	for {
		n, err := c.deliverRound(ctx, time.Now())
		if ctx.Err() != nil {
			return nil
		}
		var fe *fatalError
		if errors.As(err, &fe) {
			c.log.Error("delivery stopped", "error", fe.err.Error())
			return err
		}
		wait := c.opts.FlushInterval
		if err != nil {
			failures++
			if failures == 1 || failures%10 == 0 {
				c.log.Warn("delivery failed; data remains spooled and will be retried", "error", err.Error(), "attempt", failures)
			}
			wait = backoff(c.opts.RetryInitial, c.opts.RetryMax, failures)
		} else {
			if failures > 0 {
				c.log.Info("delivery recovered")
			}
			failures = 0
			if n > 0 {
				// More data may be due; continue without waiting a full interval.
				wait = min(wait, time.Second)
			} else if next, ok, _ := c.store.NextDue(ctx); ok {
				if d := time.Until(next); d > wait {
					wait = d
				}
			}
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		case <-c.wake:
			timer.Stop()
			// Coalesce bursts of small commits into fewer inserts.
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(min(c.opts.FlushInterval, 2*time.Second)):
			}
		}
	}
}

// deliverRound sends every entry due at now, grouped by table. It returns the
// number of rows delivered and the first error encountered.
func (c *Collector) deliverRound(ctx context.Context, now time.Time) (int, error) {
	if c.dest.ID == 0 {
		return 0, errors.New("collector: destination not prepared")
	}
	// The destination is checked before the first delivery and again at least
	// every identityCheckInterval, whether or not anything is queued: a
	// database that is dropped and recreated while the collector runs accepts
	// inserts without any error, so only its destination id reveals that it no
	// longer holds what was delivered before, and one that was restored from a
	// backup even keeps its id, so its delivery checkpoint is compared as well.
	// The check never creates or migrates the schema; the only thing it may
	// write is the collector's own first checkpoint row.
	if !c.schemaReady || time.Since(c.checkedAt) >= identityCheckInterval {
		c.schemaReady = false
		if err := c.checkDestination(ctx); err != nil {
			c.checkFailed = true
			return 0, err
		}
		c.schemaReady, c.checkedAt = true, time.Now()
		if c.checkFailed {
			// Nothing may be queued to report a success, so clear the failure here.
			c.checkFailed = false
			_ = c.store.SetKV(ctx, "delivery.last_error", "")
		}
	}
	entries, err := c.store.Due(ctx, now, c.opts.MaxInsertRows)
	if err != nil || len(entries) == 0 {
		return 0, err
	}
	order, groups := byTable(entries)
	var sent []spool.Entry
	sentRows := 0
	var firstErr error
	// retryIn schedules the next attempt for entries that stay queued.
	retryIn := func(group []spool.Entry, err error) time.Duration {
		wait := backoff(c.opts.RetryInitial, c.opts.RetryMax, group[0].Attempts+1)
		var chErr *clickhouse.Error
		if errors.As(err, &chErr) {
			if !chErr.Transient {
				wait = c.opts.RetryMax
			}
			// Unknown table or database: check the destination again before the
			// next attempt (it may have been dropped or recreated).
			if chErr.Code == 60 || chErr.Code == 81 {
				c.schemaReady = false
			}
		}
		return wait
	}
	for _, table := range order {
		group := groups[table]
		rows, err := c.insertGroup(ctx, table, group)
		if err == nil {
			sent = append(sent, group...)
			sentRows += rows
			continue
		}
		if ctx.Err() != nil {
			// Shutting down: what was inserted so far is still acknowledged
			// below if that needs no further request (no checkpoint table).
			break
		}
		_ = c.store.Fail(context.WithoutCancel(ctx), entryIDs(group), time.Now().Add(retryIn(group, err)), err.Error())
		_ = c.store.SetKV(ctx, "delivery.last_error", table+": "+err.Error())
		if firstErr == nil {
			firstErr = fmt.Errorf("%s: %w", table, err)
		}
	}
	delivered := 0
	if len(sent) > 0 && (ctx.Err() == nil || !c.checkpoints) {
		// The inserts were acknowledged, but the rows only leave the spool
		// once a checkpoint covers them (see confirmDelivered).
		switch err := c.confirmDelivered(ctx, c.dest, sent, true); {
		case err == nil:
			delivered = sentRows
		case isFatal(err):
			return 0, err
		case ctx.Err() != nil:
			return 0, ctx.Err()
		default:
			_ = c.store.Fail(context.WithoutCancel(ctx), entryIDs(sent), time.Now().Add(retryIn(sent, err)), err.Error())
			_ = c.store.SetKV(ctx, "delivery.last_error", err.Error())
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	if delivered > 0 {
		_ = c.store.SetKV(ctx, "delivery.last_success", strconv.Itoa(delivered))
		if firstErr == nil {
			_ = c.store.SetKV(ctx, "delivery.last_error", "")
		}
		c.log.Debug("delivered", "rows", delivered)
	}
	if ctx.Err() != nil {
		return delivered, ctx.Err()
	}
	return delivered, firstErr
}

func isFatal(err error) bool {
	var fe *fatalError
	return errors.As(err, &fe)
}

// byTable groups entries by table, keeping the order in which tables first appear.
func byTable(entries []spool.Entry) (order []string, groups map[string][]spool.Entry) {
	groups = map[string][]spool.Entry{}
	for _, e := range entries {
		if _, ok := groups[e.Table]; !ok {
			order = append(order, e.Table)
		}
		groups[e.Table] = append(groups[e.Table], e)
	}
	return order, groups
}

func entryIDs(group []spool.Entry) []int64 {
	ids := make([]int64, len(group))
	for i, e := range group {
		ids[i] = e.ID
	}
	return ids
}

// insertGroup sends the entries of one table as a single insert and returns
// the number of rows ClickHouse acknowledged. It does not remove the entries
// from the spool: that is confirmDelivered's decision.
func (c *Collector) insertGroup(ctx context.Context, table string, group []spool.Entry) (int, error) {
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	h := sha256.New()
	rows := 0
	for _, e := range group {
		body, err := e.Body()
		if err != nil {
			return 0, fmt.Errorf("corrupt spool entry %d: %w", e.ID, err)
		}
		if _, err := zw.Write(body); err != nil {
			return 0, err
		}
		h.Write([]byte(e.Token))
		rows += e.Rows
	}
	if err := zw.Close(); err != nil {
		return 0, err
	}
	token := hex.EncodeToString(h.Sum(nil)[:16])
	if err := c.sink.Insert(ctx, table, gz.Bytes(), token); err != nil {
		return 0, err
	}
	return rows, nil
}

// confirmDelivered removes entries from the spool whose inserts ClickHouse
// acknowledged, after covering them with a delivery checkpoint:
//
//  1. read the highest checkpoint the database holds for this spool. It must
//     not be lower than the confirmed one: otherwise the database is not what
//     was delivered to (restored to an earlier state, or recreated since the
//     last destination check), nothing is acknowledged, and the destination
//     check decides which it is and stops delivery. Reading right before
//     writing matters: writing a higher checkpoint into a restored database
//     would hide the rollback from every later check;
//  2. insert the next checkpoint;
//  3. only after that insert was acknowledged, delete the entries and record
//     the checkpoint as confirmed, in one spool transaction.
//
// If any step fails, or its outcome is unknown (a lost response), the entries
// stay queued and are sent again in a later round, followed by a new, higher
// checkpoint: the confirmed checkpoint never runs ahead of rows the database
// acknowledged, and a checkpoint that was written without the spool learning
// of it only raises the number the database holds. Rows sent twice carry the
// same keys and versions.
//
// A database without the checkpoint table (a schema older than version 3)
// gets the entries acknowledged directly, as before; a rollback cannot be
// detected there, which the destination check reports.
func (c *Collector) confirmDelivered(ctx context.Context, d spool.Destination, sent []spool.Entry, mark bool) error {
	ids := entryIDs(sent)
	if !c.checkpoints {
		// The inserts succeeded; acknowledge even if ctx was cancelled meanwhile.
		return c.store.Ack(context.WithoutCancel(ctx), ids)
	}
	cp, err := c.store.CheckpointWriter(ctx, d.ID)
	if err != nil {
		return err
	}
	seq, err := c.readCheckpoint(ctx, cp)
	if err != nil {
		return fmt.Errorf("delivery checkpoint: rows were sent but stay queued, the checkpoint could not be read: %w", err)
	}
	if seq < cp.Confirmed {
		// Less than the database acknowledged. Before this is called a
		// rollback, find out which database it is: one that was dropped and
		// recreated since the last destination check holds no checkpoint
		// either, and is a replacement. The destination check compares the
		// id first and then reports the rollback itself.
		var err error
		if mark {
			err = c.checkDestination(ctx)
		} else {
			err = c.checkRetained(ctx, d)
		}
		if err == nil {
			err = errors.New("delivery checkpoint: rows were sent but stay queued, the database reported a lower checkpoint for a moment")
		}
		return err
	}
	if err := c.sink.WriteCheckpoint(ctx, cp.Writer, seq+1); err != nil {
		return fmt.Errorf("delivery checkpoint: rows were sent but stay queued, checkpoint %d was not confirmed: %w", seq+1, err)
	}
	// The checkpoint is durable; record it even if ctx was cancelled meanwhile.
	return c.store.AckCheckpoint(context.WithoutCancel(ctx), d.ID, ids, seq+1)
}

type DrainResult struct {
	Delivered int
	Remaining int64
}

// Drain delivers everything in the spool, ignoring retry schedules, and stops at the first failure.
func (c *Collector) Drain(ctx context.Context) (DrainResult, error) {
	var res DrainResult
	for {
		n, err := c.deliverRound(ctx, time.Now().Add(365*24*time.Hour))
		res.Delivered += n
		if err != nil || n == 0 {
			st, serr := c.store.Stats(context.WithoutCancel(ctx))
			if serr == nil {
				res.Remaining = st.Rows
			}
			if err == nil && res.Remaining > 0 {
				err = errors.New("spool entries could not be delivered")
			}
			return res, err
		}
	}
}

// ErrDestinationCheck wraps a failed destination check in DeliverRetained:
// the database could not be identified, so nothing was sent.
var ErrDestinationCheck = errors.New("the destination could not be checked")

// DeliverRetained delivers the rows queued for d, a destination other than
// the active one whose rows were kept by `vario destination switch
// --keep-pending`, to the database they were queued for. It is the explicit
// retry of `vario destination pending`; the collector itself never calls it.
//
// The rows are sent only if the configured ClickHouse database is that
// destination: it has d's endpoint and database and reports the destination
// id recorded for d (spool.BindServerID, the rule the collector applies to its
// own destination). Otherwise an error wrapping ErrDestinationCheck,
// spool.ErrIdentityMismatch or spool.ErrIdentityMissing is returned and
// nothing is sent or changed. The identity is checked again at least every
// identityCheckInterval.
//
// Neither the active destination nor d's cursors and fingerprints change:
// they already describe these rows as queued for d. Every batch is removed
// from the queue only after its insert succeeded and a delivery checkpoint
// covers it (confirmDelivered), so an interrupted or failed
// retry leaves the remaining rows queued for d and can simply be repeated;
// rows sent twice carry the same keys and versions. Retry schedules are
// ignored and delivery stops at the first failure, which is recorded on the
// failed entries.
func (c *Collector) DeliverRetained(ctx context.Context, d spool.Destination) (DrainResult, error) {
	res := DrainResult{Remaining: d.Pending}
	if d.ID <= 0 || d.Endpoint != c.opts.Endpoint || d.Database != c.opts.Database {
		return res, fmt.Errorf("collector: %s is not the configured ClickHouse database", d)
	}
	selected := c.store.Selected()
	c.store.SelectDestination(d.ID)
	defer c.store.SelectDestination(selected)

	err := c.drainRetained(ctx, d, &res)
	if cur, ok, derr := c.store.Destination(context.WithoutCancel(ctx), d.ID); derr == nil && ok {
		res.Remaining = cur.Pending
	}
	return res, err
}

// checkRetained is the destination check of DeliverRetained: the configured
// database must report the destination id recorded for d and hold the
// delivery checkpoint d acknowledged, the same rules as for the collector's
// own destination. Nothing is recorded about the active destination.
func (c *Collector) checkRetained(ctx context.Context, d spool.Destination) error {
	info, err := c.sink.CheckDestination(ctx)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrDestinationCheck, err)
	}
	if err := c.store.BindServerID(ctx, d.ID, info.ServerID); err != nil {
		return err
	}
	if err := c.verifyCheckpoint(ctx, d, info, false); err != nil {
		var de *DestinationError
		if errors.As(err, &de) {
			return err
		}
		return fmt.Errorf("%w: %w", ErrDestinationCheck, err)
	}
	return nil
}

func (c *Collector) drainRetained(ctx context.Context, d spool.Destination, res *DrainResult) error {
	var checked time.Time
	for {
		if time.Since(checked) >= identityCheckInterval {
			if err := c.checkRetained(ctx, d); err != nil {
				return err
			}
			checked = time.Now()
		}
		entries, err := c.store.Due(ctx, time.Now().Add(365*24*time.Hour), c.opts.MaxInsertRows)
		if err != nil || len(entries) == 0 {
			return err
		}
		order, groups := byTable(entries)
		var sent []spool.Entry
		sentRows := 0
		var failed error
		for _, table := range order {
			rows, err := c.insertGroup(ctx, table, groups[table])
			if err == nil {
				sent = append(sent, groups[table]...)
				sentRows += rows
				continue
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			_ = c.store.Fail(context.WithoutCancel(ctx), entryIDs(groups[table]), time.Now(), err.Error())
			failed = fmt.Errorf("%s: %w", table, err)
			break
		}
		if len(sent) > 0 {
			if err := c.confirmDelivered(ctx, d, sent, false); err != nil {
				var fe *fatalError
				if errors.As(err, &fe) {
					return fe.err
				}
				if ctx.Err() != nil {
					return ctx.Err()
				}
				_ = c.store.Fail(context.WithoutCancel(ctx), entryIDs(sent), time.Now(), err.Error())
				return err
			}
			res.Delivered += sentRows
		}
		if failed != nil {
			return failed
		}
	}
}
