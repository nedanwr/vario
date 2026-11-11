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
// failures that must stop the collector (a replaced destination).
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
	// longer holds what was delivered before. Read-only: the collector never
	// creates or migrates the schema.
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
	groups := map[string][]spool.Entry{}
	var order []string
	for _, e := range entries {
		if _, ok := groups[e.Table]; !ok {
			order = append(order, e.Table)
		}
		groups[e.Table] = append(groups[e.Table], e)
	}
	delivered := 0
	var firstErr error
	for _, table := range order {
		group := groups[table]
		rows, err := c.insertGroup(ctx, table, group)
		if err == nil {
			delivered += rows
			continue
		}
		if ctx.Err() != nil {
			return delivered, ctx.Err()
		}
		attempts := group[0].Attempts + 1
		wait := backoff(c.opts.RetryInitial, c.opts.RetryMax, attempts)
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
		ids := make([]int64, len(group))
		for i, e := range group {
			ids[i] = e.ID
		}
		_ = c.store.Fail(context.WithoutCancel(ctx), ids, time.Now().Add(wait), err.Error())
		_ = c.store.SetKV(ctx, "delivery.last_error", table+": "+err.Error())
		if firstErr == nil {
			firstErr = fmt.Errorf("%s: %w", table, err)
		}
	}
	if delivered > 0 {
		_ = c.store.SetKV(ctx, "delivery.last_success", strconv.Itoa(delivered))
		if firstErr == nil {
			_ = c.store.SetKV(ctx, "delivery.last_error", "")
		}
		c.log.Debug("delivered", "rows", delivered)
	}
	return delivered, firstErr
}

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
	ids := make([]int64, len(group))
	for i, e := range group {
		ids[i] = e.ID
	}
	// The insert succeeded; acknowledge even if ctx was cancelled meanwhile.
	if err := c.store.Ack(context.WithoutCancel(ctx), ids); err != nil {
		return 0, err
	}
	return rows, nil
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
