package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/nedanwr/vario/internal/config"
	"github.com/nedanwr/vario/internal/model"
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
		Long: `Init walks through the ClickHouse connection, database, collection mode and
data sources, optionally tests the connection, and writes the configuration
(default ~/.vario/config.toml) and the ClickHouse password file, both readable
only by you.

Init is safe to re-run: an existing configuration is offered for review with
its current values as defaults, and only the settings you change are
rewritten. Init never creates databases or tables unless you confirm it, and
never installs or starts the background service.

All prompts can be answered with flags. In scripts use --non-interactive;
with an existing configuration add --update (apply the given flags, keep
everything else) or --force (start over from defaults, keeping a backup).`,
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
	f.BoolVar(&o.createDatabase, "create-database", false, "allow Vario to create the database if it is missing (default: use an existing database)")
	f.StringVar(&o.username, "username", "", "ClickHouse user (default default)")
	f.BoolVar(&o.passwordStdin, "password-stdin", false, "read the ClickHouse password from stdin")
	f.StringVar(&o.mode, "mode", "", "collection mode: metrics, messages or full (default metrics)")
	f.BoolVar(&o.allowSensitive, "allow-sensitive-content", false, "confirm collecting conversation content (required for messages/full)")
	f.StringVar(&o.t3Path, "t3code-path", "", "path to T3 Code's statev2.sqlite")
	f.BoolVar(&o.noT3, "no-t3code", false, "do not enable the T3 Code source")
	f.BoolVar(&o.skipCheck, "skip-connection-check", false, "do not test the ClickHouse connection")
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
	replace  bool // replace an existing file (backup kept)
	cfg      *config.Config

	pwAction    pwAction
	pwValue     string
	pwStored    string // existing password file, if any
	pwFromEnv   bool
	sensitiveOK bool // sensitive content already confirmed in the existing configuration

	check     checkStatus
	checkNote string
	report    *clickhouse.Report
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
		p: newPrompter(a.stdin, a.stdout, interactive)}
	_, w.pwFromEnv = os.LookupEnv(config.EnvPassword)
	return w.run()
}

func (w *wizard) printf(format string, args ...any) { fmt.Fprintf(w.out, format, args...) }

func (w *wizard) run() error {
	interactive := w.p.interactive
	if interactive {
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
	if !interactive {
		return w.runNonInteractive()
	}
	return w.runInteractive()
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
		w.cfg = config.Default()
		w.cfg.SetPath(w.path)
		return true, nil
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
	switch choice {
	case 1:
		w.printf("The configuration was left unchanged.\n")
		return false, nil
	case 2:
		w.cfg = config.Default()
		w.cfg.SetPath(w.path)
		w.replace = true
		return true, nil
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

// ---- interactive flow -------------------------------------------------------

func (w *wizard) runInteractive() error {
	if err := w.connectionSteps(); err != nil {
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
		case 2:
			err = w.stepMode()
		case 3:
			err = w.stepSources()
		case 4:
			w.printf("Nothing was saved.\n")
			return nil
		}
		if err != nil {
			return err
		}
	}
}

func (w *wizard) connectionSteps() error {
	for {
		if err := w.stepEndpoint(); err != nil {
			return err
		}
		if err := w.stepAuth(); err != nil {
			return err
		}
		for {
			if err := w.stepDatabase(); err != nil {
				return err
			}
			next, err := w.stepValidate()
			if err != nil {
				return err
			}
			switch next {
			case nextContinue:
				return nil
			case nextDatabase:
				continue
			}
			break // nextConnection
		}
	}
}

func (w *wizard) stepEndpoint() error {
	w.printf("ClickHouse connection\n\n")
	def := w.cfg.ClickHouse.URL
	if def == "" {
		def = "http://localhost:8123"
	}
	for {
		u, err := w.p.askValid("ClickHouse endpoint (HTTP interface)", def, func(s string) error {
			if err := config.CheckURL(s); err != nil {
				return fmt.Errorf("the endpoint %v", err)
			}
			return nil
		})
		if err != nil {
			return err
		}
		if config.InsecureRemote(u) {
			w.printf("  Warning: plain http:// sends your data and credentials unencrypted over the network.\n")
			ok, err := w.p.confirm("  Use unencrypted HTTP for this remote server anyway?", false)
			if err != nil {
				return err
			}
			if !ok {
				def = "https://" + strings.TrimPrefix(u, "http://")
				continue
			}
			w.cfg.ClickHouse.AllowInsecureHTTP = true
		}
		if u != w.cfg.ClickHouse.URL {
			w.check = checkNotRun
		}
		w.cfg.ClickHouse.URL = u
		return nil
	}
}

func (w *wizard) stepAuth() error {
	user, err := w.p.askValid("ClickHouse username", w.cfg.ClickHouse.Username, func(s string) error {
		if s == "" {
			return errors.New("a username is required")
		}
		return nil
	})
	if err != nil {
		return err
	}
	w.cfg.ClickHouse.Username = user
	if w.pwFromEnv {
		w.printf("Using the password from $%s for this setup; it is not saved.\n\n", config.EnvPassword)
		return nil
	}
	if w.pwStored != "" || w.pwAction == pwSet {
		where := "is already stored in " + config.DisplayPath(w.pwStored)
		if w.pwAction == pwSet {
			where = "was entered"
		}
		c, err := w.p.choose("A ClickHouse password "+where+".", []string{"Keep it", "Enter a different password", "Use no password"}, 0)
		if err != nil {
			return err
		}
		switch c {
		case 0:
			w.printf("\n")
			return nil
		case 2:
			w.pwAction, w.pwValue = pwClear, ""
			w.printf("\n")
			return nil
		}
	}
	pw, err := w.p.secret("ClickHouse password (input hidden; leave empty for none)")
	if err != nil {
		return err
	}
	switch {
	case pw != "":
		w.pwAction, w.pwValue = pwSet, pw
	case w.pwStored != "":
		w.pwAction, w.pwValue = pwClear, ""
	default:
		w.pwAction, w.pwValue = pwKeep, ""
	}
	w.printf("\n")
	return nil
}

func (w *wizard) stepDatabase() error {
	db, err := w.p.askValid("Database name", w.cfg.ClickHouse.Database, func(s string) error {
		if err := config.CheckDatabaseName(s); err != nil {
			return fmt.Errorf("the database name %v", err)
		}
		return nil
	})
	if err != nil {
		return err
	}
	w.cfg.ClickHouse.Database = db
	def := 0
	if w.cfg.ClickHouse.CreateDatabase {
		def = 1
	}
	w.printf("\n")
	c, err := w.p.choose("Database management", []string{
		"Use an existing database (Vario never creates or removes databases)",
		"Allow Vario to create the database if it is missing",
	}, def)
	if err != nil {
		return err
	}
	w.cfg.ClickHouse.CreateDatabase = c == 1
	w.printf("\n")
	return nil
}

type nextStep int

const (
	nextContinue nextStep = iota
	nextConnection
	nextDatabase
)

func (w *wizard) stepValidate() (nextStep, error) {
	ok, err := w.p.confirm("Test the connection to ClickHouse now? (read-only; nothing is created)", true)
	if err != nil {
		return 0, err
	}
	if !ok {
		w.check, w.checkNote, w.report = checkNotRun, "not tested", nil
		w.printf("\n")
		return nextContinue, nil
	}
	for {
		w.printf("\n")
		rep, err := w.runCheck()
		if err != nil {
			c, cerr := w.p.choose("\nThe connection could not be verified. What would you like to do?", []string{
				"Retry",
				"Change the connection settings",
				"Continue without verifying (the settings are kept and can be saved)",
			}, 0)
			if cerr != nil {
				return 0, cerr
			}
			w.printf("\n")
			switch c {
			case 0:
				continue
			case 1:
				return nextConnection, nil
			}
			return nextContinue, nil
		}
		if rep.Ready() {
			w.printf("\n")
			return nextContinue, nil
		}
		var missingDB bool
		for _, is := range rep.Issues {
			missingDB = missingDB || is.Kind == clickhouse.KindDatabaseMissing
		}
		if missingDB {
			c, err := w.p.choose(fmt.Sprintf("\nDatabase %s does not exist (or %s cannot see it).", w.cfg.ClickHouse.Database, w.cfg.ClickHouse.Username), []string{
				"Allow Vario to create it when it first delivers data",
				"Use a different database",
				"Keep these settings (I will create the database myself)",
			}, 0)
			if err != nil {
				return 0, err
			}
			w.printf("\n")
			switch c {
			case 0:
				w.cfg.ClickHouse.CreateDatabase = true
				w.printf("Checking again with database creation enabled:\n")
				continue
			case 1:
				return nextDatabase, nil
			}
			return nextContinue, nil
		}
		c, err := w.p.choose("\nVario may not be able to deliver data with these settings.", []string{
			"Continue (fix the problems in ClickHouse later)",
			"Change the connection settings",
			"Retry the check",
		}, 0)
		if err != nil {
			return 0, err
		}
		w.printf("\n")
		switch c {
		case 1:
			return nextConnection, nil
		case 2:
			continue
		}
		return nextContinue, nil
	}
}

// password returns the password to use for checks before it is saved.
func (w *wizard) password() (string, error) {
	if p, ok := os.LookupEnv(config.EnvPassword); ok {
		return p, nil
	}
	switch w.pwAction {
	case pwSet:
		return w.pwValue, nil
	case pwClear:
		return "", nil
	}
	if w.pwStored == "" {
		return "", nil
	}
	b, err := os.ReadFile(w.pwStored)
	if err != nil {
		return "", fmt.Errorf("read password file: %w", err)
	}
	return strings.TrimRight(string(b), "\r\n"), nil
}

func (w *wizard) client() (*clickhouse.Client, error) {
	pw, err := w.password()
	if err != nil {
		return nil, err
	}
	return clickhouse.New(w.cfg.ClickHouse, pw)
}

// runCheck tests the connection and prints the findings.
func (w *wizard) runCheck() (*clickhouse.Report, error) {
	w.report = nil
	client, err := w.client()
	if err != nil {
		w.check, w.checkNote = checkFailed, err.Error()
		w.printf("  ✗ %v\n", err)
		return nil, err
	}
	timeout := w.cfg.ClickHouse.Timeout.Duration
	if timeout <= 0 || timeout > 15*time.Second {
		timeout = 15 * time.Second
	}
	ctx, cancel := context.WithTimeout(w.ctx, timeout)
	defer cancel()
	rep, err := client.CheckConnection(ctx)
	if err != nil {
		var ce *clickhouse.CheckError
		kind := clickhouse.Classify(err)
		if errors.As(err, &ce) {
			kind = ce.Kind
			if ce.Stage != "reach" {
				w.printf("  ✓ endpoint reachable (%s)\n", w.cfg.ClickHouse.URL)
			}
		}
		w.check, w.checkNote = checkFailed, fmt.Sprintf("%s failure", kind)
		w.printf("  ✗ %s failure: %v\n    %s\n", kind, err, clickhouse.Hint(kind))
		return nil, err
	}
	w.report = rep
	printReport(w.out, rep)
	if rep.Ready() {
		w.check, w.checkNote = checkVerified, "verified (ClickHouse "+rep.Version+")"
	} else {
		w.check, w.checkNote = checkVerified, "connected, with problems (see above)"
	}
	return rep, nil
}

// printReport prints a connection report as check lines.
func printReport(out io.Writer, r *clickhouse.Report) {
	fmt.Fprintf(out, "  ✓ endpoint reachable\n  ✓ authenticated as %s (ClickHouse %s)\n", r.User, r.Version)
	st := r.Schema
	switch {
	case st.DatabaseExists:
		fmt.Fprintf(out, "  ✓ database %s exists\n", r.Database)
	case r.CreateDatabase:
		fmt.Fprintf(out, "  · database %s does not exist yet; Vario will create it on first delivery (create_database = true)\n", r.Database)
	default:
		fmt.Fprintf(out, "  ✗ database %s does not exist or %s has no access to it\n", r.Database, r.User)
	}
	if st.DatabaseExists {
		switch {
		case len(st.Problems) > 0:
			for _, p := range st.Problems {
				fmt.Fprintf(out, "  ✗ incompatible schema: %s\n", p)
			}
		case len(st.MissingTables) == len(model.Tables):
			fmt.Fprintf(out, "  · no Vario tables yet; they are created on first delivery\n")
		case len(st.MissingTables) > 0 || len(st.MissingViews) > 0:
			fmt.Fprintf(out, "  · Vario schema incomplete (missing: %s); completed on first delivery\n", strings.Join(append(append([]string{}, st.MissingTables...), st.MissingViews...), ", "))
		default:
			fmt.Fprintf(out, "  ✓ Vario tables present and compatible\n")
		}
		if len(st.OutdatedViews) > 0 {
			fmt.Fprintf(out, "  · outdated views (updated on next delivery): %s\n", strings.Join(st.OutdatedViews, ", "))
		}
	}
	if r.PrivilegesUnknown != "" {
		fmt.Fprintf(out, "  ! %s\n", r.PrivilegesUnknown)
	}
	var granted, missing, optional []string
	for _, p := range r.Privileges {
		switch {
		case p.Granted:
			granted = append(granted, p.Grant)
		case p.Required:
			missing = append(missing, p.Grant)
		default:
			optional = append(optional, p.Grant)
		}
	}
	if len(granted) > 0 {
		fmt.Fprintf(out, "  ✓ privileges: %s\n", strings.Join(granted, ", "))
	}
	for _, g := range missing {
		fmt.Fprintf(out, "  ✗ missing privilege: %s\n", g)
	}
	for _, g := range optional {
		fmt.Fprintf(out, "  ! missing optional privilege: %s (needed to update outdated views)\n", g)
	}
	kinds := map[clickhouse.Kind]bool{}
	for _, is := range r.Issues {
		if !kinds[is.Kind] {
			kinds[is.Kind] = true
			fmt.Fprintf(out, "    %s\n", clickhouse.Hint(is.Kind))
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

func (w *wizard) passwordSummary() string {
	switch {
	case w.pwFromEnv:
		return "from $" + config.EnvPassword + " (not saved)"
	case w.pwAction == pwSet:
		return "new password, saved to " + config.DisplayPath(w.cfg.PasswordPath())
	case w.pwAction == pwClear && w.pwStored != "":
		return "none (the stored password will be removed)"
	case w.pwStored != "":
		return "stored in " + config.DisplayPath(w.pwStored) + " (unchanged)"
	}
	return "none"
}

func (w *wizard) summary() {
	c := w.cfg
	dbMode := "existing database (Vario will not create it)"
	if c.ClickHouse.CreateDatabase {
		dbMode = "created by Vario if missing"
	}
	if w.check == checkNotRun && w.checkNote == "" {
		w.checkNote = "not tested"
	}
	srcs := "none"
	var parts []string
	for _, s := range c.Sources {
		if s.Enabled {
			parts = append(parts, fmt.Sprintf("%s (%s)", s.Kind, config.DisplayPath(sourcePath(s))))
		}
	}
	if len(parts) > 0 {
		srcs = strings.Join(parts, ", ")
	}
	w.printf("Summary\n\n")
	rows := [][2]string{
		{"ClickHouse endpoint", c.ClickHouse.URL},
		{"Username", c.ClickHouse.Username},
		{"Password", w.passwordSummary()},
		{"Database", c.ClickHouse.Database + " — " + dbMode},
		{"Connection", w.checkNote},
		{"Collection mode", c.Privacy.Mode},
		{"Sources", srcs},
		{"Configuration file", config.DisplayPath(w.path)},
	}
	for _, r := range rows {
		w.printf("  %-20s %s\n", r[0], r[1])
	}
	w.printf("\n")
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
		c.ClickHouse.CreateDatabase = o.createDatabase
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
	if !o.skipCheck {
		fmt.Fprintf(w.out, "Checking ClickHouse at %s...\n", c.ClickHouse.URL)
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

func (w *wizard) saveAndFinish() error {
	saved, err := w.save()
	if err != nil {
		return err
	}
	if saved {
		w.printf("✓ Configuration saved\n  %s\n", config.DisplayPath(w.path))
	} else {
		w.printf("✓ No configuration changes; %s was left untouched\n", config.DisplayPath(w.path))
	}
	if w.pwAction == pwSet {
		w.printf("✓ Password saved to %s (readable only by you)\n", config.DisplayPath(w.cfg.PasswordPath()))
	} else if w.pwAction == pwClear && w.pwStored != "" {
		w.printf("✓ Stored password removed\n")
	}
	if w.check != checkVerified {
		w.printf("! ClickHouse connectivity is not verified (%s). Check it later with `vario doctor`.\n", w.checkNote)
	}
	if err := w.offerProvisioning(); err != nil {
		return err
	}
	serviceActive := w.a.serviceNotice(w.path, saved || w.pwAction != pwKeep)
	w.printf("\nNext steps:\n\n  vario doctor\n")
	if _, ok := w.cfg.Source(config.SourceT3Code); ok {
		w.printf("  vario backfill --source t3code\n")
	} else {
		w.printf("  vario sources detect\n")
	}
	w.printf("  vario run\n")
	if !serviceActive {
		w.printf("\nTo run in the background:\n  vario service install\n")
	}
	return nil
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

// offerProvisioning offers to create the database or tables now, after an
// explicit confirmation. Delivery creates them automatically otherwise.
func (w *wizard) offerProvisioning() error {
	r := w.report
	if !w.p.interactive || r == nil || len(r.Schema.Problems) > 0 {
		return nil
	}
	for _, is := range r.Issues {
		if is.Kind == clickhouse.KindPermission {
			return nil
		}
	}
	st := r.Schema
	var q string
	switch {
	case !st.DatabaseExists && w.cfg.ClickHouse.CreateDatabase:
		q = fmt.Sprintf("Create database %s and Vario's tables now?", w.cfg.ClickHouse.Database)
	case st.DatabaseExists && (len(st.MissingTables) > 0 || len(st.MissingViews) > 0):
		q = fmt.Sprintf("Create Vario's tables and views in database %s now?", w.cfg.ClickHouse.Database)
	default:
		return nil
	}
	w.printf("\nVario creates what it needs automatically when it first delivers data.\n")
	ok, err := w.p.confirm(q+" (nothing existing is changed)", false)
	if errors.Is(err, errInputEnded) {
		return nil // the configuration is already saved; treat as "no"
	}
	if err != nil || !ok {
		return err
	}
	client, err := w.client()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(w.ctx, time.Minute)
	defer cancel()
	res, err := client.InitSchema(ctx)
	if err != nil {
		kind := clickhouse.Classify(err)
		w.printf("✗ %v\n  %s\n  The configuration is saved; fix the problem and run `vario doctor --create-schema`.\n", err, clickhouse.Hint(kind))
		return nil
	}
	printSchemaResult(w.out, w.cfg.ClickHouse.Database, res)
	return nil
}

func printSchemaResult(out io.Writer, db string, res clickhouse.SchemaResult) {
	if res.CreatedDatabase {
		fmt.Fprintf(out, "✓ created database %s\n", db)
	}
	if n := len(res.CreatedTables) + len(res.CreatedViews); n > 0 {
		fmt.Fprintf(out, "✓ created %d tables and %d views in %s\n", len(res.CreatedTables), len(res.CreatedViews), db)
	}
	if len(res.UpdatedViews) > 0 {
		fmt.Fprintf(out, "✓ updated views: %s\n", strings.Join(res.UpdatedViews, ", "))
	}
	if !res.CreatedDatabase && len(res.CreatedTables)+len(res.CreatedViews)+len(res.UpdatedViews) == 0 {
		fmt.Fprintf(out, "✓ schema in %s is already up to date\n", db)
	}
	for _, wn := range res.Warnings {
		fmt.Fprintf(out, "! %s\n", wn)
	}
}
