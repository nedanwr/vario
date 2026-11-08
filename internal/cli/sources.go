package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/nedanwr/vario/internal/config"
	"github.com/nedanwr/vario/internal/sources"
	"github.com/nedanwr/vario/internal/sources/t3code"
	"github.com/nedanwr/vario/internal/spool"
)

func (a *app) sourcesCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "sources",
		Short: "Discover and configure data sources",
	}
	cmd.AddCommand(a.sourcesDetectCmd(), a.sourcesListCmd(), a.sourcesAddCmd())
	return cmd
}

// candidatePaths returns default discovery paths plus any configured path.
func (a *app) candidatePaths() []string {
	paths := t3code.DefaultPaths()
	if path, err := a.resolveConfigPath(); err == nil {
		if cfg, err := config.Load(path); err == nil {
			if sc, ok := cfg.Source(config.SourceT3Code); ok && sc.Path != "" {
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
any configured location. Databases are opened read-only and never modified.

Supported: T3 Code (~/.t3/userdata/statev2.sqlite by default).`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if len(paths) == 0 {
				paths = a.candidatePaths()
			}
			found := false
			for _, d := range t3code.Detect(cmd.Context(), paths) {
				printDetection(a.stdout, d)
				found = found || d.Compatible
			}
			if !found {
				fmt.Fprintln(a.stdout, "\nNo compatible sources found. If T3 Code stores data elsewhere, run `vario sources add t3code --path <statev2.sqlite>`.")
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
				fmt.Fprintln(a.stdout, "No sources configured. Run `vario sources detect` and `vario sources add t3code`.")
				return nil
			}
			store, _ := spool.OpenReadOnly(statePath(cfg))
			if store != nil {
				defer store.Close()
			}
			tw := tabwriter.NewWriter(a.stdout, 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "KIND\tENABLED\tPATH\tCURSOR\tLAST SYNC")
			for _, sc := range cfg.Sources {
				path := sourcePath(sc)
				cur, last := "-", "-"
				if store != nil {
					cur, last = cursorSummary(cmd.Context(), store, t3code.SourceID(path))
				}
				fmt.Fprintf(tw, "%s\t%t\t%s\t%s\t%s\n", sc.Kind, sc.Enabled, path, cur, last)
			}
			return tw.Flush()
		},
	}
}

func cursorSummary(ctx context.Context, store *spool.Store, id string) (string, string) {
	cur, last := "-", "-"
	if c, ok, _ := store.Cursor(ctx, id); ok {
		var v struct {
			Epoch int64 `json:"epoch"`
			Seq   int64 `json:"seq"`
		}
		if json.Unmarshal([]byte(c), &v) == nil {
			cur = fmt.Sprintf("seq %d", v.Seq)
			if v.Epoch > 1 {
				cur += fmt.Sprintf(" (epoch %d)", v.Epoch)
			}
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
		Use:   "add t3code",
		Short: "Configure and enable a source",
		Long: `Add validates that the source database exists and has a compatible schema,
then enables it in the configuration. Use --path when the database is not in
its default location.`,
		Args:      cobra.ExactArgs(1),
		ValidArgs: []string{config.SourceT3Code},
		RunE: func(cmd *cobra.Command, args []string) error {
			if args[0] != config.SourceT3Code {
				return withCode(ExitUsage, fmt.Errorf("unsupported source %q (supported: %s)", args[0], config.SourceT3Code))
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
				check = sourcePath(config.SourceConfig{})
			}
			d := t3code.Detect(cmd.Context(), []string{check})[0]
			printDetection(a.stdout, d)
			if !d.Compatible && !force {
				return fmt.Errorf("source not usable at %s (use --force to add it anyway)", check)
			}
			cfg.UpsertSource(config.SourceConfig{Kind: config.SourceT3Code, Enabled: !disabled, Path: check})
			if err := cfg.Save(); err != nil {
				return err
			}
			state := "enabled"
			if disabled {
				state = "added (disabled)"
			}
			fmt.Fprintf(a.stdout, "\nSource t3code %s in %s\n", state, cfgPath)
			fmt.Fprintln(a.stdout, "Next: `vario backfill --source t3code` or `vario run`.")
			return nil
		},
	}
	cmd.Flags().StringVar(&path, "path", "", "path to T3 Code's statev2.sqlite")
	cmd.Flags().BoolVar(&disabled, "disabled", false, "add the source without enabling it")
	cmd.Flags().BoolVar(&force, "force", false, "add even if the database is missing or incompatible")
	return cmd
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
