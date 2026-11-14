package cli

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/nedanwr/vario/internal/collector"
	"github.com/nedanwr/vario/internal/config"
	"github.com/nedanwr/vario/internal/privacy"
	"github.com/nedanwr/vario/internal/service"
	"github.com/nedanwr/vario/internal/sources/t3code"
	"github.com/nedanwr/vario/internal/spool"
	"github.com/nedanwr/vario/internal/storage/clickhouse"
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
func (c *checker) info(format string, a ...any) { fmt.Fprintf(c.w, "  · "+format+"\n", a...) }
func (c *checker) hint(s string)                { fmt.Fprintf(c.w, "    %s\n", s) }
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

Doctor only reads: it checks the operational login (authentication, database
access, read-only settings, privileges via CHECK GRANT and an insert probe
that writes no rows), the schema version, which destination queued data
belongs to, and whether the database still holds the delivery checkpoint the
collector recorded (a lower one means it was restored to an earlier state).
It never changes ClickHouse.

--create-schema is deprecated: it runs 'vario migrate', which asks for
administrative credentials and confirmation.`,
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
			for _, l := range legacyWarnings(cfg) {
				c.warn("%s", l)
			}
			checkPrivate(c, path, "configuration file")
			if dir := filepath.Dir(path); filepath.Base(dir) == ".vario" {
				checkPrivate(c, dir, "configuration directory")
			}
			a.checkServiceConfig(c, cfg)
			if st, err := loadPendingStore(pendingPath(path)); err != nil {
				c.warn("%v", err)
			} else {
				for _, e := range st.Users {
					c.warn("an unfinished `vario init` created ClickHouse %s; its password is kept in %s until a configuration uses it (run `vario init`)",
						e, config.DisplayPath(pendingPath(path)))
				}
			}
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
			replaced, rolledBack := false, false
			if err := os.MkdirAll(cfg.Collector.DataDir, 0o700); err != nil {
				c.fail("data directory %s is not writable: %v", cfg.Collector.DataDir, err)
			} else if store, err := spool.OpenReadOnly(statePath(cfg)); err == nil {
				replaced, rolledBack = checkDestinations(ctx, c, cfg, store)
				st, err := store.Stats(ctx)
				store.Close()
				if err != nil {
					c.fail("cannot read spool: %v", err)
				} else {
					c.ok("spool: %d rows pending (%s of %d MiB)", st.Rows+st.OtherRows, human(st.TotalBytes), cfg.Spool.MaxSizeMB)
					if st.FailingEntries > 0 {
						c.warn("%d batches are being retried; last error: %s", st.FailingEntries, st.LastError)
					}
					if float64(st.TotalBytes) > 0.8*float64(int64(cfg.Spool.MaxSizeMB)<<20) {
						c.warn("spool is over 80%% full; collection pauses when it is full")
					}
				}
			} else {
				c.ok("data directory %s (no state yet)", cfg.Collector.DataDir)
			}

			fmt.Fprintln(c.w, "\nClickHouse")
			if fixSchema {
				fmt.Fprintln(a.stderr, "Note: `vario doctor --create-schema` is deprecated; running `vario migrate`.")
				fmt.Fprint(a.stdout, c.w.String())
				c.w.Reset()
				if err := a.runMigrate(ctx, migrateOptions{}); err != nil {
					c.fail("migration: %v", err)
				}
				fmt.Fprintln(c.w)
			}
			a.checkClickHouse(ctx, c, cfg, replaced, rolledBack)

			if c.failures > 0 {
				fmt.Fprintf(c.w, "\n%d check(s) failed, %d warning(s)\n", c.failures, c.warnings)
				return withCode(ExitFailure, errors.New("doctor found problems"))
			}
			fmt.Fprintf(c.w, "\nAll checks passed (%d warning(s))\n", c.warnings)
			return nil
		},
	}
	cmd.Flags().BoolVar(&fixSchema, "create-schema", false, "deprecated: run `vario migrate` instead")
	_ = cmd.Flags().MarkDeprecated("create-schema", "use `vario migrate` (this flag runs it)")
	return cmd
}

func checkPrivate(c *checker, path, what string) {
	st, err := os.Stat(path)
	if err != nil {
		return
	}
	if runtime.GOOS != "windows" && st.Mode().Perm()&0o077 != 0 {
		mode := "600"
		if st.IsDir() {
			mode = "700"
		}
		c.warn("%s %s is accessible by other users (mode %o); run: chmod %s %q", what, path, st.Mode().Perm(), mode, path)
		return
	}
	c.ok("%s permissions are restricted", what)
}

// checkServiceConfig warns when the installed background service reads a
// different configuration file than this command.
func (a *app) checkServiceConfig(c *checker, cfg *config.Config) {
	path := cfg.Path()
	mgr, err := serviceManager()
	if err != nil {
		return
	}
	args, err := mgr.Args()
	if err != nil {
		return
	}
	if p := service.ConfigPath(args); p != "" && p != path {
		c.warn("the background service reads %s, not this configuration; run `vario service install` to switch it", p)
		return
	}
	if d := serviceDataDir(cfg); d != cfg.Collector.DataDir {
		c.fail("collector.data_dir is empty: the background service (without $%s) uses %s, this shell %s; set it with `vario config set collector.data_dir %s`",
			config.EnvHome, d, cfg.Collector.DataDir, cfg.Collector.DataDir)
	}
}

// checkDestinations reports which destination queued data belongs to. It
// returns whether it reported a detected database replacement or rollback.
func checkDestinations(ctx context.Context, c *checker, cfg *config.Config, store *spool.Store) (replaced, rolledBack bool) {
	if store.Legacy() {
		c.info("local state from an earlier Vario version; the next `vario run` assigns it to the configured destination")
		return false, false
	}
	active, hasActive, err := store.Active(ctx)
	if err != nil {
		c.fail("cannot read destinations: %v", err)
		return false, false
	}
	configured, hasConfigured, _ := store.Lookup(ctx, cfg.ClickHouse.Endpoint(), cfg.ClickHouse.Database)
	rep, isReplaced, _ := store.Replaced(ctx)
	rb, isRolledBack, _ := store.RolledBack(ctx)
	switch {
	case isRolledBack && hasConfigured && rb.Destination.ID == configured.ID && !isReplaced:
		rolledBack = true
		c.fail("the database of %s was restored to an earlier state (detected %s): it acknowledged delivery checkpoint %d, but held %s", configured, rb.At.Local().Format(time.DateTime), rb.Confirmed, heldCheckpoint(rb.Found))
		c.hint(fmt.Sprintf("It carries the recorded destination id %s, so rows delivered after that point are missing.", orDash(configured.ServerID)))
		c.hint(fmt.Sprintf("Delivery is suspended and the collector does not start; %d queued rows are kept in the local spool.", configured.Pending))
		for _, line := range strings.Split(collector.RollbackRecovery, "\n") {
			c.hint(line)
		}
	case isReplaced && hasConfigured && rep.Destination.ID == configured.ID:
		replaced = true
		c.fail("the database of %s was replaced (detected %s): it now carries destination id %s, Vario delivered to %s", configured, rep.At.Local().Format(time.DateTime), rep.Found, configured.ServerID)
		c.hint(fmt.Sprintf("Delivery is suspended and the collector does not start; %d queued rows are kept in the local spool.", configured.Pending))
		for _, line := range strings.Split(collector.ReplacedRecovery, "\n") {
			c.hint(line)
		}
	case !hasActive:
	case hasConfigured && configured.ID == active.ID:
		c.ok("destination: %s", active)
	case active.Pending > 0:
		c.fail("the configured destination differs from %s, which still has %d queued rows; the collector will not start", active, active.Pending)
		c.hint("Choose with `vario destination switch` (see `vario destination`).")
	default:
		c.info("the configured destination is new; the collector switches to it and reads every source again in full")
	}
	dests, _ := store.Destinations(ctx)
	for _, d := range dests {
		if d.Pending > 0 && (!hasActive || d.ID != active.ID) && (!hasConfigured || d.ID != configured.ID) {
			c.warn("%d rows are queued for %s, which is not in use; they are not delivered (see `vario destination pending`)", d.Pending, d)
		}
	}
	return replaced, rolledBack
}

// checkCheckpoint compares the delivery checkpoint the database holds now
// with the one recorded locally as acknowledged, for the configured
// destination. Unlike the local state check, it also finds a rollback no
// collector has seen yet. It only reads. reported says that the local state
// check already gave the recovery steps.
func checkCheckpoint(ctx context.Context, c *checker, cfg *config.Config, client *clickhouse.Client, st clickhouse.SchemaStatus, reported bool) {
	if !st.Usable() {
		return
	}
	var cp spool.Checkpoint
	recorded := ""
	if store, err := spool.OpenReadOnly(statePath(cfg)); err == nil {
		if d, ok, _ := store.Lookup(ctx, cfg.ClickHouse.Endpoint(), cfg.ClickHouse.Database); ok && !store.Legacy() {
			recorded = d.ServerID
			cp, _ = store.Checkpoint(ctx, d.ID)
		}
		store.Close()
	}
	if recorded != "" && st.DestinationID != recorded {
		return // another or an unverifiable database: reported by the identity check
	}
	recovery := func() {
		if reported {
			c.hint("See the recovery steps under \"Local state\" above.")
			return
		}
		c.hint("Delivery is refused and queued rows are kept until you decide.")
		for _, line := range strings.Split(collector.RollbackRecovery, "\n") {
			c.hint(line)
		}
	}
	switch {
	case !st.Checkpoints && cp.Confirmed > 0:
		c.fail("the database has no delivery checkpoint table (%s) that this user can read, but it acknowledged checkpoint %d: it was restored to a state from before the table existed, or the user lost access to it", clickhouse.CheckpointTable, cp.Confirmed)
		recovery()
	case !st.Checkpoints:
		c.warn("the database has no delivery checkpoint table (%s) that this user can use: a database restored to an earlier state (for example from a backup) cannot be detected; run `vario migrate`", clickhouse.CheckpointTable)
	case cp.Confirmed == 0:
		c.info("delivery checkpoints are available; the collector writes its first one when it next starts, and detects a database restored to an earlier state from then on")
	default:
		found, _, err := client.ReadCheckpoint(ctx, cp.Writer)
		switch {
		case err != nil:
			c.warn("the delivery checkpoint could not be read (%s): %v", clickhouse.Classify(err), err)
		case found < cp.Confirmed:
			c.fail("delivery checkpoint: the database acknowledged checkpoint %d, but holds %s: it was restored to an earlier state and lacks rows delivered after that point", cp.Confirmed, heldCheckpoint(found))
			recovery()
		default:
			c.ok("delivery checkpoint %d (acknowledged %s) is held by the database: no rollback detected", cp.Confirmed, cp.At.Local().Format(time.DateTime))
		}
	}
}

// checkIdentity compares the destination id the database reports now with
// the one recorded locally for the configured destination. Unlike the local
// state check, it also finds a replacement no collector has seen yet.
// reported says that the local state check already gave the recovery steps.
func checkIdentity(ctx context.Context, c *checker, cfg *config.Config, st clickhouse.SchemaStatus, reported bool) {
	if !st.Usable() {
		return // reported as a schema problem; there is no identity to compare
	}
	recorded := ""
	if store, err := spool.OpenReadOnly(statePath(cfg)); err == nil {
		if d, ok, _ := store.Lookup(ctx, cfg.ClickHouse.Endpoint(), cfg.ClickHouse.Database); ok && !store.Legacy() {
			recorded = d.ServerID
		}
		store.Close()
	}
	found := st.DestinationID
	switch {
	case found == "" && recorded != "":
		c.fail("the database reports no destination id, but Vario recorded %s for it: %s is missing or not readable for this user", recorded, clickhouse.MetaTable)
		c.hint("The collector delivers nothing while the database cannot be verified; queued rows are kept.")
		c.hint("Restore read access to the table, or run `vario migrate` if the database was recreated (it then counts as replaced; see `vario destination`).")
	case found == "":
		c.warn("the database has no destination id yet (schema from before version 2): a replaced database cannot be detected; run `vario migrate` to record one")
	case recorded == "":
		c.ok("destination id %s", found)
	case recorded == found:
		c.ok("destination id %s (the database Vario delivered to before)", found)
	default:
		c.fail("destination id %s does not match %s, which Vario recorded for this destination: the database was replaced", found, recorded)
		if reported {
			c.hint("See the recovery steps under \"Local state\" above.")
			return
		}
		c.hint("Delivery is refused and queued rows are kept until you decide.")
		for _, line := range strings.Split(collector.ReplacedRecovery, "\n") {
			c.hint(line)
		}
	}
}

func (a *app) checkClickHouse(ctx context.Context, c *checker, cfg *config.Config, replaced, rolledBack bool) {
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
		c.ok("operational password supplied via $%s", config.EnvPassword)
	} else if fileExists(cfg.PasswordPath()) {
		checkPrivate(c, cfg.PasswordPath(), "password file")
	} else {
		c.warn("no operational password configured (%s or $%s)", cfg.PasswordPath(), config.EnvPassword)
	}
	c.ok("operational user %s, database %s (the collector never changes the schema)", cfg.ClickHouse.Username, cfg.ClickHouse.Database)
	client, err := newClient(cfg)
	if err != nil {
		c.fail("client setup: %v", err)
		return
	}
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	rep, err := client.CheckConnection(cctx)
	if err != nil {
		var ce *clickhouse.CheckError
		kind := clickhouse.Classify(err)
		stage := "reach"
		if errors.As(err, &ce) {
			kind, stage = ce.Kind, ce.Stage
		}
		switch stage {
		case "reach":
			c.fail("endpoint unreachable (%s): %v", kind, err)
		case "authenticate":
			c.ok("endpoint reachable")
			c.fail("authentication or query failed (%s): %v", kind, err)
		default:
			c.ok("endpoint reachable")
			c.fail("cannot inspect database %s (%s): %v", cfg.ClickHouse.Database, kind, err)
		}
		c.hint(clickhouse.Hint(kind))
		return
	}
	var b strings.Builder
	printReport(&b, rep)
	for _, line := range strings.Split(strings.TrimRight(b.String(), "\n"), "\n") {
		t := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(t, "✓ "):
			c.ok("%s", t[len("✓ "):])
		case strings.HasPrefix(t, "✗ "):
			c.fail("%s", t[len("✗ "):])
		case strings.HasPrefix(t, "! "):
			c.warn("%s", t[2:])
		case strings.HasPrefix(t, "· "):
			c.info("%s", t[len("· "):])
		default:
			c.hint(t)
		}
	}
	if rep.Schema.DatabaseExists && rep.Schema.Usable() && !rep.Schema.Complete() && !rep.Schema.Newer() {
		c.info("run `vario migrate` to update the schema")
	}
	checkIdentity(ctx, c, cfg, rep.Schema, replaced)
	checkCheckpoint(cctx, c, cfg, client, rep.Schema, rolledBack)
}
