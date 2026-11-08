package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/spf13/cobra"

	"github.com/nedanwr/vario/internal/config"
	"github.com/nedanwr/vario/internal/service"
)

func (a *app) serviceCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "service",
		Short: "Manage the background collector service (macOS launchd)",
		Long: `Service registers 'vario run' as a per-user background service that starts
at login, restarts after abnormal exits and keeps running without a terminal.

Implemented for macOS (per-user LaunchAgent). Linux and Windows are not yet
supported; run 'vario run' under your own supervisor there.`,
	}
	cmd.AddCommand(a.serviceInstallCmd(), a.serviceUninstallCmd(), a.serviceRestartCmd(), a.serviceStatusCmd())
	return cmd
}

// executable returns the running binary's path; tests replace it.
var executable = os.Executable

func logDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	if runtime.GOOS == "darwin" {
		return filepath.Join(home, "Library", "Logs", "Vario"), nil
	}
	return "", service.ErrUnsupported
}

func (a *app) serviceInstallCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "install",
		Short: "Install and start the background service for the current user",
		Long: `Install writes a LaunchAgent definition to ~/Library/LaunchAgents and loads
it immediately. The service runs this vario executable with the resolved
configuration file (see 'vario config path'); no credentials are written to
the definition. Configuration changes (endpoint, database, mode, ...) only
need 'vario service restart'. Re-run install after moving the executable or
switching to a different configuration file.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			mgr, err := serviceManager()
			if err != nil {
				return err
			}
			cfg, err := a.loadConfig()
			if err != nil {
				return err
			}
			if len(cfg.Sources) == 0 {
				return withCode(ExitUsage, errors.New("no sources configured; run `vario sources add t3code` first"))
			}
			exe, err := executable()
			if err != nil {
				return err
			}
			if exe, err = filepath.EvalSymlinks(exe); err != nil {
				return err
			}
			if strings.Contains(exe, string(filepath.Separator)+"go-build") {
				return withCode(ExitUsage, errors.New("refusing to install a temporary `go run` binary; build vario and run the installed executable"))
			}
			logs, err := logDir()
			if err != nil {
				return err
			}
			spec := service.Spec{
				Program:    exe,
				Args:       []string{"run", "--config", cfg.Path(), "--log-file", filepath.Join(logs, "vario.log")},
				StdoutPath: filepath.Join(logs, "vario.stdout.log"),
				StderrPath: filepath.Join(logs, "vario.stderr.log"),
			}
			if err := mgr.Install(spec); err != nil {
				return err
			}
			fmt.Fprintf(a.stdout, "Installed %s\n", mgr.Definition())
			fmt.Fprintf(a.stdout, "Executable: %s\nLogs: %s\n", exe, filepath.Join(logs, "vario.log"))
			if st, err := mgr.Status(); err == nil && st.Running {
				fmt.Fprintf(a.stdout, "Service is running (pid %d). Check progress with `vario status`.\n", st.PID)
			} else {
				fmt.Fprintln(a.stdout, "Service loaded. Check progress with `vario status`.")
			}
			return nil
		},
	}
}

func (a *app) serviceUninstallCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "uninstall",
		Short: "Stop and remove the background service",
		Long: `Uninstall stops the service (it shuts down gracefully, keeping spooled data
and cursors) and removes its definition. Configuration and local state are kept.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			mgr, err := serviceManager()
			if err != nil {
				return err
			}
			if err := mgr.Uninstall(); err != nil {
				if errors.Is(err, service.ErrNotInstalled) {
					fmt.Fprintln(a.stdout, "Service is not installed.")
					return nil
				}
				return err
			}
			fmt.Fprintf(a.stdout, "Removed %s\n", mgr.Definition())
			return nil
		},
	}
}

func (a *app) serviceRestartCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "restart",
		Short: "Restart the background service to apply configuration changes",
		Long: `Restart stops the running collector gracefully (spooled data and cursors are
kept) and starts it again, so that it reads the current configuration. The
service definition is not changed. The configuration is validated first so
that a broken configuration does not stop a working collector.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			mgr, err := serviceManager()
			if err != nil {
				return err
			}
			st, err := mgr.Status()
			if err != nil {
				return err
			}
			if !st.Installed {
				return withCode(ExitUsage, errors.New("the service is not installed; run `vario service install`"))
			}
			path, err := a.resolveConfigPath()
			if err != nil {
				return withCode(ExitUsage, err)
			}
			if args, err := mgr.Args(); err == nil {
				if p := service.ConfigPath(args); p != "" {
					path = p
				}
			}
			cfg, err := config.Load(path)
			if err == nil {
				err = cfg.Validate()
			}
			if err != nil {
				return withCode(ExitUsage, fmt.Errorf("not restarting: the configuration used by the service (%s) is invalid:\n  %s", path, strings.ReplaceAll(err.Error(), "\n", "\n  ")))
			}
			if err := mgr.Restart(); err != nil {
				if errors.Is(err, service.ErrNotLoaded) {
					return fmt.Errorf("the service is installed but not loaded; run `vario service install` to load it")
				}
				return err
			}
			fmt.Fprintf(a.stdout, "Restarted the background service with %s\n", config.DisplayPath(path))
			return nil
		},
	}
}

func (a *app) serviceStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show whether the background service is installed and running",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			mgr, err := serviceManager()
			if err != nil {
				return err
			}
			st, err := mgr.Status()
			if err != nil {
				return err
			}
			fmt.Fprintf(a.stdout, "Definition  %s (installed: %t)\n", mgr.Definition(), st.Installed)
			if args, err := mgr.Args(); err == nil {
				if p := service.ConfigPath(args); p != "" {
					fmt.Fprintf(a.stdout, "Config      %s\n", p)
					if cur, err := a.resolveConfigPath(); err == nil && cur != p {
						fmt.Fprintf(a.stdout, "            ! differs from the current configuration %s; run `vario service install` to switch\n", cur)
					}
				}
			}
			switch {
			case st.Running:
				fmt.Fprintf(a.stdout, "State       running (pid %d)\n", st.PID)
			case st.Installed:
				fmt.Fprintf(a.stdout, "State       not running (last exit: %s)\n", orDash(st.LastExit))
			default:
				fmt.Fprintln(a.stdout, "State       not installed")
			}
			return nil
		},
	}
}
