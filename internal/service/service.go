// Package service registers the collector with the platform's user-level service manager.
package service

import (
	"errors"
	"runtime"

	"github.com/nedanwr/vario/internal/service/launchd"
)

const Label = "io.github.nedanwr.vario"

// ErrUnsupported is returned on platforms without an implemented service manager.
var ErrUnsupported = errors.New("background service management is only implemented for macOS (launchd) in this release")

var ErrNotInstalled = launchd.ErrNotInstalled

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

func (m launchdManager) Definition() string { return m.a.PlistPath() }

func (m launchdManager) Status() (Status, error) {
	st, err := m.a.Status()
	return Status{Installed: st.Installed, Running: st.Running, PID: st.PID, LastExit: st.LastExit, Definition: st.PlistPath}, err
}
