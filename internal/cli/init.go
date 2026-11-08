package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/nedanwr/vario/internal/config"
	"github.com/nedanwr/vario/internal/sources/t3code"
	"github.com/nedanwr/vario/internal/storage/clickhouse"
)

type initOptions struct {
	nonInteractive bool
	force          bool
	url            string
	database       string
	username       string
	passwordStdin  bool
	mode           string
	allowSensitive bool
	t3Path         string
	noT3           bool
	skipCheck      bool
	dataDir        string
}

func (a *app) initCmd() *cobra.Command {
	var o initOptions
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Create the Vario configuration interactively",
		Long: `Init writes the configuration file and the ClickHouse password file (both
readable only by you). It detects supported sources, asks for ClickHouse
connection details and the collection mode, and optionally tests the
connection. Init never installs or starts the background service.

All prompts can be answered with flags; use --non-interactive in scripts.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.runInit(cmd.Context(), o)
		},
	}
	f := cmd.Flags()
	f.BoolVar(&o.nonInteractive, "non-interactive", false, "do not prompt; use flags and defaults")
	f.BoolVar(&o.force, "force", false, "overwrite an existing configuration")
	f.StringVar(&o.url, "clickhouse-url", "", "ClickHouse HTTP(S) endpoint, e.g. https://ch.example.com:8443")
	f.StringVar(&o.database, "database", "", "ClickHouse database (default vario)")
	f.StringVar(&o.username, "username", "", "ClickHouse user (default default)")
	f.BoolVar(&o.passwordStdin, "password-stdin", false, "read the ClickHouse password from stdin")
	f.StringVar(&o.mode, "mode", "", "collection mode: metrics, messages or full (default metrics)")
	f.BoolVar(&o.allowSensitive, "allow-sensitive-content", false, "confirm collecting conversation content (required for messages/full)")
	f.StringVar(&o.t3Path, "t3code-path", "", "path to T3 Code's statev2.sqlite")
	f.BoolVar(&o.noT3, "no-t3code", false, "do not enable the T3 Code source")
	f.BoolVar(&o.skipCheck, "skip-connection-check", false, "do not test the ClickHouse connection")
	f.StringVar(&o.dataDir, "data-dir", "", "directory for the spool and state (default: platform data directory)")
	return cmd
}

type prompter struct {
	r           *bufio.Reader
	w           io.Writer
	interactive bool
	in          io.Reader
}

func (p *prompter) ask(label, def string) (string, error) {
	if !p.interactive {
		return def, nil
	}
	if def != "" {
		fmt.Fprintf(p.w, "%s [%s]: ", label, def)
	} else {
		fmt.Fprintf(p.w, "%s: ", label)
	}
	line, err := p.r.ReadString('\n')
	if err != nil && !(errors.Is(err, io.EOF) && line != "") {
		return "", fmt.Errorf("read input: %w", err)
	}
	line = strings.TrimSpace(line)
	if line == "" {
		return def, nil
	}
	return line, nil
}

func (p *prompter) confirm(label string, def bool) (bool, error) {
	d := "y/N"
	if def {
		d = "Y/n"
	}
	ans, err := p.ask(label+" ("+d+")", "")
	if err != nil {
		return false, err
	}
	switch strings.ToLower(ans) {
	case "":
		return def, nil
	case "y", "yes":
		return true, nil
	case "n", "no":
		return false, nil
	}
	return false, fmt.Errorf("please answer yes or no")
}

func (p *prompter) secret(label string) (string, error) {
	if !p.interactive {
		return "", nil
	}
	fmt.Fprintf(p.w, "%s: ", label)
	if f, ok := p.in.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		// ReadPassword disables echo; restore it if the user presses Ctrl-C,
		// which would otherwise leave the terminal without echo.
		if st, err := term.GetState(int(f.Fd())); err == nil {
			sig := make(chan os.Signal, 1)
			signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
			done := make(chan struct{})
			defer func() { signal.Stop(sig); close(done) }()
			go func() {
				select {
				case <-sig:
					_ = term.Restore(int(f.Fd()), st)
					fmt.Fprintln(p.w)
					os.Exit(130)
				case <-done:
				}
			}()
		}
		b, err := term.ReadPassword(int(f.Fd()))
		fmt.Fprintln(p.w)
		return string(b), err
	}
	line, err := p.r.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

func (a *app) runInit(ctx context.Context, o initOptions) error {
	path, err := a.resolveConfigPath()
	if err != nil {
		return withCode(ExitUsage, err)
	}
	interactive := !o.nonInteractive
	if f, ok := a.stdin.(*os.File); ok && interactive && !term.IsTerminal(int(f.Fd())) {
		interactive = false
		fmt.Fprintln(a.stderr, "stdin is not a terminal; continuing non-interactively with flags and defaults")
	}
	reader := bufio.NewReader(a.stdin)
	p := &prompter{r: reader, w: a.stdout, interactive: interactive, in: a.stdin}

	if fileExists(path) && !o.force {
		if !interactive {
			return withCode(ExitUsage, fmt.Errorf("configuration already exists at %s (use --force to overwrite)", path))
		}
		ok, err := p.confirm("A configuration already exists at "+path+". Overwrite it?", false)
		if err != nil {
			return err
		}
		if !ok {
			fmt.Fprintln(a.stdout, "Keeping the existing configuration.")
			return nil
		}
	}

	cfg := config.Default()
	cfg.SetPath(path)
	fmt.Fprintf(a.stdout, "Vario setup — configuration will be written to %s\n\n", path)

	// Sources.
	if !o.noT3 {
		candidates := t3code.DefaultPaths()
		if o.t3Path != "" {
			if o.t3Path, err = config.ExpandPath(o.t3Path); err != nil {
				return withCode(ExitUsage, err)
			}
			candidates = []string{o.t3Path}
		}
		var found string
		for _, d := range t3code.Detect(ctx, candidates) {
			printDetection(a.stdout, d)
			if d.Compatible && found == "" {
				found = d.Path
			}
		}
		def := found
		if def == "" && len(candidates) > 0 {
			def = candidates[0]
		}
		enable := found != "" || o.t3Path != ""
		if interactive {
			if enable, err = p.confirm("Enable the T3 Code source?", found != ""); err != nil {
				return err
			}
		}
		if enable {
			sp, err := p.ask("T3 Code database path", def)
			if err != nil {
				return err
			}
			if sp, err = config.ExpandPath(sp); err != nil {
				return withCode(ExitUsage, err)
			}
			cfg.UpsertSource(config.SourceConfig{Kind: config.SourceT3Code, Enabled: true, Path: sp})
		}
		fmt.Fprintln(a.stdout)
	}

	// ClickHouse.
	def := func(v, d string) string {
		if v != "" {
			return v
		}
		return d
	}
	for {
		u, err := p.ask("ClickHouse URL (HTTP interface)", def(o.url, "http://localhost:8123"))
		if err != nil {
			return err
		}
		cfg.ClickHouse.URL = u
		pu, perr := url.Parse(u)
		if perr == nil && pu.Scheme == "http" && !config.IsLoopback(pu.Hostname()) {
			fmt.Fprintln(a.stdout, "  Warning: this URL sends data and credentials unencrypted over the network.")
			allow := false
			if interactive {
				if allow, err = p.confirm("  Use unencrypted HTTP anyway?", false); err != nil {
					return err
				}
			}
			if !allow {
				if !interactive {
					return withCode(ExitUsage, errors.New("refusing unencrypted http:// for a remote ClickHouse; use https://"))
				}
				continue
			}
			cfg.ClickHouse.AllowInsecureHTTP = true
		}
		break
	}
	if cfg.ClickHouse.Database, err = p.ask("ClickHouse database", def(o.database, cfg.ClickHouse.Database)); err != nil {
		return err
	}
	if cfg.ClickHouse.Username, err = p.ask("ClickHouse user", def(o.username, cfg.ClickHouse.Username)); err != nil {
		return err
	}
	var password string
	switch {
	case o.passwordStdin:
		b, err := io.ReadAll(io.LimitReader(reader, 4096))
		if err != nil {
			return err
		}
		password = strings.TrimRight(string(b), "\r\n")
	case os.Getenv(config.EnvPassword) != "":
		fmt.Fprintf(a.stdout, "Using the password from $%s (it will not be written to disk).\n", config.EnvPassword)
	default:
		if password, err = p.secret("ClickHouse password (leave empty for none)"); err != nil {
			return err
		}
	}
	fmt.Fprintln(a.stdout)

	// Collection mode.
	fmt.Fprintln(a.stdout, `Collection modes:
  metrics   token usage, models, timings, tool names and statuses (no conversation text)
  messages  metrics + user/assistant message text, conversation and project titles
  full      messages + tool inputs/outputs, reasoning, file diffs, web searches, raw events`)
	for {
		mode, err := p.ask("Collection mode", def(o.mode, config.ModeMetrics))
		if err != nil {
			return err
		}
		if mode != config.ModeMetrics && mode != config.ModeMessages && mode != config.ModeFull {
			if !interactive {
				return withCode(ExitUsage, fmt.Errorf("invalid mode %q", mode))
			}
			fmt.Fprintln(a.stdout, "  Choose metrics, messages or full.")
			continue
		}
		cfg.Privacy.Mode = mode
		break
	}
	if cfg.Privacy.Mode != config.ModeMetrics {
		fmt.Fprintf(a.stdout, `
  The %q mode copies conversation content to ClickHouse. Transcripts and command
  output can contain credentials, private files and proprietary source code.
  Known secret formats are redacted before data leaves this machine, but
  redaction cannot catch everything.
`, cfg.Privacy.Mode)
		ok := o.allowSensitive
		if interactive && !ok {
			ans, err := p.ask("  Type 'yes' to collect conversation content", "")
			if err != nil {
				return err
			}
			ok = strings.EqualFold(ans, "yes")
		}
		if !ok {
			if !interactive {
				return withCode(ExitUsage, fmt.Errorf("mode %q requires --allow-sensitive-content", cfg.Privacy.Mode))
			}
			fmt.Fprintln(a.stdout, "  Not confirmed; using the metrics mode.")
			cfg.Privacy.Mode = config.ModeMetrics
		}
		cfg.Privacy.AllowSensitiveContent = ok
	}

	if o.dataDir != "" {
		if cfg.Collector.DataDir, err = config.ExpandPath(o.dataDir); err != nil {
			return withCode(ExitUsage, err)
		}
	} else if cfg.Collector.DataDir, err = config.DefaultDataDir(); err != nil {
		return err
	}

	if err := cfg.Validate(); err != nil {
		return withCode(ExitUsage, fmt.Errorf("invalid settings:\n  %s", strings.ReplaceAll(err.Error(), "\n", "\n  ")))
	}
	if err := cfg.Save(); err != nil {
		return err
	}
	if password != "" {
		if err := cfg.WritePassword(password); err != nil {
			return err
		}
	}
	fmt.Fprintf(a.stdout, "\nWrote %s\n", path)
	if password != "" {
		fmt.Fprintf(a.stdout, "Wrote password to %s (mode 0600)\n", cfg.PasswordPath())
	}

	if !o.skipCheck {
		pw, _ := cfg.Password()
		client, err := clickhouse.New(cfg.ClickHouse, pw)
		if err == nil {
			cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			var ver string
			ver, err = client.ServerVersion(cctx)
			cancel()
			if err == nil {
				fmt.Fprintf(a.stdout, "Connected to ClickHouse %s\n", ver)
			}
		}
		if err != nil {
			fmt.Fprintf(a.stdout, "Warning: could not connect to ClickHouse: %v\n(the configuration was saved; data will be spooled locally until ClickHouse is reachable)\n", err)
		}
	}

	fmt.Fprint(a.stdout, `
Next steps:
  vario doctor                    validate configuration, source access and ClickHouse
  vario backfill --source t3code  import existing history
  vario run                       collect continuously in the foreground
  vario service install           run in the background at login (macOS)
`)
	return nil
}
