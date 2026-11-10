package collector

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/nedanwr/vario/internal/spool"
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
}

func (e *DestinationError) Error() string {
	var b strings.Builder
	if e.Replaced {
		fmt.Fprintf(&b, "the ClickHouse database at %s is not the one Vario delivered to before: it carries a different destination id, so it was recreated or the URL now leads to another database.\n", e.Configured)
		fmt.Fprintf(&b, "Vario will not assume that previously delivered data exists there")
		if e.Configured.Pending > 0 {
			fmt.Fprintf(&b, ", and keeps %d queued rows until you decide what to do with them", e.Configured.Pending)
		}
		b.WriteString(".\n")
	} else {
		fmt.Fprintf(&b, "the ClickHouse destination changed:\n  previous: %s (%d rows queued, not yet delivered)\n  current:  %s\n", e.Previous, e.Previous.Pending, e.Configured)
		b.WriteString("Vario will not send data queued for the previous destination to the current one.\n")
	}
	b.WriteString(`Run "vario destination" for details, then one of:
  vario destination switch --keep-pending      use the configured destination; queued rows stay with the previous one
  vario destination switch --move-pending      also deliver the queued rows to the configured destination
  vario destination switch --discard-pending   delete the queued rows (the source is re-read for the new destination anyway)`)
	if !e.Replaced {
		b.WriteString("\nOr restore the previous clickhouse.url / clickhouse.database to deliver the queued rows there first.")
	}
	return b.String()
}

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
func (c *Collector) Prepare(ctx context.Context, probe bool) (spool.Resolution, error) {
	serverID := ""
	if probe {
		pctx, cancel := context.WithTimeout(ctx, probeTimeout)
		info, err := c.sink.CheckDestination(pctx)
		cancel()
		if err == nil {
			serverID = info.ServerID
			c.noteWarnings(info.Warnings)
			c.schemaReady = true
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
		return res, &DestinationError{Configured: res.Configured, Replaced: true}
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
// binds the destination id. It returns errFatal-wrapped errors for an
// identity mismatch, which stops the collector.
func (c *Collector) checkDestination(ctx context.Context) error {
	info, err := c.sink.CheckDestination(ctx)
	if err != nil {
		_ = c.store.SetKV(ctx, "delivery.last_error", "destination: "+err.Error())
		return fmt.Errorf("destination check: %w", err)
	}
	if err := c.store.BindServerID(ctx, c.dest.ID, info.ServerID); err != nil {
		if errors.Is(err, spool.ErrIdentityMismatch) {
			d := c.dest
			_ = c.store.SetKV(ctx, "delivery.last_error", "destination: "+err.Error())
			return &fatalError{&DestinationError{Configured: d, Replaced: true}}
		}
		return err
	}
	c.noteWarnings(info.Warnings)
	return nil
}

// fatalError stops the collector instead of being retried.
type fatalError struct{ err error }

func (e *fatalError) Error() string { return e.err.Error() }
func (e *fatalError) Unwrap() error { return e.err }
