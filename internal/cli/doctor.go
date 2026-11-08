package cli

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/nedanwr/vario/internal/config"
	"github.com/nedanwr/vario/internal/model"
	"github.com/nedanwr/vario/internal/privacy"
	"github.com/nedanwr/vario/internal/sources/t3code"
	"github.com/nedanwr/vario/internal/spool"
)

type checker struct {
	w        *strings.Builder
	failures int
	warnings int
}

func (c *checker) ok(format string, a ...any) { fmt.Fprintf(c.w, "  ✓ "+format+"\n", a...) }
func (c *checker) warn(format string, a ...any) {
	c.warnings++
	fmt.Fprintf(c.w, "  ! "+format+"\n", a...)
}
func (c *checker) fail(format string, a ...any) {
	c.failures++
	fmt.Fprintf(c.w, "  ✗ "+format+"\n", a...)
}

func (a *app) doctorCmd() *cobra.Command {
	var fixSchema bool
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Validate configuration, source access and ClickHouse connectivity",
		Long: `Doctor checks the configuration file and its permissions, every configured
source (read-only), the local spool, and the ClickHouse connection,
authentication and schema. It exits with status 1 if any check fails.

With --create-schema, missing ClickHouse tables and views are created.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c := &checker{w: &strings.Builder{}}
			defer func() { fmt.Fprint(a.stdout, c.w.String()) }()
			ctx := cmd.Context()

			fmt.Fprintln(c.w, "Configuration")
			path, err := a.resolveConfigPath()
			if err != nil {
				c.fail("cannot resolve configuration path: %v", err)
				return withCode(ExitFailure, errors.New("doctor found problems"))
			}
			cfg, err := config.Load(path)
			if err != nil {
				c.fail("%v", err)
				return withCode(ExitFailure, errors.New("doctor found problems"))
			}
			c.ok("loaded %s", path)
			if err := cfg.Validate(); err != nil {
				for _, line := range strings.Split(err.Error(), "\n") {
					c.fail("%s", line)
				}
				return withCode(ExitFailure, errors.New("doctor found problems"))
			}
			c.ok("configuration is valid")
			checkPrivate(c, path, "configuration file")
			c.ok("collection mode: %s (secret redaction %s)", cfg.Privacy.Mode, onOff(cfg.Privacy.RedactSecrets))
			if !cfg.Privacy.RedactSecrets && cfg.Privacy.Mode != config.ModeMetrics {
				c.warn("secret redaction is disabled while collecting conversation content")
			}
			if _, err := privacy.New(cfg.Privacy); err != nil {
				c.fail("privacy settings: %v", err)
			}

			fmt.Fprintln(c.w, "\nSources")
			enabled := 0
			for _, sc := range cfg.Sources {
				if !sc.Enabled {
					c.warn("%s is configured but disabled", sc.Kind)
					continue
				}
				enabled++
				d := t3code.Detect(ctx, []string{sourcePath(sc)})[0]
				if d.Compatible {
					c.ok("%s: %s (%s, read-only access verified)", sc.Kind, d.Path, d.Detail)
				} else {
					c.fail("%s: %s: %s", sc.Kind, d.Path, d.Detail)
				}
			}
			if enabled == 0 {
				c.fail("no enabled sources; run `vario sources add t3code`")
			}

			fmt.Fprintln(c.w, "\nLocal state")
			if err := os.MkdirAll(cfg.Collector.DataDir, 0o700); err != nil {
				c.fail("data directory %s is not writable: %v", cfg.Collector.DataDir, err)
			} else if store, err := spool.OpenReadOnly(statePath(cfg)); err == nil {
				st, err := store.Stats(ctx)
				store.Close()
				if err != nil {
					c.fail("cannot read spool: %v", err)
				} else {
					c.ok("spool: %d rows pending (%s of %d MiB)", st.Rows, human(st.StoredBytes), cfg.Spool.MaxSizeMB)
					if st.FailingEntries > 0 {
						c.warn("%d batches are being retried; last error: %s", st.FailingEntries, st.LastError)
					}
					if float64(st.StoredBytes) > 0.8*float64(int64(cfg.Spool.MaxSizeMB)<<20) {
						c.warn("spool is over 80%% full; collection pauses when it is full")
					}
				}
			} else {
				c.ok("data directory %s (no state yet)", cfg.Collector.DataDir)
			}

			fmt.Fprintln(c.w, "\nClickHouse")
			a.checkClickHouse(ctx, c, cfg, fixSchema)

			if c.failures > 0 {
				fmt.Fprintf(c.w, "\n%d check(s) failed, %d warning(s)\n", c.failures, c.warnings)
				return withCode(ExitFailure, errors.New("doctor found problems"))
			}
			fmt.Fprintf(c.w, "\nAll checks passed (%d warning(s))\n", c.warnings)
			return nil
		},
	}
	cmd.Flags().BoolVar(&fixSchema, "create-schema", false, "create missing ClickHouse tables and views")
	return cmd
}

func checkPrivate(c *checker, path, what string) {
	st, err := os.Stat(path)
	if err != nil {
		return
	}
	if runtime.GOOS != "windows" && st.Mode().Perm()&0o077 != 0 {
		c.warn("%s %s is accessible by other users (mode %o); run: chmod 600 %q", what, path, st.Mode().Perm(), path)
		return
	}
	c.ok("%s permissions are restricted", what)
}

func (a *app) checkClickHouse(ctx context.Context, c *checker, cfg *config.Config, fixSchema bool) {
	u, _ := url.Parse(cfg.ClickHouse.URL)
	switch {
	case u.Scheme == "https":
		c.ok("TLS enabled (%s)", u.Host)
		if cfg.ClickHouse.TLSInsecureSkipVerify {
			c.warn("TLS certificate verification is disabled")
		}
	case config.IsLoopback(u.Hostname()):
		c.ok("local endpoint %s (unencrypted HTTP on loopback)", u.Host)
	default:
		c.warn("unencrypted HTTP to remote host %s", u.Host)
	}
	if _, ok := os.LookupEnv(config.EnvPassword); ok {
		c.ok("password supplied via $%s", config.EnvPassword)
	} else if fileExists(cfg.PasswordPath()) {
		checkPrivate(c, cfg.PasswordPath(), "password file")
	} else {
		c.warn("no password configured (%s or $%s)", cfg.PasswordPath(), config.EnvPassword)
	}
	client, err := newClient(cfg)
	if err != nil {
		c.fail("client setup: %v", err)
		return
	}
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := client.Ping(cctx); err != nil {
		c.fail("endpoint unreachable: %v", err)
		return
	}
	c.ok("endpoint reachable")
	ver, err := client.ServerVersion(cctx)
	if err != nil {
		c.fail("authentication or query failed: %v", err)
		return
	}
	c.ok("authenticated as %s (ClickHouse %s)", cfg.ClickHouse.Username, ver)
	if fixSchema {
		if err := client.EnsureSchema(cctx); err != nil {
			c.fail("schema creation failed: %v", err)
			return
		}
		c.ok("schema created or already up to date")
	}
	missing, err := client.MissingTables(cctx)
	switch {
	case err != nil:
		c.fail("cannot inspect schema: %v", err)
	case len(missing) == len(model.Tables):
		c.warn("database %s has no Vario tables yet; they are created on first delivery (or run `vario doctor --create-schema`)", cfg.ClickHouse.Database)
	case len(missing) > 0:
		c.warn("missing tables: %s (created automatically on next delivery)", strings.Join(missing, ", "))
	default:
		c.ok("all Vario tables present in database %s", cfg.ClickHouse.Database)
	}
}
