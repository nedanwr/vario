package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/nedanwr/vario/internal/config"
	"github.com/nedanwr/vario/internal/sources/t3code"
	"github.com/nedanwr/vario/internal/storage/clickhouse"
)

type initOptions struct {
	nonInteractive bool
	force          bool
	update         bool
	url            string
	database       string
	createDatabase bool
	username       string
	passwordStdin  bool
	mode           string
	allowSensitive bool
	t3Path         string
	noT3           bool
	skipCheck      bool
	dataDir        string

	flags *pflag.FlagSet
}

func (o initOptions) set(name string) bool { return o.flags != nil && o.flags.Changed(name) }

func (a *app) initCmd() *cobra.Command {
	var o initOptions
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Set up or update the Vario configuration interactively",
		Long: `Init sets up Vario's ClickHouse access, collection mode and data sources, and
writes the configuration (default ~/.vario/config.toml) and the operational
password file, both readable only by you.

ClickHouse access uses two kinds of credentials:

  administrative  asked for only during guided setup (and by 'vario migrate')
                  to list and create databases and users, grant privileges and
                  create the schema; never saved
  operational     a restricted user that the collector, backfill and the
                  background service use; its password is saved

Guided setup signs in with an administrative account, lets you pick an
existing database or create one, pick an existing user or create a dedicated
one (with a generated password if you like), shows every change it would make
and applies it only after you confirm. Manually managed setup uses an existing
database and operational user without administrative credentials. Either way,
the operational login is verified before saving.

The password of a user created by guided setup is kept in
pending-credentials.json (next to the configuration, readable only by you)
from before the user is created until a configuration using it is saved, so
an interrupted setup (Ctrl-C, closed input, a crash) never loses it; the next
init offers to finish the setup with it.

Init is safe to re-run: existing databases, users and schema objects are
reused, never recreated, and an existing user's privileges are changed only
with your explicit confirmation. Only the settings you change are rewritten.
Init never installs or starts the background service.

In scripts use --non-interactive (manually managed setup); with an existing
configuration add --update (apply the given flags, keep everything else) or
--force (start over from defaults, keeping a backup). Then create the schema
with 'vario migrate'.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			o.flags = cmd.Flags()
			return a.runInit(cmd.Context(), o)
		},
	}
	f := cmd.Flags()
	f.BoolVar(&o.nonInteractive, "non-interactive", false, "do not prompt; use flags and defaults")
	f.BoolVar(&o.update, "update", false, "with an existing configuration: apply the given flags and keep all other settings")
	f.BoolVar(&o.force, "force", false, "replace an existing configuration with defaults and the given flags (a backup is kept)")
	f.StringVar(&o.url, "clickhouse-url", "", "ClickHouse HTTP(S) endpoint, e.g. https://ch.example.com:8443")
	f.StringVar(&o.database, "database", "", "ClickHouse database (default vario)")
	f.BoolVar(&o.createDatabase, "create-database", false, "deprecated and ignored: create the database with `vario migrate`")
	_ = f.MarkHidden("create-database")
	f.StringVar(&o.username, "username", "", "operational ClickHouse user (default default)")
	f.BoolVar(&o.passwordStdin, "password-stdin", false, "read the operational ClickHouse password from stdin")
	f.StringVar(&o.mode, "mode", "", "collection mode: metrics, messages or full (default metrics)")
	f.BoolVar(&o.allowSensitive, "allow-sensitive-content", false, "confirm collecting conversation content (required for messages/full)")
	f.StringVar(&o.t3Path, "t3code-path", "", "path to T3 Code's statev2.sqlite")
	f.BoolVar(&o.noT3, "no-t3code", false, "do not enable the T3 Code source")
	f.BoolVar(&o.skipCheck, "skip-connection-check", false, "do not verify the operational ClickHouse login")
	f.StringVar(&o.dataDir, "data-dir", "", "directory for the spool and state (default ~/.vario/data)")
	return cmd
}

type pwAction int

const (
	pwKeep  pwAction = iota // leave the stored password (or its absence) as it is
	pwSet                   // store a new password
	pwClear                 // remove the stored password
)

type checkStatus int

const (
	checkNotRun checkStatus = iota
	checkVerified
	checkPartial // no problems found, some checks inconclusive
	checkFailed
)

type wizard struct {
	a    *app
	ctx  context.Context
	p    *prompter
	o    initOptions
	out  io.Writer
	loc  config.Location
	path string // the file that will be written

	orig     *config.Config // existing configuration, nil when starting fresh
	origData []byte
	// prev is the configuration found on disk (even when starting over), used
	// to keep queued data bound to its destination.
	prev    *config.Config
	replace bool // replace an existing file (backup kept)
	cfg     *config.Config

	pwAction    pwAction
	pwValue     string
	pwStored    string // existing password file, if any
	pwFromEnv   bool
	sensitiveOK bool // sensitive content already confirmed in the existing configuration
	pwGenerated bool // pwValue was generated by Vario

	// ClickHouse setup. The administrative connection (and its password)
	// lives only in memory for the duration of init.
	setup       string // "guided" or "manual"
	admin       *clickhouse.Admin
	adminUser   string
	dbState     string // how the database was chosen, for the summary
	userState   string // how the operational user was chosen
	privState   string // privilege changes made by this setup
	schemaState string // schema changes made by this setup
	changedCH   bool   // something was changed in ClickHouse

	check     checkStatus
	checkNote string
	report    *clickhouse.Report

	// Passwords of users created by guided setup that no saved configuration
	// uses yet (see pendingCredentials).
	pend      *pendingStore
	pendErr   error
	pendPath  string
	creating  string            // name of the user the current plan creates
	newUsers  map[string]string // endpoint|user -> "created" or "unknown" (this run)
	pendFound map[string]bool   // endpoint|user entries that existed before this run
	resumed   *pendingCredentials
	applying  bool // a confirmed plan is being applied
}

func (a *app) runInit(ctx context.Context, o initOptions) error {
	if o.force && o.update {
		return withCode(ExitUsage, errors.New("--force and --update cannot be combined"))
	}
	loc, err := a.resolveLocation()
	if err != nil {
		return withCode(ExitUsage, err)
	}
	interactive := !o.nonInteractive
	if _, ok := a.stdin.(*os.File); ok && interactive && !isTerminal(a.stdin) {
		interactive = false
		fmt.Fprintln(a.stderr, "stdin is not a terminal; continuing non-interactively with flags and defaults")
	}
	w := &wizard{a: a, ctx: ctx, o: o, out: a.stdout, loc: loc, path: loc.Path,
		p: newPrompter(a.stdin, a.stdout, interactive), pendPath: pendingPath(loc.Path),
		newUsers: map[string]string{}, pendFound: map[string]bool{}}
	_, w.pwFromEnv = os.LookupEnv(config.EnvPassword)
	return w.run()
}

func (w *wizard) printf(format string, args ...any) { fmt.Fprintf(w.out, format, args...) }

func (w *wizard) run() error {
	interactive := w.p.interactive
	if interactive {
		// Ctrl-C at any prompt restores the terminal and explains what was
		// applied and what was kept, instead of silently killing the process.
		defer w.p.trapInterrupts("Setup interrupted.\nNothing was saved.\n")()
		w.welcome()
	}
	proceed, err := w.loadExisting()
	if err != nil || !proceed {
		return err
	}
	// A password file may exist even without a configuration (or when
	// starting over); offer to keep it rather than silently reusing it.
	if w.pwStored == "" && w.cfg.HasStoredPassword() {
		w.pwStored = w.cfg.PasswordPath()
	}
	if err := w.reviewPending(); err != nil {
		return err
	}
	if !interactive {
		return w.runNonInteractive()
	}
	err = w.runInteractive()
	if err != nil {
		w.printf("\n")
		w.unsavedNotice(false)
	}
	return err
}

// updateNotice prepares what an interrupt (Ctrl-C) prints at this point.
func (w *wizard) updateNotice() {
	w.p.setNotice("Setup interrupted.\n" + w.unsavedText(true))
}

// unsavedNotice explains what remains after the wizard ends without saving:
// changes applied in ClickHouse and the passwords of users it created, which
// are kept for the next `vario init`. Nothing is said when nothing happened,
// unless always is set.
func (w *wizard) unsavedNotice(always bool) { w.printf("%s", w.unsavedText(always)) }

func (w *wizard) unsavedText(always bool) string {
	var b strings.Builder
	users := w.unsavedUsers()
	for _, k := range users {
		e := w.pend.find(splitKey(k))
		if e == nil {
			continue
		}
		verb := "was created"
		if w.newUsers[k] != "created" {
			verb = "may have been created (setup stopped before ClickHouse confirmed it)"
		}
		fmt.Fprintf(&b, "! ClickHouse user %s %s at %s, but no configuration using it was saved.\n"+
			"  Its password is kept in %s (readable only by you).\n"+
			"  Run `vario init` again to finish the setup with it.\n", e.User, verb, e.Endpoint, config.DisplayPath(w.pendPath))
	}
	switch {
	case w.applying:
		b.WriteString("! Setup stopped while applying confirmed changes in ClickHouse; some of them may have been applied.\n" +
			"  Review with `vario init` or `vario migrate --dry-run`. The configuration was not saved.\n")
	case w.changedCH:
		b.WriteString("! Changes confirmed earlier were applied in ClickHouse and remain in effect; the configuration was not saved.\n")
	case len(users) == 0 && always:
		b.WriteString("Nothing was saved.\n")
	}
	return b.String()
}

func pendKey(endpoint, user string) string { return config.NormalizeEndpoint(endpoint) + "|" + user }

func splitKey(k string) (string, string) {
	i := strings.LastIndexByte(k, '|')
	return k[:i], k[i+1:]
}

// unsavedUsers returns the users created (or possibly created) in this run
// whose password is pending, in a stable order.
func (w *wizard) unsavedUsers() []string {
	var out []string
	if w.pend == nil {
		return nil
	}
	for _, e := range w.pend.Users {
		if _, ok := w.newUsers[pendKey(e.Endpoint, e.User)]; ok {
			out = append(out, pendKey(e.Endpoint, e.User))
		}
	}
	return out
}

func (w *wizard) welcome() {
	w.printf(`Welcome to Vario — observe everything AI.

Vario reads AI activity recorded by applications on this computer (currently
T3 Code): token usage and, if you choose, conversations and tool executions.
It stores that data in a ClickHouse database you choose, and nowhere else.
Vario sends no telemetry to its developers. You decide what is collected and
where it goes, and can change both later with `+"`vario config`"+`.

This setup writes %s. It changes nothing in ClickHouse
unless you explicitly ask it to.

`, config.DisplayPath(w.path))
}

// loadExisting handles an existing configuration. It returns false when the
// user chose to keep the configuration unchanged.
func (w *wizard) loadExisting() (bool, error) {
	data, err := os.ReadFile(w.loc.Path)
	if errors.Is(err, os.ErrNotExist) {
		return true, w.startFresh()
	}
	if err != nil {
		return false, withCode(ExitFailure, fmt.Errorf("cannot read the existing configuration %s: %w", w.loc.Path, err))
	}
	parsed, perr := config.Parse(w.loc.Path, data)

	choice := 0 // 0 update, 1 keep, 2 start over
	switch {
	case w.o.force:
		choice = 2
	case !w.p.interactive && !w.o.update:
		return false, withCode(ExitUsage, fmt.Errorf("already configured (%s); use --update to change the given settings and keep the rest, or --force to start over (a backup is kept)", w.loc.Path))
	case !w.p.interactive && perr != nil:
		return false, withCode(ExitUsage, fmt.Errorf("the existing configuration cannot be updated: %v\nFix it with `vario config edit`, or use --force to start over (a backup is kept)", perr))
	case !w.p.interactive:
	case perr != nil:
		w.printf("The existing configuration %s cannot be read:\n  %s\n\n", config.DisplayPath(w.loc.Path), strings.ReplaceAll(perr.Error(), "\n", "\n  "))
		c, err := w.p.choose("What would you like to do?", []string{
			"Exit and fix it with `vario config edit`",
			"Start over from defaults (the current file is kept as a backup)",
		}, 0)
		if err != nil {
			return false, err
		}
		if c == 0 {
			w.printf("Nothing was changed.\n")
			return false, nil
		}
		choice = 2
	default:
		w.printf("Vario is already configured (%s).\n", config.DisplayPath(w.path))
		c, err := w.p.choose("", []string{
			"Review and update the configuration (current values are the defaults)",
			"Keep the current configuration and exit",
			"Start over from defaults (the current file is kept as a backup)",
		}, 0)
		if err != nil {
			return false, err
		}
		w.printf("\n")
		choice = c
	}
	w.prev = parsed
	switch choice {
	case 1:
		w.printf("The configuration was left unchanged.\n")
		return false, nil
	case 2:
		w.replace = true
		return true, w.startFresh()
	}
	w.orig, w.origData = parsed, data
	w.cfg = parsed.Clone()
	w.cfg.SetPath(w.path)
	w.sensitiveOK = parsed.Privacy.AllowSensitiveContent
	if parsed.HasStoredPassword() {
		w.pwStored = parsed.PasswordPath()
	}
	return true, nil
}

// startFresh begins from the defaults. The data directory is written as an
// absolute path, so that the background service, which does not inherit
// $VARIO_HOME, uses the same spool, cursors and lease as this shell.
func (w *wizard) startFresh() error {
	w.cfg = config.Default()
	w.cfg.SetPath(w.path)
	d, err := config.DefaultDataDir()
	if err != nil {
		return err
	}
	w.cfg.Collector.DataDir = d
	return nil
}

// ---- interactive flow -------------------------------------------------------

func (w *wizard) runInteractive() error {
	if err := w.resumeOrConnect(); err != nil {
		if errors.Is(err, errCancelled) {
			w.printf("Setup cancelled.\n")
			w.unsavedNotice(true)
			return nil
		}
		return err
	}
	if err := w.stepMode(); err != nil {
		return err
	}
	if err := w.stepSources(); err != nil {
		return err
	}
	for {
		w.summary()
		verr := w.cfg.Validate()
		if verr != nil {
			w.printf("\nThe configuration is not valid yet:\n  %s\n", strings.ReplaceAll(verr.Error(), "\n", "\n  "))
		}
		opts := []string{"Save this configuration", "Change the ClickHouse connection or database", "Change the collection mode", "Change the data sources", "Quit without saving"}
		def := 0
		if verr != nil {
			opts[0] = "Save (not available until the problems above are fixed)"
			def = 1
		}
		c, err := w.p.choose("", opts, def)
		if err != nil {
			return err
		}
		w.printf("\n")
		switch c {
		case 0:
			if verr != nil {
				continue
			}
			return w.saveAndFinish()
		case 1:
			err = w.connectionSteps()
			if errors.Is(err, errCancelled) {
				err = nil
			}
		case 2:
			err = w.stepMode()
		case 3:
			err = w.stepSources()
		case 4:
			quit, qerr := w.confirmQuit()
			if qerr != nil {
				return qerr
			}
			if quit {
				return nil
			}
		}
		if err != nil {
			return err
		}
	}
}

func (w *wizard) stepMode() error {
	w.printf("What should Vario collect?\n")
	def := map[string]int{config.ModeMetrics: 0, config.ModeMessages: 1, config.ModeFull: 2}[w.cfg.Privacy.Mode]
	c, err := w.p.choose("", []string{
		"Metrics only — token usage, models, timings, tool names and statuses; no conversation content",
		"Messages and metrics — adds your and the assistant's messages, conversation and project titles",
		"Full execution telemetry — adds commands, tool inputs and outputs, file changes, reasoning and raw events",
	}, def)
	if err != nil {
		return err
	}
	mode := []string{config.ModeMetrics, config.ModeMessages, config.ModeFull}[c]
	w.printf("\n")
	if mode == config.ModeMetrics {
		w.cfg.Privacy.Mode = mode
		return nil
	}
	if w.sensitiveOK && w.orig != nil && w.orig.Privacy.Mode == mode {
		w.printf("Keeping the %s mode you confirmed earlier.\n\n", mode)
		w.cfg.Privacy.Mode, w.cfg.Privacy.AllowSensitiveContent = mode, true
		return nil
	}
	w.printf(`The %q mode copies conversation content to ClickHouse. Prompts, transcripts,
source code and command or tool output can contain credentials, private files
and proprietary code. Known secret formats are redacted before data leaves this
machine, but redaction cannot catch everything. Protect the ClickHouse database
accordingly.
`, mode)
	ans, err := w.p.ask("Type 'yes' to collect conversation content", "")
	if err != nil {
		return err
	}
	if !strings.EqualFold(ans, "yes") {
		w.printf("Not confirmed; using the metrics mode.\n\n")
		w.cfg.Privacy.Mode = config.ModeMetrics
		w.cfg.Privacy.AllowSensitiveContent = false
		return nil
	}
	w.cfg.Privacy.Mode, w.cfg.Privacy.AllowSensitiveContent = mode, true
	w.sensitiveOK = true
	w.printf("\n")
	return nil
}

func (w *wizard) stepSources() error {
	w.printf("Data sources\n\n")
	sc, configured := w.cfg.Source(config.SourceT3Code)
	candidates := t3code.DefaultPaths()
	if w.o.t3Path != "" {
		p, err := config.ExpandPath(w.o.t3Path)
		if err != nil {
			return withCode(ExitUsage, err)
		}
		candidates = []string{p}
	} else if configured && sc.Path != "" {
		candidates = append([]string{sc.Path}, candidates...)
	}
	found := ""
	for _, d := range t3code.Detect(w.ctx, candidates) {
		if d.Compatible {
			found = d.Path
			break
		}
	}
	if found != "" {
		w.printf("  [x] T3 Code — detected at %s\n\n", config.DisplayPath(found))
	} else {
		w.printf("  [ ] T3 Code — not detected in the default location\n\n")
	}
	enableDef := found != ""
	if configured {
		enableDef = sc.Enabled
	}
	label := "Enable T3 Code?"
	if found == "" {
		label = "Enable T3 Code anyway (for example at a custom path)?"
	}
	enable, err := w.p.confirm(label, enableDef)
	if err != nil {
		return err
	}
	if !enable {
		if configured {
			sc.Enabled = false
			w.cfg.UpsertSource(sc)
		}
		w.printf("\nNo source enabled. Add one later with `vario sources add t3code --path <statev2.sqlite>`.\n\n")
		return nil
	}
	def := found
	if def == "" {
		def = candidates[0]
	}
	if configured && sc.Path != "" && (found == "" || found == sc.Path) {
		def = sc.Path
	}
	for {
		p, err := w.p.ask("T3 Code database path", config.DisplayPath(def))
		if err != nil {
			return err
		}
		if p, err = config.ExpandPath(p); err != nil {
			w.printf("  %v\n", err)
			continue
		}
		d := t3code.Detect(w.ctx, []string{p})[0]
		if !d.Compatible {
			w.printf("  %s: %s\n", config.DisplayPath(p), d.Detail)
			keep, err := w.p.confirm("  Use this path anyway? Vario will read it once it exists", false)
			if err != nil {
				return err
			}
			if !keep {
				def = p
				continue
			}
		}
		w.cfg.UpsertSource(config.SourceConfig{Kind: config.SourceT3Code, Enabled: true, Path: p})
		w.printf("\n")
		return nil
	}
}

// ---- non-interactive flow ---------------------------------------------------

func (w *wizard) runNonInteractive() error {
	o, c := w.o, w.cfg
	fresh := w.orig == nil
	if o.set("clickhouse-url") || fresh {
		u := o.url
		if u == "" {
			u = c.ClickHouse.URL
		}
		if u == "" {
			u = "http://localhost:8123"
		}
		if err := config.CheckURL(u); err != nil {
			return withCode(ExitUsage, fmt.Errorf("--clickhouse-url %v", err))
		}
		if config.InsecureRemote(u) && !c.ClickHouse.AllowInsecureHTTP {
			return withCode(ExitUsage, errors.New("refusing unencrypted http:// for a remote ClickHouse; use https:// (or set clickhouse.allow_insecure_http = true with `vario config set`)"))
		}
		c.ClickHouse.URL = u
	}
	if o.set("database") {
		c.ClickHouse.Database = o.database
	}
	if o.set("create-database") {
		fmt.Fprintln(w.a.stderr, "warning: --create-database is deprecated and ignored; create the database and schema with `vario migrate`")
	}
	if o.set("username") {
		c.ClickHouse.Username = o.username
	}
	if o.set("mode") {
		switch o.mode {
		case config.ModeMetrics:
		case config.ModeMessages, config.ModeFull:
			if !o.allowSensitive && !(w.orig != nil && w.orig.Privacy.AllowSensitiveContent) {
				return withCode(ExitUsage, fmt.Errorf("mode %q collects conversation content and requires --allow-sensitive-content", o.mode))
			}
			c.Privacy.AllowSensitiveContent = true
		default:
			return withCode(ExitUsage, fmt.Errorf("invalid mode %q (choose metrics, messages or full)", o.mode))
		}
		c.Privacy.Mode = o.mode
	}
	if o.set("data-dir") {
		d, err := config.ExpandPath(o.dataDir)
		if err != nil {
			return withCode(ExitUsage, err)
		}
		c.Collector.DataDir = d
	} else if fresh {
		d, err := config.DefaultDataDir()
		if err != nil {
			return err
		}
		c.Collector.DataDir = d
	}
	if err := w.nonInteractiveSources(fresh); err != nil {
		return err
	}
	if o.passwordStdin {
		b, err := io.ReadAll(io.LimitReader(w.p.r, 4096))
		if err != nil {
			return err
		}
		if pw := strings.TrimRight(string(b), "\r\n"); pw != "" {
			w.pwAction, w.pwValue = pwSet, pw
		}
	} else if w.pwFromEnv {
		fmt.Fprintf(w.out, "Using the password from $%s (it is not written to disk).\n", config.EnvPassword)
	}
	if err := c.Validate(); err != nil {
		return withCode(ExitUsage, fmt.Errorf("invalid settings:\n  %s", strings.ReplaceAll(err.Error(), "\n", "\n  ")))
	}
	w.setup = "manual"
	if !o.skipCheck {
		fmt.Fprintf(w.out, "Checking ClickHouse at %s as %s...\n", c.ClickHouse.URL, c.ClickHouse.Username)
		_, _ = w.runCheck()
	} else {
		w.checkNote = "not tested (--skip-connection-check)"
	}
	return w.saveAndFinish()
}

func (w *wizard) nonInteractiveSources(fresh bool) error {
	o := w.o
	switch {
	case o.noT3:
		if sc, ok := w.cfg.Source(config.SourceT3Code); ok {
			sc.Enabled = false
			w.cfg.UpsertSource(sc)
		}
	case o.t3Path != "":
		p, err := config.ExpandPath(o.t3Path)
		if err != nil {
			return withCode(ExitUsage, err)
		}
		d := t3code.Detect(w.ctx, []string{p})[0]
		printDetection(w.out, d)
		w.cfg.UpsertSource(config.SourceConfig{Kind: config.SourceT3Code, Enabled: true, Path: p})
	case fresh:
		for _, d := range t3code.Detect(w.ctx, t3code.DefaultPaths()) {
			printDetection(w.out, d)
			if d.Compatible {
				w.cfg.UpsertSource(config.SourceConfig{Kind: config.SourceT3Code, Enabled: true, Path: d.Path})
				break
			}
		}
	}
	return nil
}

// ---- saving -------------------------------------------------------------------

func (w *wizard) changes() []config.Change {
	if w.orig == nil {
		return nil
	}
	return config.Diff(w.orig, w.cfg)
}

// save writes the configuration and password. It returns false when the
// configuration file did not need to change.
func (w *wizard) save() (bool, error) {
	saved := true
	switch {
	case w.orig != nil:
		ch := w.changes()
		if len(ch) == 0 {
			saved = false
			break
		}
		if _, err := config.Update(w.path, w.orig.Fingerprint(), ch); err != nil {
			return false, fmt.Errorf("save configuration: %w", err)
		}
	default:
		backup, err := w.cfg.Create(w.replace)
		if errors.Is(err, config.ErrExists) {
			return false, fmt.Errorf("%w; it was created while init was running and was not overwritten", err)
		}
		if err != nil {
			return false, fmt.Errorf("save configuration: %w", err)
		}
		if backup != "" {
			w.printf("  The previous configuration was backed up to %s\n", config.DisplayPath(backup))
		}
	}
	switch w.pwAction {
	case pwSet:
		if err := w.cfg.WritePassword(w.pwValue); err != nil {
			return saved, fmt.Errorf("save password: %w", err)
		}
	case pwClear:
		if err := w.cfg.ClearPassword(); err != nil {
			return saved, fmt.Errorf("remove password: %w", err)
		}
	}
	return saved, nil
}
