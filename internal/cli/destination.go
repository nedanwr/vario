package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/nedanwr/vario/internal/collector"
	"github.com/nedanwr/vario/internal/config"
	"github.com/nedanwr/vario/internal/spool"
	"github.com/nedanwr/vario/internal/storage/clickhouse"
)

func (a *app) destinationCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "destination",
		Short: "Show which ClickHouse destination queued data belongs to; handle destination changes",
		Long: `Every queued record, source cursor and fingerprint belongs to one ClickHouse
destination (endpoint and database). Data queued for one destination is never
delivered to another, and a new destination has its own cursors, so every
source is read again in full for it.

A destination is identified by the destination id that 'vario migrate' stores
in the database, or, until that is known, by its normalized endpoint and
database. Credentials are not part of it: rotating the password or changing
the user keeps the destination. The same database under a new URL is
recognized by its id; a dropped and recreated database is a new destination.

A database that keeps its id but was restored to an earlier state (from a
backup, for example) is recognized by its delivery checkpoint: the collector
records in the database how far it has delivered, and a database that holds
less than it acknowledged no longer has everything that was sent to it.

Without arguments, destination lists the known destinations. If the
configured destination changes while rows are still queued for the previous
one, or the configured database was replaced or restored to an earlier state,
the collector does not start until you decide with 'vario destination switch'.

Rows kept for a previous destination ('switch --keep-pending') are not
delivered while that destination is not in use. 'vario destination pending'
shows them and retries, moves or discards them.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return a.destinationStatus(cmd.Context()) },
	}
	cmd.AddCommand(a.destinationSwitchCmd(), a.destinationPendingCmd())
	return cmd
}

func (a *app) destinationStatus(ctx context.Context) error {
	cfg, err := a.loadConfig()
	if err != nil {
		return err
	}
	w := a.stdout
	fmt.Fprintf(w, "Configured  %s, database %s (user %s)\n", cfg.ClickHouse.Endpoint(), cfg.ClickHouse.Database, cfg.ClickHouse.Username)
	store, err := spool.OpenReadOnly(statePath(cfg))
	if err != nil {
		fmt.Fprintln(w, "\nNo local state yet.")
		return nil
	}
	defer store.Close()
	dests, err := store.Destinations(ctx)
	if err != nil {
		return err
	}
	active, hasActive, err := store.Active(ctx)
	if err != nil {
		return err
	}
	configured, hasConfigured, err := store.Lookup(ctx, cfg.ClickHouse.Endpoint(), cfg.ClickHouse.Database)
	if err != nil {
		return err
	}
	if len(dests) == 0 {
		fmt.Fprintln(w, "\nNo destinations recorded yet.")
		return nil
	}
	fmt.Fprintln(w, "\nKnown destinations")
	for _, d := range dests {
		var tags []string
		if hasActive && d.ID == active.ID {
			tags = append(tags, "active")
		}
		if hasConfigured && d.ID == configured.ID {
			tags = append(tags, "configured")
		}
		if !d.Retired.IsZero() {
			tags = append(tags, "replaced")
		}
		label := ""
		if len(tags) > 0 {
			label = " [" + joinComma(tags) + "]"
		}
		id := fmt.Sprintf("#%d", d.ID)
		if d.ID < 0 {
			id = "legacy"
		}
		fmt.Fprintf(w, "  %s  %s%s\n", id, d, label)
		fmt.Fprintf(w, "        queued rows %d · sources with a cursor %d · destination id %s\n", d.Pending, d.Cursors, orDash(d.ServerID))
	}
	if store.Legacy() {
		fmt.Fprintln(w, "\nThis state was written by an earlier Vario version. The next `vario run` or `vario backfill` assigns it to the configured destination.")
		return nil
	}
	switch {
	case hasActive && (!hasConfigured || configured.ID != active.ID) && active.Pending > 0:
		fmt.Fprintf(w, "\nThe configured destination differs from the one the collector last delivered to, which still has %d queued rows.\nThe collector will not start until you choose with `vario destination switch` (--keep-pending, --move-pending or --discard-pending).\n", active.Pending)
	case hasActive && (!hasConfigured || configured.ID != active.ID):
		fmt.Fprintln(w, "\nThe configured destination differs from the one the collector last delivered to. Nothing is queued for that one, so the collector switches automatically and reads every source again in full for the configured destination.")
	}
	if rep, ok, _ := store.Replaced(ctx); ok && hasConfigured && rep.Destination.ID == configured.ID {
		fmt.Fprint(w, "\n"+replacementNotice(rep, ""))
	}
	if rb, ok, _ := store.RolledBack(ctx); ok && hasConfigured && rb.Destination.ID == configured.ID {
		fmt.Fprint(w, "\n"+rollbackNotice(rb, ""))
	}
	for _, d := range dests {
		if d.Pending > 0 && (!hasActive || d.ID != active.ID) && (!hasConfigured || d.ID != configured.ID) {
			fmt.Fprintf(w, "\n! %d rows are queued for %s and are not delivered while it is not in use.\n  Inspect, retry, move or discard them with `vario destination pending %d`.\n", d.Pending, d, d.ID)
		}
	}
	return nil
}

// replacementNotice reports a detected database replacement that still
// awaits a decision, with the recovery steps, each line prefixed by indent.
func replacementNotice(rep spool.Replacement, indent string) string {
	d := rep.Destination
	var b strings.Builder
	fmt.Fprintf(&b, "! DATABASE REPLACED (detected %s): %s now carries destination id %s; Vario delivered to %s there.\n",
		rep.At.Local().Format(time.DateTime), d, rep.Found, d.ServerID)
	fmt.Fprintf(&b, "Delivery is suspended and the collector does not start. %d queued rows are kept in the local spool; nothing was changed in ClickHouse.\n", d.Pending)
	b.WriteString(collector.ReplacedRecovery)
	return indent + strings.ReplaceAll(b.String(), "\n", "\n"+indent) + "\n"
}

// rollbackNotice reports a detected database rollback that still awaits a
// decision, with the recovery steps, each line prefixed by indent.
func rollbackNotice(rb spool.Rollback, indent string) string {
	d := rb.Destination
	var b strings.Builder
	fmt.Fprintf(&b, "! DATABASE RESTORED TO AN EARLIER STATE (detected %s): %s acknowledged delivery checkpoint %d, but held %s. It carries the recorded destination id %s, so rows delivered after that point are missing.\n",
		rb.At.Local().Format(time.DateTime), d, rb.Confirmed, heldCheckpoint(rb.Found), orDash(d.ServerID))
	fmt.Fprintf(&b, "Delivery is suspended and the collector does not start. %d queued rows are kept in the local spool; nothing was changed in ClickHouse.\n", d.Pending)
	b.WriteString(collector.RollbackRecovery)
	return indent + strings.ReplaceAll(b.String(), "\n", "\n"+indent) + "\n"
}

func heldCheckpoint(seq uint64) string {
	if seq == 0 {
		return "none"
	}
	return fmt.Sprintf("only checkpoint %d", seq)
}

func joinComma(s []string) string {
	out := ""
	for i, x := range s {
		if i > 0 {
			out += ", "
		}
		out += x
	}
	return out
}

type pendingAction int

const (
	pendingUnset pendingAction = iota
	pendingKeep
	pendingMove
	pendingDiscard
)

func (a *app) destinationSwitchCmd() *cobra.Command {
	var keep, move, discard, yes bool
	cmd := &cobra.Command{
		Use:   "switch",
		Short: "Start delivering to the configured destination",
		Long: `Switch makes the configured ClickHouse destination the active one. Use it
after changing clickhouse.url or clickhouse.database while rows are still
queued for the previous destination, or after the configured database was
dropped and recreated (a new destination id).

Switch is also how a database that was restored to an earlier state is
accepted (same destination id, a lower delivery checkpoint than the collector
had acknowledged). No choice about queued rows is needed then: they belong to
this database and are delivered to it. Its cursors and fingerprints are reset,
so the next run reads every source again in full and delivers the complete
history; rows the database still has are sent again with the same keys and
versions and count once.

Choose what happens to the rows queued for the previous destination:

  --keep-pending     keep them for the previous destination; they are not
                     delivered until you configure it again or decide with
                     'vario destination pending' (retry, move or discard)
  --move-pending     deliver them to the configured destination instead
                     (re-sent rows carry the same keys and versions, so the
                     *_latest views count each record once)
  --discard-pending  delete them (asks for confirmation unless --yes)

The configured destination keeps its own cursors: if it has none, every source
is read again in full for it.

Moving or deleting queued rows means the previous destination never receives
them, so its cursors and fingerprints are reset as well: if you configure it
again later, every source is read again in full for it and nothing is missing.
With --keep-pending its state is kept and it resumes where it left off.

Switch needs the collector to be stopped, so that nothing is collected or
delivered while queued rows are reassigned. If a collector is running, switch
changes nothing and exits immediately, naming the process. For the
background service:

  vario service uninstall           # stops it; configuration, queued rows and cursors are kept
  vario destination switch --keep-pending
  vario service install             # starts it again`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			n := 0
			action := pendingUnset
			for f, act := range map[*bool]pendingAction{&keep: pendingKeep, &move: pendingMove, &discard: pendingDiscard} {
				if *f {
					n++
					action = act
				}
			}
			if n > 1 {
				return withCode(ExitUsage, errors.New("use only one of --keep-pending, --move-pending and --discard-pending"))
			}
			return a.destinationSwitch(cmd.Context(), action, yes)
		},
	}
	cmd.Flags().BoolVar(&keep, "keep-pending", false, "keep queued rows for the previous destination")
	cmd.Flags().BoolVar(&move, "move-pending", false, "deliver queued rows to the configured destination")
	cmd.Flags().BoolVar(&discard, "discard-pending", false, "delete rows queued for the previous destination")
	cmd.Flags().BoolVar(&yes, "yes", false, "do not ask for confirmation")
	return cmd
}

func (a *app) destinationSwitch(ctx context.Context, action pendingAction, yes bool) error {
	cfg, err := a.loadConfig()
	if err != nil {
		return err
	}
	w := a.stdout
	store, err := openStore(cfg)
	if err != nil {
		return err
	}
	defer store.Close()
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	_, release, err := exclusive(ctx, store, "destination switch", "vario destination switch"+switchFlag(action))
	if err != nil {
		return err
	}
	defer release()

	serverID, unidentified := "", ""
	var client *clickhouse.Client
	var info clickhouse.DestinationInfo
	if cl, err := newClient(cfg); err == nil {
		client = cl
		pctx, cancel := context.WithTimeout(ctx, checkTimeout(cfg.ClickHouse))
		info, err = client.CheckDestination(pctx)
		cancel()
		switch kind := clickhouse.Classify(err); {
		case err != nil:
			unidentified = fmt.Sprintf("it cannot be checked now (%s): %v\n%s", kind, err, clickhouse.Hint(kind))
		case info.ServerID == "":
			unidentified = "it reports no destination id (a schema without " + clickhouse.MetaTable + "); run `vario migrate` to record one"
		default:
			serverID = info.ServerID
		}
		// A replaced database must be identified before it is accepted:
		// otherwise this switch would change nothing and still report success.
		if rep, ok, _ := store.Replaced(ctx); ok && unidentified != "" && rep.Destination.Endpoint == cfg.ClickHouse.Endpoint() && rep.Destination.Database == cfg.ClickHouse.Database {
			return withCode(ExitFailure, fmt.Errorf("the database at %s was replaced (Vario delivered to destination id %s there), but %s\nNothing was changed; queued rows are kept. Run this command again once the database can be identified",
				rep.Destination, rep.Destination.ServerID, unidentified))
		}
		// Likewise a database that was restored to an earlier state: accepting
		// it needs the database it is about.
		if rb, ok, _ := store.RolledBack(ctx); ok && unidentified != "" && configuredAt(cfg, rb.Destination) {
			return withCode(ExitFailure, fmt.Errorf("the database at %s was restored to an earlier state (it acknowledged delivery checkpoint %d), but %s\nNothing was changed; queued rows are kept. Run this command again once the database can be identified",
				rb.Destination, rb.Confirmed, unidentified))
		}
		if err != nil {
			fmt.Fprintf(w, "! The destination could not be checked (%s): it is identified by its endpoint and database only.\n", clickhouse.Classify(err))
		}
	}
	res, err := store.Resolve(ctx, cfg.ClickHouse.Endpoint(), cfg.ClickHouse.Database, serverID)
	if err != nil {
		return err
	}
	target, prev := res.Configured, res.Previous
	if res.Replaced {
		fmt.Fprintf(w, "The database at %s carries destination id %s, but Vario delivered to %s there before: it was recreated, or the URL leads to another database.\n",
			target, serverID, target.ServerID)
		old := target
		if target, err = store.Replace(ctx, old.ID, serverID); err != nil {
			return err
		}
		old.Retired = time.Now()
		prev = &old
		fmt.Fprintln(w, "It is recorded as a new destination without cursors, so every source is read again in full for it; the previous state is kept as \"replaced\".")
	}
	// The same database, but does it still hold what was delivered to it?
	recovered := false
	if !res.Replaced && serverID != "" && target.ServerID == serverID {
		if recovered, err = acceptRollback(ctx, w, cfg, store, client, info, target); err != nil {
			return err
		}
	}
	if prev == nil {
		if err := store.Activate(ctx, target.ID); err != nil {
			return err
		}
		if recovered {
			a.serviceNotice(cfg.Path(), true)
			return nil
		}
		fmt.Fprintf(w, "Already using %s; nothing to do.\n", target)
		return nil
	}
	if prev.Pending == 0 {
		action = pendingKeep
	}
	p := a.migratePrompter(false)
	if action == pendingUnset {
		if !p.interactive {
			return withCode(ExitUsage, fmt.Errorf("%d rows are queued for %s; choose --keep-pending, --move-pending or --discard-pending", prev.Pending, prev))
		}
		c, err := p.choose(fmt.Sprintf("%d rows are queued for %s.\nWhat should happen to them?", prev.Pending, prev), []string{
			"Keep them for that destination (decide later with `vario destination pending`)",
			"Deliver them to " + target.String() + " instead",
			"Delete them",
			"Cancel",
		}, 0)
		if err != nil {
			return err
		}
		if c == 3 {
			fmt.Fprintln(w, "Nothing was changed.")
			return nil
		}
		action = []pendingAction{pendingKeep, pendingMove, pendingDiscard}[c]
	}
	if action == pendingDiscard && !yes {
		if !p.interactive {
			return withCode(ExitUsage, errors.New("--discard-pending deletes queued data; confirm with --yes"))
		}
		ok, err := p.confirm(fmt.Sprintf("Permanently delete %d queued rows for %s?", prev.Pending, prev), false)
		if err != nil {
			return err
		}
		if !ok {
			fmt.Fprintln(w, "Nothing was changed.")
			return nil
		}
	}
	switch action {
	case pendingMove:
		n, err := store.MovePending(ctx, prev.ID, target.ID)
		if err != nil {
			return err
		}
		fmt.Fprintf(w, "✓ %d queued rows will be delivered to %s\n", n, target)
		resyncNotice(w, n, *prev)
	case pendingDiscard:
		n, err := store.DiscardPending(ctx, prev.ID)
		if err != nil {
			return err
		}
		fmt.Fprintf(w, "✓ deleted %d queued rows for %s\n", n, prev)
		resyncNotice(w, n, *prev)
	default:
		if prev.Pending > 0 {
			fmt.Fprintf(w, "✓ %d queued rows stay with %s\n", prev.Pending, prev)
			fmt.Fprintf(w, "  They are not delivered while it is not in use. Inspect, retry, move or discard them with `vario destination pending %d`.\n", prev.ID)
		}
	}
	if err := store.Activate(ctx, target.ID); err != nil {
		return err
	}
	fmt.Fprintf(w, "✓ active destination: %s\n", target)
	if target.Cursors == 0 {
		fmt.Fprintln(w, "  It has no synchronization state yet: the next `vario run` (or `vario backfill --source t3code`) reads every source in full for it.")
	}
	a.serviceNotice(cfg.Path(), true)
	return nil
}

// acceptRollback handles a configured database that carries the recorded
// destination id but holds a lower delivery checkpoint than the spool
// recorded as acknowledged: it was restored to an earlier state. Running
// `vario destination switch` is the explicit decision to keep that database,
// so its synchronization state is reset (spool.AcceptRollback) and the next
// collector run delivers the complete history again. It compares with the
// database as it is now, so it also handles a rollback no collector has seen
// yet, and withdraws a recorded one when the database holds the checkpoint
// again. It reports whether a rollback was accepted.
func acceptRollback(ctx context.Context, w io.Writer, cfg *config.Config, store *spool.Store, client *clickhouse.Client, info clickhouse.DestinationInfo, target spool.Destination) (bool, error) {
	cp, err := store.Checkpoint(ctx, target.ID)
	if err != nil {
		return false, err
	}
	if cp.Confirmed == 0 {
		return false, nil // no checkpoint was acknowledged: nothing to compare with
	}
	var found uint64
	if info.Checkpoints {
		pctx, cancel := context.WithTimeout(ctx, checkTimeout(cfg.ClickHouse))
		found, _, err = client.ReadCheckpoint(pctx, cp.Writer)
		cancel()
		if err != nil {
			return false, withCode(ExitFailure, fmt.Errorf("the delivery checkpoint of %s could not be read (%s): %v\nNothing was changed; queued rows are kept. Run this command again once it can be read",
				target, clickhouse.Classify(err), err))
		}
	}
	if found >= cp.Confirmed {
		if rb, ok, _ := store.RolledBack(ctx); ok && rb.Destination.ID == target.ID {
			if err := store.ClearRolledBack(ctx); err != nil {
				return false, err
			}
			_ = store.SetKV(ctx, "delivery.last_error", "")
			fmt.Fprintf(w, "✓ %s holds the acknowledged delivery checkpoint %d again: nothing to recover, delivery resumes at the next start.\n", target, cp.Confirmed)
		}
		return false, nil
	}
	fmt.Fprintf(w, "The database at %s carries the recorded destination id %s, but it acknowledged delivery checkpoint %d and now holds %s: it was restored to an earlier state and lacks rows delivered after that point.\n",
		target, target.ServerID, cp.Confirmed, heldCheckpoint(found))
	if err := store.AcceptRollback(ctx, target.ID); err != nil {
		return false, err
	}
	_ = store.SetKV(ctx, "delivery.last_error", "") // the suspension it reported is over
	fmt.Fprintln(w, "✓ accepted the restored database: its cursors and fingerprints were reset")
	fmt.Fprintln(w, "  The next `vario run` (or `vario backfill --source t3code`) reads every source again in full and delivers the complete history.")
	fmt.Fprintln(w, "  Rows the database still has are sent again with the same keys and versions, so the *_latest and usage views count each record once.")
	if target.Pending > 0 {
		fmt.Fprintf(w, "  %d queued rows are kept and delivered to it as well.\n", target.Pending)
	}
	return true, nil
}

// resyncNotice says that a destination whose queued rows were moved or
// deleted lost its synchronization state (see spool.MovePending).
func resyncNotice(w io.Writer, rows int64, prev spool.Destination) {
	if rows > 0 && prev.Retired.IsZero() {
		fmt.Fprintf(w, "  %s did not receive them: its cursors were reset, so every source is read again in full if you configure it again.\n", prev)
	}
}

// destinationNotice explains, after a configuration change, what happens to
// queued data when the configured destination no longer matches the one the
// collector delivered to.
//
// State written by an earlier Vario version records no destination; the
// collector would assign it to whatever is configured at its next start. If
// the destination changes before that, the state is first labelled with the
// previous configuration (prev), so it is not sent to the new destination.
func destinationNotice(w io.Writer, prev, next *config.Config) {
	if prev != nil && prev.ClickHouse.Endpoint() == next.ClickHouse.Endpoint() && prev.ClickHouse.Database == next.ClickHouse.Database {
		return
	}
	ctx := context.Background()
	path := statePath(next)
	store, err := spool.OpenReadOnly(path)
	if err != nil {
		return
	}
	active, ok, err := store.Active(ctx)
	if err == nil && ok && active.Legacy() && prev != nil {
		store.Close()
		if store, err = spool.Open(path, 0); err != nil {
			fmt.Fprintf(w, "\n! Local state from the previous Vario version could not be bound to the previous destination: %v\n"+
				"  Do not start the collector before checking `vario destination`.\n", err)
			return
		}
		if err := store.LabelLegacy(ctx, prev.ClickHouse.Endpoint(), prev.ClickHouse.Database); err != nil {
			fmt.Fprintf(w, "\n! %v\n", err)
		}
		active, ok, err = store.Active(ctx)
	}
	defer store.Close()
	if err != nil || !ok || (active.Endpoint == next.ClickHouse.Endpoint() && active.Database == next.ClickHouse.Database) {
		return
	}
	fmt.Fprintf(w, "\nThe ClickHouse destination changed (the collector last delivered to %s).\n", active)
	if active.Pending > 0 {
		fmt.Fprintf(w, "  %d queued rows belong to that destination and will NOT be sent to the new one.\n"+
			"  The collector does not start until you choose with `vario destination switch` (see `vario destination`),\n"+
			"  or restore the previous setting to deliver them first.\n", active.Pending)
		return
	}
	fmt.Fprintln(w, "  Nothing is queued for it. The new destination has its own synchronization state, so the\n"+
		"  collector reads every source again in full for it; the previous state is kept if you switch back.")
}

func switchFlag(action pendingAction) string {
	switch action {
	case pendingKeep:
		return " --keep-pending"
	case pendingMove:
		return " --move-pending"
	case pendingDiscard:
		return " --discard-pending"
	}
	return " [--keep-pending | --move-pending | --discard-pending]"
}

// exclusive takes the collector lease for a command that reassigns, delivers
// or deletes queued rows outside the collector, so that nothing is collected
// or delivered meanwhile. A running collector holds the lease: the command
// then fails at once, naming it, instead of waiting, and Vario never stops a
// collector on its own. again is the command line to run once it is stopped.
// The returned context is cancelled if the lease is lost.
func exclusive(ctx context.Context, store *spool.Store, command, again string) (context.Context, func(), error) {
	if l, live, err := store.CurrentLease(ctx); err == nil && live {
		return nil, nil, collectorRunningError(l, again)
	}
	lctx, release, err := collector.Lease(ctx, store, command, 0)
	if errors.Is(err, spool.ErrLeaseHeld) {
		if l, live, lerr := store.CurrentLease(ctx); lerr == nil && live {
			return nil, nil, collectorRunningError(l, again)
		}
		return nil, nil, withCode(ExitFailure, fmt.Errorf("%w; stop it, then run this command again", err))
	}
	if err != nil {
		return nil, nil, err
	}
	return lctx, release, nil
}

// collectorRunningError explains which collector holds the lease and how to
// stop it before running the command again. Vario never stops a collector on
// its own.
func collectorRunningError(l spool.Lease, again string) error {
	host, _ := os.Hostname()
	service := false
	if mgr, err := serviceManager(); err == nil {
		if st, err := mgr.Status(); err == nil && st.Running && st.PID != 0 && st.PID == l.PID {
			service = true
		}
	}
	var b strings.Builder
	switch {
	case l.Host != "" && l.Host != host:
		fmt.Fprintf(&b, "a collector on host %s (`vario %s`, pid %d) holds the lease on this state directory until %s.\n"+
			"Stop that collector, or wait until its lease expires if it is no longer running, then run this command again.",
			l.Host, l.Command, l.PID, l.Expires.Local().Format("15:04:05"))
	case service:
		fmt.Fprintf(&b, "the background service is running the collector (pid %d). Nothing was changed.\n"+
			"Stop the service, run the command, and start it again:\n\n"+
			"  vario service uninstall   # stops it; configuration, queued rows and cursors are kept\n"+
			"  %s\n"+
			"  vario service install\n", l.PID, again)
	default:
		fmt.Fprintf(&b, "a collector is running on this computer (`vario %s`, pid %d). Nothing was changed.\n"+
			"Stop it (Ctrl-C in its terminal, or `kill %d`; queued rows and cursors are kept), then run:\n\n"+
			"  %s\n", l.Command, l.PID, l.PID, again)
		if mgr, err := serviceManager(); err == nil {
			if st, err := mgr.Status(); err == nil && st.Installed {
				b.WriteString("\nThe background service is installed as well; if it restarts the collector, stop it with `vario service uninstall` first and reinstall it afterwards.\n")
			}
		}
	}
	return withCode(ExitFailure, errors.New(strings.TrimRight(b.String(), "\n")))
}
