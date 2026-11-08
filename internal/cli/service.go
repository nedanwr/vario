package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/spf13/cobra"

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
	cmd.AddCommand(a.serviceInstallCmd(), a.serviceUninstallCmd(), a.serviceStatusCmd())
	return cmd
}

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
it immediately. The service runs this vario executable with the current
configuration file. Re-run install after moving the executable or config.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			mgr, err := service.Default()
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
			exe, err := os.Executable()
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
			mgr, err := service.Default()
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

func (a *app) serviceStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show whether the background service is installed and running",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			mgr, err := service.Default()
			if err != nil {
				return err
			}
			st, err := mgr.Status()
			if err != nil {
				return err
			}
			fmt.Fprintf(a.stdout, "Definition  %s (installed: %t)\n", mgr.Definition(), st.Installed)
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
