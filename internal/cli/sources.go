package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/nedanwr/vario/internal/config"
	"github.com/nedanwr/vario/internal/sources"
	"github.com/nedanwr/vario/internal/spool"
)

func (a *app) sourcesCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "sources",
		Short: "Discover and configure data sources",
		Long: `Without a subcommand, sources shows every supported source: whether it is
installed on this computer (read-only discovery) and how it is configured.

Supported sources:
  t3code      T3 Code (~/.t3/userdata/statev2.sqlite)
  claudecode  Claude Code (~/.claude/projects, or $CLAUDE_CONFIG_DIR/projects)`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return a.sourcesOverview(cmd.Context()) },
	}
	cmd.AddCommand(a.sourcesDetectCmd(), a.sourcesListCmd(), a.sourcesAddCmd())
	return cmd
}

// sourcesOverview prints discovery results and configuration for every kind.
func (a *app) sourcesOverview(ctx context.Context) error {
	var cfg *config.Config
	if path, err := a.resolveConfigPath(); err == nil {
		if c, err := config.Load(path); err == nil {
			cfg = c
		}
	}
	for i, k := range sourceKinds {
		if i > 0 {
			fmt.Fprintln(a.stdout)
		}
		fmt.Fprintf(a.stdout, "%s (%s)\n", k.name, k.kind)
		sc, configured := config.SourceConfig{}, false
		if cfg != nil {
			sc, configured = cfg.Source(k.kind)
		}
		switch {
		case cfg == nil:
			fmt.Fprintln(a.stdout, "  configured: no configuration yet (run `vario init`)")
		case !configured:
			fmt.Fprintf(a.stdout, "  configured: no (enable with `vario sources add %s`)\n", k.kind)
		default:
			fmt.Fprintf(a.stdout, "  configured: enabled=%t  %s\n", sc.Enabled, sourcePath(sc))
		}
		found := false
		for _, d := range visibleDetections(k.detect(ctx, a.candidatePaths(k))) {
			found = found || d.Compatible
			mark := "✗"
			if d.Compatible {
				mark = "✓"
			}
			fmt.Fprintf(a.stdout, "  %s %s: %s\n", mark, d.Path, d.Detail)
			for _, f := range d.Facts {
				fmt.Fprintf(a.stdout, "      %s: %s\n", f.Name, f.Value)
			}
		}
		if !found && !configured {
			fmt.Fprintf(a.stdout, "  not detected; if it stores data elsewhere: `vario sources add %s --path <%s>`\n", k.kind, k.pathHelp)
		}
	}
	return nil
}

// candidatePaths returns a kind's default discovery paths plus any configured path.
func (a *app) candidatePaths(k sourceKind) []string {
	paths := k.defaults()
	if path, err := a.resolveConfigPath(); err == nil {
		if cfg, err := config.Load(path); err == nil {
			if sc, ok := cfg.Source(k.kind); ok && sc.Path != "" {
				paths = append([]string{sc.Path}, paths...)
			}
		}
	}
	seen := map[string]bool{}
	var out []string
	for _, p := range paths {
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	return out
}

// visibleDetections omits alternative locations that do not exist when
// another one does.
func visibleDetections(ds []sources.Detection) []sources.Detection {
	exists := false
	for _, d := range ds {
		exists = exists || d.Exists
	}
	if !exists {
		return ds[:min(len(ds), 1)]
	}
	var out []sources.Detection
	for _, d := range ds {
		if d.Exists {
			out = append(out, d)
		}
	}
	return out
}

func printDetection(w io.Writer, d sources.Detection) {
	mark := "✗"
	if d.Compatible {
		mark = "✓"
	}
	fmt.Fprintf(w, "%s %s  %s\n  status: %s\n", mark, d.Kind, d.Path, d.Detail)
	for _, f := range d.Facts {
		fmt.Fprintf(w, "  %s: %s\n", f.Name, f.Value)
	}
}

func (a *app) sourcesDetectCmd() *cobra.Command {
	var paths []string
	return &cobra.Command{
		Use:   "detect",
		Short: "Discover supported local data sources (read-only)",
		Long: `Detect looks for supported AI applications in their default locations and
any configured location. Sources are only read, never modified.

Supported: T3 Code (~/.t3/userdata/statev2.sqlite by default) and Claude Code
(~/.claude/projects, $CLAUDE_CONFIG_DIR/projects or ~/.config/claude/projects).`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			found := false
			for _, k := range sourceKinds {
				ps := paths
				if len(ps) == 0 {
					ps = a.candidatePaths(k)
				}
				for _, d := range visibleDetections(k.detect(cmd.Context(), ps)) {
					printDetection(a.stdout, d)
					found = found || d.Compatible
				}
			}
			if !found {
				fmt.Fprintln(a.stdout, "\nNo compatible sources found. If a source stores data elsewhere, run `vario sources add <t3code|claudecode> --path <path>`.")
			}
			return nil
		},
	}
}

func (a *app) sourcesListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "Show configured sources and their synchronization state",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := a.loadConfig()
			if err != nil {
				return err
			}
			if len(cfg.Sources) == 0 {
				fmt.Fprintln(a.stdout, "No sources configured. Run `vario sources` and `vario sources add <t3code|claudecode>`.")
				return nil
			}
			store, _ := spool.OpenReadOnly(statePath(cfg))
			if store != nil {
				defer store.Close()
				if !selectConfigured(cmd.Context(), store, cfg) {
					store = nil // no state for the configured destination yet
				}
			}
			tw := tabwriter.NewWriter(a.stdout, 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "KIND\tENABLED\tPATH\tCURSOR\tLAST SYNC")
			for _, sc := range cfg.Sources {
				path := sourcePath(sc)
				cur, last := "-", "-"
				if store != nil {
					cur, last = cursorSummary(cmd.Context(), store, sc.Kind, sourceID(sc))
				}
				fmt.Fprintf(tw, "%s\t%t\t%s\t%s\t%s\n", sc.Kind, sc.Enabled, path, cur, last)
			}
			return tw.Flush()
		},
	}
}

func cursorSummary(ctx context.Context, store *spool.Store, kind, id string) (string, string) {
	cur, last := "-", "-"
	if c, ok, _ := store.Cursor(ctx, id); ok {
		if k, ok := lookupKind(kind); ok {
			cur = k.summary(c)
		}
	}
	if v, _, ok, _ := store.KV(ctx, "source."+id+".last_sync"); ok && v != "" {
		last = since(v)
	}
	return cur, last
}

func since(rfc3339 string) string {
	t, err := time.Parse(time.RFC3339, rfc3339)
	if err != nil {
		return rfc3339
	}
	return fmt.Sprintf("%s (%s ago)", t.Local().Format("2006-01-02 15:04:05"), time.Since(t).Round(time.Second))
}

func (a *app) sourcesAddCmd() *cobra.Command {
	var path string
	var disabled, force bool
	cmd := &cobra.Command{
		Use:   "add <t3code|claudecode>",
		Short: "Configure and enable a source",
		Long: `Add validates that the source exists and can be read, then enables it in the
configuration. Use --path when it is not in its default location:

  t3code      the path of T3 Code's statev2.sqlite
  claudecode  Claude Code's configuration directory (~/.claude, or
              $CLAUDE_CONFIG_DIR) or its projects directory

Enabling a source does not affect other sources or data already collected.
The running collector picks it up after a restart; the first synchronization
reads its history in full.`,
		Args:      cobra.ExactArgs(1),
		ValidArgs: config.SourceKinds,
		RunE: func(cmd *cobra.Command, args []string) error {
			k, ok := lookupKind(args[0])
			if !ok {
				return withCode(ExitUsage, fmt.Errorf("unsupported source %q (supported: %s)", args[0], supportedList()))
			}
			cfgPath, err := a.resolveConfigPath()
			if err != nil {
				return withCode(ExitUsage, err)
			}
			cfg, err := config.Load(cfgPath)
			if errors.Is(err, config.ErrNotFound) {
				return withCode(ExitUsage, fmt.Errorf("no configuration at %s; run `vario init` first", cfgPath))
			}
			if err != nil {
				return withCode(ExitUsage, err)
			}
			if path != "" {
				if path, err = config.ExpandPath(path); err != nil {
					return withCode(ExitUsage, err)
				}
			}
			check := path
			if check == "" {
				check = sourcePath(config.SourceConfig{Kind: k.kind})
			}
			d := k.detect(cmd.Context(), []string{check})[0]
			printDetection(a.stdout, d)
			if !d.Compatible && !force {
				return fmt.Errorf("source not usable at %s (use --force to add it anyway)", check)
			}
			if d.Compatible {
				check = d.Path
			}
			next := cfg.Clone()
			next.UpsertSource(config.SourceConfig{Kind: k.kind, Enabled: !disabled, Path: check})
			if _, err := config.Update(cfgPath, cfg.Fingerprint(), config.Diff(cfg, next)); err != nil {
				return err
			}
			state := "enabled"
			if disabled {
				state = "added (disabled)"
			}
			fmt.Fprintf(a.stdout, "\nSource %s %s in %s\n", k.kind, state, cfgPath)
			fmt.Fprintf(a.stdout, "Next: `vario backfill --source %s` or `vario run`.\n", k.kind)
			a.serviceNotice(cfgPath, true)
			return nil
		},
	}
	cmd.Flags().StringVar(&path, "path", "", "source location (t3code: statev2.sqlite; claudecode: ~/.claude or its projects directory)")
	cmd.Flags().BoolVar(&disabled, "disabled", false, "add the source without enabling it")
	cmd.Flags().BoolVar(&force, "force", false, "add even if the source is missing or unreadable")
	return cmd
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
