package cli

import (
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/nedanwr/vario/internal/model"
	"github.com/nedanwr/vario/internal/sources/t3code"
	"github.com/nedanwr/vario/internal/spool"
)

func (a *app) statusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Report configuration, collector, synchronization and delivery status",
		Long: `Status reads the local configuration and state. It does not contact
ClickHouse; use 'vario doctor' for connectivity checks.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := a.loadConfig()
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			w := a.stdout
			fmt.Fprintf(w, "Configuration  %s\n", cfg.Path())
			fmt.Fprintf(w, "Mode           %s (redaction %s)\n", cfg.Privacy.Mode, onOff(cfg.Privacy.RedactSecrets))
			fmt.Fprintf(w, "ClickHouse     %s  database=%s user=%s\n", cfg.ClickHouse.URL, cfg.ClickHouse.Database, cfg.ClickHouse.Username)
			fmt.Fprintf(w, "State          %s\n", statePath(cfg))

			if mgr, err := serviceManager(); err == nil {
				st, _ := mgr.Status()
				switch {
				case st.Running:
					fmt.Fprintf(w, "Service        running (pid %d)\n", st.PID)
				case st.Installed:
					fmt.Fprintf(w, "Service        installed, not running (last exit: %s)\n", orDash(st.LastExit))
				default:
					fmt.Fprintln(w, "Service        not installed")
				}
			} else {
				fmt.Fprintln(w, "Service        not available on this platform")
			}

			store, err := spool.OpenReadOnly(statePath(cfg))
			if err != nil {
				fmt.Fprintln(w, "\nNo local state yet: the collector has not run. Start it with `vario run` or `vario backfill --source t3code`.")
				return nil
			}
			defer store.Close()

			if l, active, _ := store.CurrentLease(ctx); active {
				fmt.Fprintf(w, "Collector      active: %s (pid %d)\n", l.Command, l.PID)
			} else {
				fmt.Fprintln(w, "Collector      not running")
			}

			fmt.Fprintln(w, "\nSources")
			if len(cfg.Sources) == 0 {
				fmt.Fprintln(w, "  none configured")
			}
			for _, sc := range cfg.Sources {
				path := sourcePath(sc)
				id := t3code.SourceID(path)
				cur, last := cursorSummary(ctx, store, id)
				fmt.Fprintf(w, "  %s  enabled=%t  %s\n", sc.Kind, sc.Enabled, path)
				fmt.Fprintf(w, "    id             %s\n", id)
				fmt.Fprintf(w, "    cursor         %s\n", cur)
				fmt.Fprintf(w, "    last sync      %s\n", last)
				if v, _, ok, _ := store.KV(ctx, "source."+id+".last_snapshot"); ok {
					fmt.Fprintf(w, "    last snapshot  %s\n", since(v))
				}
				if v, at, ok, _ := store.KV(ctx, "source."+id+".last_error"); ok && v != "" {
					fmt.Fprintf(w, "    last error     %s (%s)\n", v, at.Local().Format(time.DateTime))
				}
			}

			st, err := store.Stats(ctx)
			if err != nil {
				return err
			}
			fmt.Fprintln(w, "\nDelivery")
			fmt.Fprintf(w, "  spooled        %d rows in %d batches (%s of %d MiB)\n", st.Rows, st.Entries, human(st.StoredBytes), cfg.Spool.MaxSizeMB)
			if st.Entries > 0 {
				fmt.Fprintf(w, "  oldest         %s\n", st.Oldest.Local().Format(time.DateTime))
				var parts []string
				for _, t := range model.Tables {
					if n := st.ByTable[t]; n > 0 {
						parts = append(parts, fmt.Sprintf("%s=%d", t, n))
					}
				}
				fmt.Fprintf(w, "  pending        %s\n", strings.Join(parts, " "))
			}
			if st.FailingEntries > 0 {
				fmt.Fprintf(w, "  retrying       %d batches; last error: %s\n", st.FailingEntries, st.LastError)
			}
			if v, at, ok, _ := store.KV(ctx, "delivery.last_success"); ok {
				fmt.Fprintf(w, "  last delivery  %s (%s rows)\n", at.Local().Format(time.DateTime), v)
			} else {
				fmt.Fprintln(w, "  last delivery  never")
			}
			if v, at, ok, _ := store.KV(ctx, "delivery.last_error"); ok && v != "" {
				fmt.Fprintf(w, "  last error     %s (%s)\n", v, at.Local().Format(time.DateTime))
			}
			return nil
		},
	}
}

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func human(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GiB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}
