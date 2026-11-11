// Package collector runs the ingestion pipeline: sources are synchronized into
// the local spool, and a delivery loop drains the spool into ClickHouse. The
// foreground command and the background service both use Run.
package collector

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/nedanwr/vario/internal/model"
	"github.com/nedanwr/vario/internal/sources"
	"github.com/nedanwr/vario/internal/spool"
	"github.com/nedanwr/vario/internal/storage/clickhouse"
)

// Sink is the delivery destination. *clickhouse.Client implements it with
// the operational user. The collector never changes the destination's
// schema, users or privileges: CheckDestination only reads, and a missing or
// incompatible schema stops delivery (data stays spooled) until it is fixed
// with `vario migrate`. Besides the rows themselves, the collector only
// writes its delivery checkpoint (ReadCheckpoint, WriteCheckpoint), which is
// what reveals a database that was restored to an earlier state.
type Sink interface {
	CheckDestination(ctx context.Context) (clickhouse.DestinationInfo, error)
	Insert(ctx context.Context, table string, gzBody []byte, token string) error
	// ReadCheckpoint returns the highest checkpoint the database holds for writer.
	ReadCheckpoint(ctx context.Context, writer string) (seq uint64, found bool, err error)
	// WriteCheckpoint inserts checkpoint seq for writer; a nil error means
	// the database acknowledged it.
	WriteCheckpoint(ctx context.Context, writer string, seq uint64) error
}

type Options struct {
	PollInterval      time.Duration
	ReconcileInterval time.Duration
	BatchSize         int
	FlushInterval     time.Duration
	MaxInsertRows     int
	RetryInitial      time.Duration
	RetryMax          time.Duration
	PolicyFingerprint string
	// Endpoint (normalized, see config.ClickHouseConfig.Endpoint) and
	// Database identify the configured destination.
	Endpoint string
	Database string
}

type Collector struct {
	store   *spool.Store
	sink    Sink
	sources []sources.Source
	opts    Options
	log     *slog.Logger

	wake chan struct{}
	// schemaReady is set once the destination passed its check; checkedAt is
	// when it last did (see identityCheckInterval).
	schemaReady bool
	checkedAt   time.Time
	checkFailed bool
	// checkpoints is set by the destination check when the database has the
	// checkpoint table: delivered rows are then acknowledged only under a
	// checkpoint (see confirmDelivered).
	checkpoints bool
	// dest is the destination selected by Prepare.
	dest   spool.Destination
	warned map[string]bool
}

func New(store *spool.Store, sink Sink, srcs []sources.Source, opts Options, log *slog.Logger) *Collector {
	return &Collector{store: store, sink: sink, sources: srcs, opts: opts, log: log, wake: make(chan struct{}, 1), warned: map[string]bool{}}
}

// leaseTTL bounds how long a collector that stopped renewing (crashed on
// another host, or suspended) blocks others. Collectors that crash on this
// host are detected by PID and replaced immediately.
var leaseTTL = 30 * time.Second

// ErrLeaseLost is the cancellation cause when another process took the lease.
var ErrLeaseLost = errors.New("the collector lease was taken over by another vario process")

// Lease acquires the single-collector lease, waiting up to wait for the
// current holder to release or expire, and renews it until release is called.
// The returned context is cancelled with ErrLeaseLost if the lease is taken
// over (for example after this machine slept past the TTL), so that two
// collectors never keep running at the same time.
func Lease(ctx context.Context, store *spool.Store, command string, wait time.Duration) (context.Context, func(), error) {
	host, _ := os.Hostname()
	owner := fmt.Sprintf("%s/%d/%d", host, os.Getpid(), time.Now().UnixNano())
	deadline := time.Now().Add(wait)
	for {
		err := store.AcquireLease(ctx, owner, command, leaseTTL)
		if err == nil {
			break
		}
		if !errors.Is(err, spool.ErrLeaseHeld) || time.Now().After(deadline) {
			return nil, nil, err
		}
		select {
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		case <-time.After(min(2*time.Second, leaseTTL/3)):
		}
	}
	lctx, cancelLease := context.WithCancelCause(ctx)
	renewCtx, stopRenew := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		t := time.NewTicker(leaseTTL / 3)
		defer t.Stop()
		for {
			select {
			case <-renewCtx.Done():
				return
			case <-t.C:
				// Other errors (a busy database) are retried on the next tick; if
				// the lease expires meanwhile and is taken, the next renewal sees it.
				if err := store.AcquireLease(renewCtx, owner, command, leaseTTL); errors.Is(err, spool.ErrLeaseHeld) {
					cancelLease(ErrLeaseLost)
					return
				}
			}
		}
	}()
	return lctx, func() {
		stopRenew()
		<-done
		cancelLease(context.Canceled)
		_ = store.ReleaseLease(context.Background(), owner)
	}, nil
}

// Run synchronizes all sources and delivers to the sink until ctx is
// cancelled. Prepare must have succeeded first.
func (c *Collector) Run(ctx context.Context) error {
	if c.dest.ID == 0 {
		return errors.New("collector: destination not prepared")
	}
	ctx, stop := context.WithCancelCause(ctx)
	defer stop(nil)
	_ = c.store.SetKV(ctx, "collector.started_at", time.Now().UTC().Format(time.RFC3339))
	_ = c.store.SetKV(ctx, "collector.pid", strconv.Itoa(os.Getpid()))
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := c.deliverLoop(ctx); err != nil {
			stop(err)
		}
	}()
	for _, src := range c.sources {
		wg.Add(1)
		go func(src sources.Source) {
			defer wg.Done()
			c.sourceLoop(ctx, src)
		}(src)
	}
	wg.Wait()
	_ = c.store.SetKV(context.Background(), "collector.stopped_at", time.Now().UTC().Format(time.RFC3339))
	var fe *fatalError
	if cause := context.Cause(ctx); errors.As(cause, &fe) {
		return fe.err
	}
	return nil
}

func (c *Collector) notify() {
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

func key(src sources.Source, name string) string { return "source." + src.ID() + "." + name }

func (c *Collector) sourceLoop(ctx context.Context, src sources.Source) {
	log := c.log.With("source", src.ID())
	var failures int
	lastErr := ""
	for {
		err := c.syncOnce(ctx, src, log)
		wait := c.opts.PollInterval
		switch {
		case ctx.Err() != nil:
			return
		case err == nil:
			if failures > 0 {
				log.Info("source sync recovered")
			}
			failures, lastErr = 0, ""
			_ = c.store.SetKV(ctx, key(src, "last_sync"), time.Now().UTC().Format(time.RFC3339))
			_ = c.store.SetKV(ctx, key(src, "last_error"), "")
		case errors.Is(err, spool.ErrFull):
			log.Warn("spool full; pausing collection until delivery catches up")
			wait = max(c.opts.PollInterval, 10*time.Second)
		default:
			failures++
			if msg := err.Error(); msg != lastErr {
				log.Error("source sync failed; will retry", "error", msg)
				lastErr = msg
			}
			_ = c.store.SetKV(ctx, key(src, "last_error"), err.Error())
			wait = backoff(c.opts.PollInterval, time.Minute, failures)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

// syncOnce performs a snapshot when required and then drains pending changes.
func (c *Collector) syncOnce(ctx context.Context, src sources.Source, log *slog.Logger) error {
	cur, ok, err := c.store.Cursor(ctx, src.ID())
	if err != nil {
		return err
	}
	if !ok {
		log.Info("no cursor; running initial backfill")
		_, err := c.Snapshot(ctx, src, false, true)
		return err
	}
	if c.opts.ReconcileInterval > 0 {
		_, at, found, err := c.store.KV(ctx, key(src, "last_snapshot"))
		if err != nil {
			return err
		}
		if !found || time.Since(at) >= c.opts.ReconcileInterval {
			log.Info("running periodic reconciliation")
			if _, err := c.Snapshot(ctx, src, false, false); err != nil {
				return err
			}
			return nil
		}
	}
	for ctx.Err() == nil {
		cs, err := src.Changes(ctx, cur, c.opts.BatchSize)
		if err != nil {
			return err
		}
		if cs.Reconcile != "" {
			log.Info("reconciling source state", "reason", cs.Reconcile)
			_, err := c.Snapshot(ctx, src, false, false)
			return err
		}
		if cs.Cursor == cur && len(cs.Records) == 0 {
			return nil
		}
		res, err := c.store.Commit(ctx, src.ID(), cs.Records, &cs.Cursor, c.commitOpts(false))
		if err != nil {
			return err
		}
		if res.Enqueued > 0 {
			log.Debug("changes queued", "records", res.Enqueued, "unchanged", res.Skipped)
			c.notify()
		}
		cur = cs.Cursor
		if !cs.More {
			return nil
		}
	}
	return ctx.Err()
}

func (c *Collector) commitOpts(force bool) spool.CommitOptions {
	return spool.CommitOptions{PolicyFingerprint: c.opts.PolicyFingerprint, Force: force, MaxRowsPerEntry: c.opts.MaxInsertRows}
}

type SnapshotResult struct {
	Enqueued int
	Skipped  int
	Duration time.Duration
}

// Snapshot reads the source's full state into the spool and then advances its
// cursor. If interrupted, the cursor is untouched and the next run repeats the
// snapshot; already-queued records are skipped by fingerprint.
func (c *Collector) Snapshot(ctx context.Context, src sources.Source, force, includeHistory bool) (SnapshotResult, error) {
	start := time.Now()
	var res SnapshotResult
	prev, _, err := c.store.Cursor(ctx, src.ID())
	if err != nil {
		return res, err
	}
	next, err := src.Snapshot(ctx, prev, includeHistory, func(recs []model.Record) error {
		r, err := c.store.Commit(ctx, src.ID(), recs, nil, c.commitOpts(force))
		res.Enqueued += r.Enqueued
		res.Skipped += r.Skipped
		if r.Enqueued > 0 {
			c.notify()
		}
		return err
	})
	if err != nil {
		return res, err
	}
	if _, err := c.store.Commit(ctx, src.ID(), nil, &next, c.commitOpts(force)); err != nil {
		return res, err
	}
	_ = c.store.SetKV(ctx, key(src, "last_snapshot"), time.Now().UTC().Format(time.RFC3339))
	res.Duration = time.Since(start)
	c.log.Info("snapshot complete", "source", src.ID(), "queued", res.Enqueued, "unchanged", res.Skipped, "duration", res.Duration.Round(time.Millisecond))
	return res, nil
}

func backoff(initial, maxWait time.Duration, attempt int) time.Duration {
	d := initial
	for i := 1; i < attempt && d < maxWait; i++ {
		d *= 2
	}
	d = min(d, maxWait)
	// Up to 20% jitter keeps multiple failing entries from retrying in lockstep.
	jitter := time.Duration(time.Now().UnixNano() % int64(d/5+1))
	return d - d/10 + jitter
}
