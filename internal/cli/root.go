// Package cli implements the vario command-line interface.
package cli

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/nedanwr/vario/internal/collector"
	"github.com/nedanwr/vario/internal/config"
	"github.com/nedanwr/vario/internal/privacy"
	"github.com/nedanwr/vario/internal/service"
	"github.com/nedanwr/vario/internal/sources"
	"github.com/nedanwr/vario/internal/sources/t3code"
	"github.com/nedanwr/vario/internal/spool"
	"github.com/nedanwr/vario/internal/storage/clickhouse"
)

// Exit codes.
const (
	ExitOK         = 0
	ExitFailure    = 1
	ExitUsage      = 2
	ExitIncomplete = 3
)

type exitError struct {
	code int
	err  error
}

func (e *exitError) Error() string { return e.err.Error() }
func (e *exitError) Unwrap() error { return e.err }

func withCode(code int, err error) error {
	if err == nil {
		return nil
	}
	return &exitError{code: code, err: err}
}

type app struct {
	configPath string
	verbose    bool
	stdin      io.Reader
	stdout     io.Writer
	stderr     io.Writer
}

// Execute runs the CLI and returns the process exit code.
func Execute() int {
	return run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr)
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	a := &app{stdin: stdin, stdout: stdout, stderr: stderr}
	root := a.rootCmd()
	root.SetArgs(args)
	root.SetIn(stdin)
	root.SetOut(stdout)
	root.SetErr(stderr)
	err := root.Execute()
	if err == nil {
		return ExitOK
	}
	fmt.Fprintln(stderr, "vario: "+err.Error())
	var ee *exitError
	if errors.As(err, &ee) {
		return ee.code
	}
	if isUsageError(err) {
		return ExitUsage
	}
	return ExitFailure
}

func isUsageError(err error) bool {
	msg := err.Error()
	for _, p := range []string{"unknown command", "unknown flag", "unknown shorthand", "accepts ", "requires ", "invalid argument", "flag needs an argument", "required flag"} {
		if strings.Contains(msg, p) {
			return true
		}
	}
	return false
}

func (a *app) rootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "vario",
		Short: "Observe everything AI",
		Long: `Vario collects AI interactions from locally installed AI applications -
conversations, tool executions and token usage - and archives them in
ClickHouse for analysis. Data goes only to the destination you configure.`,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.PersistentFlags().StringVar(&a.configPath, "config", "", "configuration file (default: $VARIO_CONFIG, else $VARIO_HOME/config.toml, else ~/.vario/config.toml)")
	root.PersistentFlags().BoolVarP(&a.verbose, "verbose", "v", false, "enable debug logging")
	root.AddCommand(a.initCmd(), a.sourcesCmd(), a.backfillCmd(), a.runCmd(), a.statusCmd(), a.doctorCmd(), a.serviceCmd(), a.configCmd(), a.versionCmd())
	return root
}

// resolveLocation applies the configuration precedence shared by every
// command, the collector and the background service (see config.Resolve).
func (a *app) resolveLocation() (config.Location, error) {
	return config.Resolve(a.configPath)
}

func (a *app) resolveConfigPath() (string, error) {
	loc, err := a.resolveLocation()
	return loc.Path, err
}

// serviceManager returns the platform service manager; tests replace it.
var serviceManager = service.Default

// serviceNotice tells the user how configuration changes reach the
// background service. It never restarts or reinstalls the service. It returns
// true if a service is installed.
func (a *app) serviceNotice(cfgPath string, changed bool) bool {
	mgr, err := serviceManager()
	if err != nil {
		return false
	}
	st, err := mgr.Status()
	if err != nil || !st.Installed {
		return false
	}
	if args, err := mgr.Args(); err == nil {
		if p := service.ConfigPath(args); p != "" && p != cfgPath {
			fmt.Fprintf(a.stdout, "\n! The background service reads %s, not %s.\n"+
				"  Run `vario service install` once to point it at this configuration.\n",
				config.DisplayPath(p), config.DisplayPath(cfgPath))
			return true
		}
	}
	if changed && st.Running {
		fmt.Fprint(a.stdout, "\nThe background service is running with the previous settings. Apply the changes with:\n"+
			"  vario service restart\n")
	}
	return true
}

// loadConfig loads and validates the configuration file.
func (a *app) loadConfig() (*config.Config, error) {
	path, err := a.resolveConfigPath()
	if err != nil {
		return nil, withCode(ExitUsage, err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		return nil, withCode(ExitUsage, err)
	}
	if err := cfg.Validate(); err != nil {
		return nil, withCode(ExitUsage, fmt.Errorf("invalid configuration %s:\n  %s", path, strings.ReplaceAll(err.Error(), "\n", "\n  ")))
	}
	return cfg, nil
}

func statePath(cfg *config.Config) string {
	return filepath.Join(cfg.Collector.DataDir, config.StateFileName)
}

func openStore(cfg *config.Config) (*spool.Store, error) {
	return spool.Open(statePath(cfg), int64(cfg.Spool.MaxSizeMB)<<20)
}

func newClient(cfg *config.Config) (*clickhouse.Client, error) {
	pw, err := cfg.Password()
	if err != nil {
		return nil, err
	}
	return clickhouse.New(cfg.ClickHouse, pw)
}

// sourcePath returns the configured database path for T3 Code, or the first default.
func sourcePath(sc config.SourceConfig) string {
	if sc.Path != "" {
		return sc.Path
	}
	if d := t3code.DefaultPaths(); len(d) > 0 {
		return d[0]
	}
	return ""
}

func buildSources(cfg *config.Config, policy *privacy.Policy, only string) ([]sources.Source, error) {
	var out []sources.Source
	for _, sc := range cfg.Sources {
		if only != "" && sc.Kind != only {
			continue
		}
		if only == "" && !sc.Enabled {
			continue
		}
		switch sc.Kind {
		case config.SourceT3Code:
			out = append(out, t3code.New(sourcePath(sc), policy, cfg.Collector.BatchSize))
		}
	}
	return out, nil
}

func newCollector(cfg *config.Config, store *spool.Store, sink collector.Sink, srcs []sources.Source, policy *privacy.Policy, log *slog.Logger) *collector.Collector {
	return collector.New(store, sink, srcs, collector.Options{
		PollInterval:      cfg.Collector.PollInterval.Duration,
		ReconcileInterval: cfg.Collector.ReconcileInterval.Duration,
		BatchSize:         cfg.Collector.BatchSize,
		FlushInterval:     cfg.ClickHouse.FlushInterval.Duration,
		MaxInsertRows:     cfg.ClickHouse.MaxInsertRows,
		RetryInitial:      cfg.ClickHouse.RetryInitialBackoff.Duration,
		RetryMax:          cfg.ClickHouse.RetryMaxBackoff.Duration,
		PolicyFingerprint: policy.Fingerprint(),
	}, log)
}

func (a *app) logger(w io.Writer, level string) *slog.Logger {
	lv := slog.LevelInfo
	switch strings.ToLower(level) {
	case "debug":
		lv = slog.LevelDebug
	case "warn":
		lv = slog.LevelWarn
	case "error":
		lv = slog.LevelError
	}
	if a.verbose {
		lv = slog.LevelDebug
	}
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: lv}))
}
