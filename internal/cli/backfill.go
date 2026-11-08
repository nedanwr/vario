package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/nedanwr/vario/internal/collector"
	"github.com/nedanwr/vario/internal/config"
	"github.com/nedanwr/vario/internal/privacy"
	"github.com/nedanwr/vario/internal/spool"
)

func (a *app) backfillCmd() *cobra.Command {
	var source string
	var force, noDeliver bool
	cmd := &cobra.Command{
		Use:   "backfill --source t3code",
		Short: "Import existing historical data from a source",
		Long: `Backfill reads a consistent snapshot of the source's current state and its
event history into the local spool, records the snapshot boundary as the
incremental cursor, and then delivers the spool to ClickHouse.

Backfill is idempotent. Records already queued with identical content are
skipped; --force re-sends everything (for example after recreating the
ClickHouse database). If ClickHouse is unreachable, data stays spooled and is
delivered later by 'vario run' (exit code 3).`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if source != config.SourceT3Code {
				return withCode(ExitUsage, fmt.Errorf("unsupported source %q (supported: %s)", source, config.SourceT3Code))
			}
			cfg, err := a.loadConfig()
			if err != nil {
				return err
			}
			if _, ok := cfg.Source(source); !ok {
				return withCode(ExitUsage, fmt.Errorf("source %q is not configured; run `vario sources add %s`", source, source))
			}
			policy, err := privacy.New(cfg.Privacy)
			if err != nil {
				return withCode(ExitUsage, err)
			}
			srcs, _ := buildSources(cfg, policy, source)
			src := srcs[0]
			defer src.Close()

			store, err := openStore(cfg)
			if err != nil {
				return err
			}
			defer store.Close()
			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			release, err := collector.Lease(ctx, store, "backfill", 0)
			if errors.Is(err, spool.ErrLeaseHeld) {
				return fmt.Errorf("%w; the running collector backfills new sources automatically (stop it to run a manual backfill)", err)
			}
			if err != nil {
				return err
			}
			defer release()

			client, err := newClient(cfg)
			if err != nil {
				return withCode(ExitUsage, err)
			}
			log := a.logger(a.stderr, cfg.Collector.LogLevel)
			c := newCollector(cfg, store, client, srcs, policy, log)

			fmt.Fprintf(a.stdout, "Backfilling %s from %s (mode: %s)\n", src.ID(), src.Location(), policy.Mode())
			res, err := c.Snapshot(ctx, src, force, force)
			if err != nil {
				return fmt.Errorf("backfill: %w", err)
			}
			fmt.Fprintf(a.stdout, "Queued %d records (%d unchanged) in %s\n", res.Enqueued, res.Skipped, res.Duration.Round(time.Millisecond))
			if noDeliver {
				fmt.Fprintln(a.stdout, "Delivery skipped (--no-deliver); `vario run` will deliver the spool.")
				return nil
			}
			fmt.Fprintf(a.stdout, "Delivering to %s (database %s)...\n", cfg.ClickHouse.URL, cfg.ClickHouse.Database)
			dr, err := c.Drain(ctx)
			fmt.Fprintf(a.stdout, "Delivered %d rows\n", dr.Delivered)
			if err != nil {
				return withCode(ExitIncomplete, fmt.Errorf("delivery incomplete, %d rows remain spooled: %w", dr.Remaining, err))
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&source, "source", "", "source to backfill (t3code)")
	cmd.Flags().BoolVar(&force, "force", false, "re-send all records and the full event history even if unchanged")
	cmd.Flags().BoolVar(&noDeliver, "no-deliver", false, "only fill the local spool; do not contact ClickHouse")
	_ = cmd.MarkFlagRequired("source")
	return cmd
}
