// Package service registers the collector with the platform's user-level service manager.
package service

import (
	"errors"
	"runtime"
	"strings"

	"github.com/nedanwr/vario/internal/service/launchd"
)

const Label = "io.github.nedanwr.vario"

// ErrUnsupported is returned on platforms without an implemented service manager.
var ErrUnsupported = errors.New("background service management is only implemented for macOS (launchd) in this release")

var ErrNotInstalled = launchd.ErrNotInstalled

// ErrNotLoaded is returned by Restart when the service is not running under the service manager.
var ErrNotLoaded = launchd.ErrNotLoaded

type Spec struct {
	Program    string
	Args       []string
	StdoutPath string
	StderrPath string
}

type Status struct {
	Installed  bool
	Running    bool
	PID        int
	LastExit   string
	Definition string
}

type Manager interface {
	Install(Spec) error
	Uninstall() error
	Status() (Status, error)
	// Restart stops and starts the service so that it reads the current configuration.
	Restart() error
	// Args returns the arguments of the installed service (after the program).
	Args() ([]string, error)
	// Definition is the path of the service definition file.
	Definition() string
}

// Default returns the service manager for the current platform.
func Default() (Manager, error) {
	if runtime.GOOS != "darwin" {
		return nil, ErrUnsupported
	}
	a, err := launchd.New(Label)
	if err != nil {
		return nil, err
	}
	return launchdManager{a}, nil
}

type launchdManager struct{ a *launchd.Agent }

func (m launchdManager) Install(s Spec) error {
	return m.a.Install(launchd.Spec{Program: s.Program, Args: s.Args, StdoutPath: s.StdoutPath, StderrPath: s.StderrPath})
}

func (m launchdManager) Uninstall() error { return m.a.Uninstall() }

func (m launchdManager) Restart() error { return m.a.Restart() }

func (m launchdManager) Args() ([]string, error) {
	args, err := m.a.ProgramArguments()
	if err != nil || len(args) == 0 {
		return nil, err
	}
	return args[1:], nil
}

// ConfigPath returns the value of --config in service arguments, if any.
func ConfigPath(args []string) string {
	for i, a := range args {
		if a == "--config" && i+1 < len(args) {
			return args[i+1]
		}
		if v, ok := strings.CutPrefix(a, "--config="); ok {
			return v
		}
	}
	return ""
}

func (m launchdManager) Definition() string { return m.a.PlistPath() }

func (m launchdManager) Status() (Status, error) {
	st, err := m.a.Status()
	return Status{Installed: st.Installed, Running: st.Running, PID: st.PID, LastExit: st.LastExit, Definition: st.PlistPath}, err
}
