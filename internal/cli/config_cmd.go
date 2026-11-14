package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/spf13/cobra"

	"github.com/nedanwr/vario/internal/config"
)

func (a *app) configCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Show and change the configuration",
		Long: `Config reads and changes the configuration file without rerunning init.

Changes are validated before they are written; only the affected lines are
rewritten, so comments and other settings are preserved. The ClickHouse
password is never shown and is changed with 'vario config set-password'.

A running background service keeps its current settings until it is
restarted with 'vario service restart'.`,
	}
	cmd.AddCommand(a.configPathCmd(), a.configShowCmd(), a.configGetCmd(), a.configSetCmd(), a.configSetPasswordCmd(), a.configEditCmd())
	return cmd
}

func (a *app) configPathCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "path",
		Short: "Print the configuration file path",
		Long: `Path prints the configuration file in use. Precedence: --config, then
$VARIO_CONFIG, then $VARIO_HOME/config.toml, then ~/.vario/config.toml.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			loc, err := a.resolveLocation()
			if err != nil {
				return withCode(ExitUsage, err)
			}
			fmt.Fprintln(a.stdout, loc.Path)
			if !fileExists(loc.Path) {
				fmt.Fprintln(a.stderr, "(does not exist yet; run `vario init`)")
			}
			return nil
		},
	}
}

// loadForConfig loads the configuration without validating it, so that
// config commands work on (and help fix) invalid configurations.
func (a *app) loadForConfig() (*config.Config, error) {
	path, err := a.resolveConfigPath()
	if err != nil {
		return nil, withCode(ExitUsage, err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		return nil, withCode(ExitUsage, err)
	}
	return cfg, nil
}

func passwordStatus(cfg *config.Config) string {
	if _, ok := os.LookupEnv(config.EnvPassword); ok {
		return "set via $" + config.EnvPassword + " (not shown)"
	}
	if cfg.HasStoredPassword() {
		return "stored in " + cfg.PasswordPath() + " (not shown)"
	}
	return "not set"
}

func (a *app) configShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show",
		Short: "Show the effective configuration (secrets masked)",
		Long: `Show prints every setting with its effective value. Settings that are not in
the file and use their default are marked "# default". The ClickHouse
password is never shown.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := a.loadForConfig()
			if err != nil {
				return err
			}
			w := a.stdout
			fmt.Fprintf(w, "# %s\n", cfg.Path())
			section := ""
			for _, k := range config.Keys() {
				if k.Deprecated && !cfg.IsSet(k.Name) {
					continue
				}
				sec := k.Name[:strings.LastIndex(k.Name, ".")]
				if sec != section {
					section = sec
					fmt.Fprintf(w, "\n[%s]\n", sec)
				}
				line := k.Name[len(sec)+1:] + " = " + k.Literal(cfg)
				if !strings.HasPrefix(sec, "sources.") && !cfg.IsSet(k.Name) {
					line += "  # default"
				}
				fmt.Fprintln(w, line)
				if k.Deprecated {
					line += "  # deprecated, ignored"
				}
				if k.Name == "clickhouse.username" {
					fmt.Fprintf(w, "# password: %s\n", passwordStatus(cfg))
					fmt.Fprintln(w, "# administrative credentials: never stored (asked for by `vario init` and `vario migrate`)")
				}
			}
			if err := cfg.Validate(); err != nil {
				fmt.Fprintf(a.stderr, "\nWarning: the configuration is invalid:\n  %s\n", strings.ReplaceAll(err.Error(), "\n", "\n  "))
			}
			return nil
		},
	}
}

func unknownKey(name string) error {
	return withCode(ExitUsage, fmt.Errorf("unknown configuration key %q\nSupported keys:\n  %s", name, strings.Join(config.KeyNames(), "\n  ")))
}

func (a *app) configGetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "get <key>",
		Short: "Print one configuration value",
		Long: `Get prints the effective value of a key, e.g. 'vario config get clickhouse.url'.
For clickhouse.password only whether a password is set is shown.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := a.loadForConfig()
			if err != nil {
				return err
			}
			if args[0] == config.PasswordKey {
				fmt.Fprintln(a.stdout, passwordStatus(cfg))
				return nil
			}
			k, ok := config.LookupKey(args[0])
			if !ok {
				return unknownKey(args[0])
			}
			fmt.Fprintln(a.stdout, k.Value(cfg))
			return nil
		},
	}
}

func (a *app) configSetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "set <key> <value>",
		Short: "Change one configuration value",
		Long: `Set validates and stores one value, preserving every other setting and the
file's comments. Examples:

  vario config set clickhouse.url https://clickhouse.example.com:8443
  vario config set clickhouse.username vario
  vario config set clickhouse.database vario
  vario config set sources.t3code.path ~/.t3/userdata/statev2.sqlite

The password cannot be set here (shell arguments end up in history and
process lists); use 'vario config set-password'.

Changing clickhouse.url or clickhouse.database changes the destination: data
already queued is never sent to the new destination, and the new destination
receives a full backfill (see 'vario destination').`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			name, raw := args[0], args[1]
			if name == config.PasswordKey {
				return withCode(ExitUsage, errors.New("the password is not accepted as a command-line argument; use `vario config set-password`"))
			}
			k, ok := config.LookupKey(name)
			if !ok {
				return unknownKey(name)
			}
			cfg, err := a.loadForConfig()
			if err != nil {
				return err
			}
			before := k.Value(cfg)
			next := cfg.Clone()
			if err := k.Set(next, raw); err != nil {
				return withCode(ExitUsage, err)
			}
			changes := []config.Change{k.Change(next)}
			if name == "clickhouse.url" && config.InsecureRemote(next.ClickHouse.URL) && !next.ClickHouse.AllowInsecureHTTP {
				return withCode(ExitUsage, errors.New("clickhouse.url uses unencrypted http:// for a remote host; use https://, or first run `vario config set clickhouse.allow_insecure_http true` if you accept sending data and credentials unencrypted"))
			}
			if name == "privacy.mode" && next.Privacy.Mode != config.ModeMetrics && !next.Privacy.AllowSensitiveContent {
				return withCode(ExitUsage, fmt.Errorf("privacy.mode %q collects conversation content; first confirm with `vario config set privacy.allow_sensitive_content true`", next.Privacy.Mode))
			}
			updated, err := config.Update(cfg.Path(), cfg.Fingerprint(), changes)
			if err != nil {
				var ve *config.ValidationError
				if errors.As(err, &ve) {
					return withCode(ExitUsage, fmt.Errorf("%s was not changed: %w", name, err))
				}
				return err
			}
			after := k.Value(updated)
			if after == before {
				fmt.Fprintf(a.stdout, "%s is already %s\n", name, after)
				return nil
			}
			fmt.Fprintf(a.stdout, "Set %s = %s (was %s) in %s\n", name, after, orDash(before), config.DisplayPath(cfg.Path()))
			if k.Deprecated {
				fmt.Fprintf(a.stdout, "Note: %s is deprecated and has no effect (%s).\n", name, k.Help)
			}
			if strings.HasPrefix(name, "clickhouse.") {
				fmt.Fprintln(a.stdout, "Run `vario doctor` to check the connection with the new settings.")
			}
			destinationNotice(a.stdout, cfg, updated)
			a.serviceNotice(cfg.Path(), true)
			return nil
		},
	}
}

func (a *app) configSetPasswordCmd() *cobra.Command {
	var fromStdin, clear bool
	cmd := &cobra.Command{
		Use:   "set-password",
		Short: "Store or remove the ClickHouse password",
		Long: `Set-password prompts for the ClickHouse password without echoing it and stores
it in the password file (clickhouse.password_file, default clickhouse-password
next to the configuration), readable only by you. Use --stdin to read it from
standard input in scripts, or --clear to remove the stored password.
$VARIO_CLICKHOUSE_PASSWORD, when set, takes precedence over the file.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := a.loadForConfig()
			if err != nil {
				return err
			}
			if clear {
				if fromStdin {
					return withCode(ExitUsage, errors.New("--clear and --stdin cannot be combined"))
				}
				if err := cfg.ClearPassword(); err != nil {
					return err
				}
				fmt.Fprintf(a.stdout, "Removed the stored password (%s)\n", config.DisplayPath(cfg.PasswordPath()))
			} else {
				var pw string
				if fromStdin {
					b, err := io.ReadAll(io.LimitReader(a.stdin, 4096))
					if err != nil {
						return err
					}
					pw = strings.TrimRight(string(b), "\r\n")
				} else {
					if !isTerminal(a.stdin) {
						return withCode(ExitUsage, errors.New("stdin is not a terminal; use --stdin to read the password from standard input"))
					}
					p := newPrompter(a.stdin, a.stdout, true)
					if pw, err = p.secret("ClickHouse password (input hidden)"); err != nil {
						return err
					}
					again, err := p.secret("Repeat the password")
					if err != nil {
						return err
					}
					if again != pw {
						return withCode(ExitUsage, errors.New("the passwords do not match; nothing was changed"))
					}
				}
				if pw == "" {
					return withCode(ExitUsage, errors.New("empty password; use --clear to remove the stored password"))
				}
				if err := cfg.WritePassword(pw); err != nil {
					return err
				}
				fmt.Fprintf(a.stdout, "Password saved to %s (readable only by you)\n", config.DisplayPath(cfg.PasswordPath()))
			}
			if _, ok := os.LookupEnv(config.EnvPassword); ok {
				fmt.Fprintf(a.stdout, "Note: $%s is set and takes precedence over the password file.\n", config.EnvPassword)
			}
			a.serviceNotice(cfg.Path(), true)
			return nil
		},
	}
	cmd.Flags().BoolVar(&fromStdin, "stdin", false, "read the password from standard input")
	cmd.Flags().BoolVar(&clear, "clear", false, "remove the stored password")
	return cmd
}

func (a *app) configEditCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "edit",
		Short: "Open the configuration file in your editor",
		Long: `Edit opens a copy of the configuration in $VISUAL, $EDITOR or a default editor
(vi; notepad on Windows). When the editor exits, the copy is validated and
only then replaces the configuration. Invalid edits are never applied: you
can edit again, or the copy is kept so that your changes are not lost.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			path, err := a.resolveConfigPath()
			if err != nil {
				return withCode(ExitUsage, err)
			}
			orig, err := os.ReadFile(path)
			if errors.Is(err, os.ErrNotExist) {
				return withCode(ExitUsage, fmt.Errorf("no configuration at %s; run `vario init` first", path))
			}
			if err != nil {
				return err
			}
			editor, err := editorCommand()
			if err != nil {
				return withCode(ExitUsage, err)
			}
			tmp, err := os.CreateTemp(filepath.Dir(path), ".config-edit-*.toml")
			if err != nil {
				return err
			}
			tmpPath := tmp.Name()
			keep := false
			defer func() {
				if !keep {
					os.Remove(tmpPath)
				}
			}()
			if err := tmp.Chmod(0o600); err != nil && runtime.GOOS != "windows" {
				tmp.Close()
				return err
			}
			_, werr := tmp.Write(orig)
			if cerr := tmp.Close(); werr != nil || cerr != nil {
				return errors.Join(werr, cerr)
			}
			p := newPrompter(a.stdin, a.stdout, isTerminal(a.stdin))
			for {
				c := exec.Command(editor[0], append(editor[1:], tmpPath)...) //nolint:gosec // the user's own editor, without a shell
				c.Stdin, c.Stdout, c.Stderr = a.stdin, a.stdout, a.stderr
				if err := c.Run(); err != nil {
					keep = true
					return fmt.Errorf("editor %q failed: %v; the configuration was not changed (your copy: %s)", editor[0], err, tmpPath)
				}
				edited, err := os.ReadFile(tmpPath)
				if err != nil {
					return err
				}
				if string(edited) == string(orig) {
					fmt.Fprintln(a.stdout, "No changes.")
					return nil
				}
				updated, err := config.Replace(path, config.FingerprintBytes(orig), edited)
				if err == nil {
					fmt.Fprintf(a.stdout, "Saved %s\n", config.DisplayPath(path))
					if prev, perr := config.Parse(path, orig); perr == nil {
						destinationNotice(a.stdout, prev, updated)
					}
					a.serviceNotice(path, true)
					return nil
				}
				if errors.Is(err, config.ErrModified) {
					keep = true
					return fmt.Errorf("%w; your edited copy is at %s", err, tmpPath)
				}
				fmt.Fprintf(a.stderr, "The edited configuration is invalid and was not saved:\n  %s\n", strings.ReplaceAll(err.Error(), "\n", "\n  "))
				again, perr := p.confirm("Edit it again?", true)
				if perr != nil || !again || !p.interactive {
					keep = true
					return withCode(ExitUsage, fmt.Errorf("the configuration was not changed; your edited copy is at %s", tmpPath))
				}
			}
		},
	}
}

// editorCommand returns the user's editor as program and arguments. The
// value of $VISUAL or $EDITOR is split into words (honouring quotes) and run
// directly, never through a shell, so it cannot inject commands.
func editorCommand() ([]string, error) {
	for _, env := range []string{"VISUAL", "EDITOR"} {
		if v := strings.TrimSpace(os.Getenv(env)); v != "" {
			words, err := splitWords(v)
			if err != nil {
				return nil, fmt.Errorf("$%s: %w", env, err)
			}
			if len(words) > 0 {
				return words, nil
			}
		}
	}
	candidates := []string{"vi", "nano"}
	if runtime.GOOS == "windows" {
		candidates = []string{"notepad"}
	}
	for _, c := range candidates {
		if p, err := exec.LookPath(c); err == nil {
			return []string{p}, nil
		}
	}
	return nil, errors.New("no editor found; set $VISUAL or $EDITOR")
}

// splitWords splits s like a POSIX shell would split words, supporting single
// and double quotes and backslash escapes, without any expansion.
func splitWords(s string) ([]string, error) {
	var words []string
	var cur strings.Builder
	inWord := false
	var quote rune
	escaped := false
	for _, r := range s {
		switch {
		case escaped:
			cur.WriteRune(r)
			escaped = false
		case quote == '\'':
			if r == '\'' {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
		case quote == '"':
			switch r {
			case '"':
				quote = 0
			case '\\':
				escaped = true
			default:
				cur.WriteRune(r)
			}
		case r == '\\' && runtime.GOOS != "windows":
			escaped, inWord = true, true
		case r == '\'' || r == '"':
			quote, inWord = r, true
		case r == ' ' || r == '\t' || r == '\n':
			if inWord {
				words = append(words, cur.String())
				cur.Reset()
				inWord = false
			}
		default:
			cur.WriteRune(r)
			inWord = true
		}
	}
	if quote != 0 || escaped {
		return nil, errors.New("unterminated quote or escape")
	}
	if inWord {
		words = append(words, cur.String())
	}
	return words, nil
}
