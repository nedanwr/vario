package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/nedanwr/vario/internal/collector"
	"github.com/nedanwr/vario/internal/config"
	"github.com/nedanwr/vario/internal/model"
	"github.com/nedanwr/vario/internal/privacy"
	"github.com/nedanwr/vario/internal/spool"
	"github.com/nedanwr/vario/internal/storage/clickhouse"
)

func (a *app) destinationPendingCmd() *cobra.Command {
	var retry, move, discard, yes bool
	cmd := &cobra.Command{
		Use:   "pending [id]",
		Short: "Show, retry, move or discard rows kept for a previous destination",
		Long: `Rows that 'vario destination switch --keep-pending' kept for a previous
destination stay in the local spool, bound to that destination. They are not
delivered while it is not in use, and never go anywhere else on their own.

Without an action, pending lists these rows per destination: its number (as
in 'vario destination'), endpoint, database and destination id, how many rows
are queued in which tables, and the last delivery failure. This only reads
local state and works while the collector runs.

With the number of a destination, choose what happens to its rows:

  --retry    deliver them to the database they were queued for. This works
             only if that database is the configured one (clickhouse.url and
             clickhouse.database) and still reports the recorded destination
             id; otherwise nothing is sent. The active destination and all
             cursors stay as they are.
  --move     deliver them to the active destination instead (the collector
             sends them at its next start). Re-sent rows carry the same keys
             and versions, so the *_latest views count each record once.
  --discard  delete them.

--move and --discard cannot be undone and ask for confirmation (--yes
confirms in advance). The previous destination then never receives these
rows, so its cursors and fingerprints are reset with them: if it is ever used
again, every source is read again in full for it.

Each action is applied in full or not at all, except that an interrupted or
failed --retry keeps the rows that were not delivered yet; run it again to
continue. Repeating a finished action changes nothing. Actions need the
collector to be stopped, like 'vario destination switch'.`,
		Example: `  vario destination pending
  vario destination pending 1 --retry
  vario destination pending 1 --move
  vario destination pending 1 --discard --yes`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			n, action := 0, ""
			for _, f := range []struct {
				set  bool
				name string
			}{{retry, "--retry"}, {move, "--move"}, {discard, "--discard"}} {
				if f.set {
					n++
					action = f.name
				}
			}
			if n > 1 {
				return withCode(ExitUsage, errors.New("use only one of --retry, --move and --discard"))
			}
			var id int64
			if len(args) == 1 {
				v, err := strconv.ParseInt(strings.TrimPrefix(args[0], "#"), 10, 64)
				if err != nil || v <= 0 {
					return withCode(ExitUsage, fmt.Errorf("%q is not a destination number (see `vario destination pending`)", args[0]))
				}
				id = v
			}
			if action == "" {
				if yes {
					return withCode(ExitUsage, errors.New("--yes confirms --move or --discard; choose one of them"))
				}
				return a.pendingStatus(cmd.Context(), id)
			}
			if id == 0 {
				return withCode(ExitUsage, fmt.Errorf("%s needs the number of the destination whose rows it applies to: vario destination pending <id> %s (see `vario destination pending`)", action, action))
			}
			return a.pendingAct(cmd.Context(), id, action, yes)
		},
	}
	cmd.Flags().BoolVar(&retry, "retry", false, "deliver the rows to the destination they were queued for")
	cmd.Flags().BoolVar(&move, "move", false, "deliver the rows to the active destination instead")
	cmd.Flags().BoolVar(&discard, "discard", false, "delete the rows")
	cmd.Flags().BoolVar(&yes, "yes", false, "do not ask for confirmation")
	return cmd
}

// configuredAt reports whether d is the database the configuration points to.
func configuredAt(cfg *config.Config, d spool.Destination) bool {
	return d.Endpoint == cfg.ClickHouse.Endpoint() && d.Database == cfg.ClickHouse.Database
}

// pendingStatus lists the rows queued for destinations other than the active
// one (id 0), or for one destination. It only reads local state.
func (a *app) pendingStatus(ctx context.Context, id int64) error {
	cfg, err := a.loadConfig()
	if err != nil {
		return err
	}
	w := a.stdout
	store, err := spool.OpenReadOnly(statePath(cfg))
	if err != nil {
		fmt.Fprintln(w, "No local state yet: nothing is queued.")
		return nil
	}
	defer store.Close()
	if store.Legacy() {
		fmt.Fprintln(w, "This state was written by an earlier Vario version and records no destinations yet; nothing is kept for a previous one.")
		return nil
	}
	dests, err := store.Destinations(ctx)
	if err != nil {
		return err
	}
	active, hasActive, err := store.Active(ctx)
	if err != nil {
		return err
	}
	var shown []spool.Destination
	for _, d := range dests {
		switch {
		case id != 0 && d.ID == id:
			shown = append(shown, d)
		case id == 0 && d.Pending > 0 && !(hasActive && d.ID == active.ID):
			shown = append(shown, d)
		}
	}
	if id != 0 && len(shown) == 0 {
		return withCode(ExitUsage, fmt.Errorf("there is no destination %d (see `vario destination`)", id))
	}
	if len(shown) == 0 {
		fmt.Fprintln(w, "No rows are kept for a previous destination.")
		if hasActive && active.Pending > 0 {
			fmt.Fprintf(w, "%d rows are queued for the active destination, %s (see `vario status`).\n", active.Pending, active)
		}
		return nil
	}
	if id == 0 {
		fmt.Fprintln(w, "Rows kept for destinations that are not in use")
	}
	for _, d := range shown {
		fmt.Fprintln(w)
		if err := pendingReport(ctx, w, store, cfg, d, active, hasActive); err != nil {
			return err
		}
	}
	fmt.Fprintln(w, "\nNothing is delivered elsewhere or deleted until you decide. --move and --discard ask for confirmation; every action needs the collector to be stopped.")
	return nil
}

func pendingReport(ctx context.Context, w io.Writer, store *spool.Store, cfg *config.Config, d, active spool.Destination, hasActive bool) error {
	fmt.Fprintf(w, "#%d  %s\n", d.ID, d)
	fmt.Fprintf(w, "    destination id  %s\n", orDash(d.ServerID))
	if d.Pending == 0 {
		fmt.Fprintln(w, "    queued          nothing")
		return nil
	}
	prev := store.Selected()
	store.SelectDestination(d.ID)
	st, err := store.Stats(ctx)
	store.SelectDestination(prev)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "    queued          %d rows in %d batches, oldest from %s\n", st.Rows, st.Entries, st.Oldest.Local().Format(time.DateTime))
	var parts []string
	for _, t := range model.Tables {
		if n := st.ByTable[t]; n > 0 {
			parts = append(parts, fmt.Sprintf("%s=%d", t, n))
		}
	}
	fmt.Fprintf(w, "    tables          %s\n", strings.Join(parts, " "))
	if st.FailingEntries > 0 {
		fmt.Fprintf(w, "    last failure    %s (%d batches)\n", st.LastError, st.FailingEntries)
	}
	if hasActive && d.ID == active.ID {
		fmt.Fprintln(w, "    This is the active destination: the collector delivers these rows (`vario run`).")
		return nil
	}
	switch {
	case !d.Retired.IsZero():
		fmt.Fprintln(w, "    Not delivered: this database was replaced, and the rows were kept for it.")
	case configuredAt(cfg, d):
		fmt.Fprintln(w, "    Not delivered yet: this destination is configured again but not active (see `vario destination`).")
	default:
		fmt.Fprintln(w, "    Not delivered: this destination is not the configured one.")
	}
	retry := "configure it again first (clickhouse.url, clickhouse.database)"
	if configuredAt(cfg, d) {
		retry = "sent only if it is reachable"
		if d.ServerID != "" {
			retry += " and reports destination id " + d.ServerID
		}
	}
	fmt.Fprintf(w, "      vario destination pending %d --retry     deliver them to this database: %s\n", d.ID, retry)
	if hasActive {
		fmt.Fprintf(w, "      vario destination pending %d --move      deliver them to the active destination (%s) instead\n", d.ID, active)
	}
	fmt.Fprintf(w, "      vario destination pending %d --discard   delete them\n", d.ID)
	return nil
}

// pendingAct retries, moves or discards the rows queued for destination id,
// which is not the active one.
func (a *app) pendingAct(ctx context.Context, id int64, action string, yes bool) error {
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
	again := fmt.Sprintf("vario destination pending %d %s", id, action)
	lctx, release, err := exclusive(ctx, store, "destination pending", again)
	if err != nil {
		return err
	}
	defer release()

	d, ok, err := store.Destination(lctx, id)
	if err != nil {
		return err
	}
	if !ok {
		return withCode(ExitUsage, fmt.Errorf("there is no destination %d (see `vario destination`)", id))
	}
	if d.Pending == 0 {
		// Also the answer to a repeated command: the first run took the rows.
		fmt.Fprintf(w, "Nothing is queued for %s; nothing to do.\n", d)
		return nil
	}
	active, hasActive, err := store.Active(lctx)
	if err != nil {
		return err
	}
	if hasActive && d.ID == active.ID {
		return withCode(ExitUsage, fmt.Errorf("%s is the active destination: the collector delivers its %d queued rows (`vario run`).\n"+
			"To stop delivering to it, change clickhouse.url or clickhouse.database and decide with `vario destination switch`", d, d.Pending))
	}
	switch action {
	case "--retry":
		return a.pendingRetry(lctx, cfg, store, d)
	case "--move":
		return a.pendingMove(lctx, cfg, store, d, active, hasActive, yes)
	}
	return a.pendingDiscard(lctx, store, d, yes)
}

func (a *app) pendingRetry(ctx context.Context, cfg *config.Config, store *spool.Store, d spool.Destination) error {
	w := a.stdout
	if !configuredAt(cfg, d) {
		return withCode(ExitUsage, fmt.Errorf("the %d rows are queued for %s, but the configuration points to %s, database %s.\n"+
			"They are only ever retried at the database they were queued for. Nothing was changed. Either\n"+
			"  - configure that database again (clickhouse.url, clickhouse.database) and repeat this command, or\n"+
			"  - deliver them to the active destination instead (`vario destination pending %d --move`), or delete them (`--discard`)",
			d.Pending, d, cfg.ClickHouse.Endpoint(), cfg.ClickHouse.Database, d.ID))
	}
	client, err := newClient(cfg)
	if err != nil {
		return withCode(ExitUsage, err)
	}
	policy, err := privacy.New(cfg.Privacy)
	if err != nil {
		return withCode(ExitUsage, err)
	}
	c := newCollector(cfg, store, client, nil, policy, a.logger(a.stderr, cfg.Collector.LogLevel))
	fmt.Fprintf(w, "Retrying %d queued rows for %s (destination id %s)...\n", d.Pending, d, orDash(d.ServerID))
	res, err := c.DeliverRetained(ctx, d)
	if res.Delivered > 0 {
		fmt.Fprintf(w, "✓ delivered %d rows to %s\n", res.Delivered, d)
	}
	switch {
	case err == nil:
		fmt.Fprintln(w, "  Nothing is left queued for it. The active destination and all cursors are unchanged.")
		return nil
	case errors.Is(err, spool.ErrIdentityMismatch), errors.Is(err, spool.ErrIdentityMissing):
		return withCode(ExitFailure, fmt.Errorf("%w.\n"+
			"The rows were queued for the database with destination id %s, which is not at %s, database %s now, so nothing was sent and nothing was changed.\n"+
			"They can be retried once that database is back there. Otherwise deliver them to the active destination (`vario destination pending %d --move`) or delete them (`--discard`)",
			err, d.ServerID, d.Endpoint, d.Database, d.ID))
	case errors.Is(err, collector.ErrDestinationCheck):
		// Not identified, so nothing was sent: say how to fix the connection.
		if hint := clickhouse.Hint(clickhouse.Classify(err)); hint != "" {
			err = fmt.Errorf("%w\n%s", err, hint)
		}
	}
	return withCode(ExitIncomplete, fmt.Errorf("retry incomplete, %d rows remain queued for %s: %w\nNothing else was changed; run this command again to continue", res.Remaining, d, err))
}

// consequences prints what an irreversible action does, before it is
// confirmed or applied.
func consequences(w io.Writer, lines ...string) {
	for _, l := range lines {
		fmt.Fprintf(w, "  - %s\n", l)
	}
}

// confirmPending asks before an irreversible action on kept rows. It returns
// false if the user declined; without a terminal, --yes is required.
func (a *app) confirmPending(yes bool, flag, question string) (bool, error) {
	if yes {
		return true, nil
	}
	p := a.migratePrompter(false)
	if !p.interactive {
		return false, withCode(ExitUsage, fmt.Errorf("%s cannot be undone; confirm with --yes. Nothing was changed", flag))
	}
	ok, err := p.confirm(question, false)
	if err != nil {
		return false, err
	}
	if !ok {
		fmt.Fprintln(a.stdout, "Nothing was changed.")
	}
	return ok, nil
}

func resetLine(d spool.Destination) string {
	if !d.Retired.IsZero() {
		return "The replaced database never receives them; its cursors and fingerprints are deleted with them."
	}
	return fmt.Sprintf("%s never receives them, so its cursors and fingerprints are reset: if you configure it again, every source is read again in full for it.", d)
}

func (a *app) pendingMove(ctx context.Context, cfg *config.Config, store *spool.Store, d, active spool.Destination, hasActive, yes bool) error {
	w := a.stdout
	configured, hasConfigured, err := store.Lookup(ctx, cfg.ClickHouse.Endpoint(), cfg.ClickHouse.Database)
	if err != nil {
		return err
	}
	// Rows are only ever moved to the destination the collector delivers to,
	// and only while that is unambiguous.
	switch rep, replaced, _ := store.Replaced(ctx); {
	case !hasActive || active.Legacy():
		return withCode(ExitFailure, errors.New("there is no active destination to move the rows to yet; run `vario run` or `vario backfill` first. Nothing was changed"))
	case !hasConfigured || configured.ID != active.ID:
		return withCode(ExitFailure, fmt.Errorf("the configured destination (%s, database %s) is not the active one (%s).\n"+
			"Rows are moved to the destination the collector delivers to: decide with `vario destination switch` first (see `vario destination`). Nothing was changed",
			cfg.ClickHouse.Endpoint(), cfg.ClickHouse.Database, active))
	case replaced && rep.Destination.ID == active.ID:
		return withCode(ExitFailure, fmt.Errorf("the database of the active destination (%s) was replaced and awaits a decision.\n"+
			"Decide with `vario destination switch` first (see `vario destination`). Nothing was changed", active))
	}
	fmt.Fprintf(w, "Move %d queued rows from %s to the active destination, %s:\n", d.Pending, d, active)
	consequences(w,
		"The collector delivers them to "+active.String()+" at its next start, not to the database they were collected for.",
		"They keep their keys and versions, so records the active destination already has count once in the *_latest and aggregate views.",
		resetLine(d),
		"This cannot be undone.")
	if ok, err := a.confirmPending(yes, "--move", fmt.Sprintf("Move %d rows to %s?", d.Pending, active)); err != nil || !ok {
		return err
	}
	n, err := store.MovePending(ctx, d.ID, active.ID)
	if err != nil {
		return fmt.Errorf("nothing was changed: %w", err)
	}
	fmt.Fprintf(w, "✓ %d queued rows will be delivered to %s\n", n, active)
	fmt.Fprintln(w, "  Start the collector to deliver them (`vario run`, or `vario service install` for the background service).")
	return nil
}

func (a *app) pendingDiscard(ctx context.Context, store *spool.Store, d spool.Destination, yes bool) error {
	w := a.stdout
	fmt.Fprintf(w, "Delete %d queued rows for %s:\n", d.Pending, d)
	consequences(w,
		"They are removed from the local spool and are never delivered anywhere.",
		"Other destinations are synchronized from the sources on their own; whatever the sources no longer hold (pruned events) exists only in these rows and is lost.",
		resetLine(d),
		"This cannot be undone.")
	if ok, err := a.confirmPending(yes, "--discard", fmt.Sprintf("Permanently delete %d queued rows for %s?", d.Pending, d)); err != nil || !ok {
		return err
	}
	n, err := store.DiscardPending(ctx, d.ID)
	if err != nil {
		return fmt.Errorf("nothing was changed: %w", err)
	}
	fmt.Fprintf(w, "✓ deleted %d queued rows for %s\n", n, d)
	return nil
}
