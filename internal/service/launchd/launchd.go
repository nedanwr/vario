// Package launchd manages Vario as a per-user macOS LaunchAgent.
package launchd

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Spec describes the agent to install.
type Spec struct {
	Label      string
	Program    string
	Args       []string
	StdoutPath string
	StderrPath string
}

type Agent struct {
	Label string
	Dir   string
	uid   int
	exec  func(args ...string) (string, error)
}

// New returns an agent manager for the current user's LaunchAgents directory.
func New(label string) (*Agent, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	return &Agent{
		Label: label,
		Dir:   filepath.Join(home, "Library", "LaunchAgents"),
		uid:   os.Getuid(),
		exec:  launchctl,
	}, nil
}

func launchctl(args ...string) (string, error) {
	out, err := exec.Command("/bin/launchctl", args...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func (a *Agent) PlistPath() string { return filepath.Join(a.Dir, a.Label+".plist") }

func (a *Agent) domain() string  { return "gui/" + strconv.Itoa(a.uid) }
func (a *Agent) service() string { return a.domain() + "/" + a.Label }

// Plist renders the LaunchAgent definition. The agent starts at login, is
// restarted after abnormal exits (throttled), and runs at background priority.
// No credentials are written into the plist.
func Plist(s Spec) ([]byte, error) {
	if s.Label == "" || s.Program == "" {
		return nil, errors.New("launchd: label and program are required")
	}
	var b bytes.Buffer
	esc := func(v string) string {
		var e bytes.Buffer
		_ = xml.EscapeText(&e, []byte(v))
		return e.String()
	}
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>` + esc(s.Label) + `</string>
  <key>ProgramArguments</key>
  <array>
`)
	for _, arg := range append([]string{s.Program}, s.Args...) {
		b.WriteString("    <string>" + esc(arg) + "</string>\n")
	}
	b.WriteString(`  </array>
  <key>RunAtLoad</key>
  <true/>
  <key>KeepAlive</key>
  <dict>
    <key>SuccessfulExit</key>
    <false/>
  </dict>
  <key>ThrottleInterval</key>
  <integer>30</integer>
  <key>ExitTimeOut</key>
  <integer>30</integer>
  <key>ProcessType</key>
  <string>Background</string>
  <key>LowPriorityIO</key>
  <true/>
  <key>Nice</key>
  <integer>5</integer>
`)
	if s.StdoutPath != "" {
		b.WriteString("  <key>StandardOutPath</key>\n  <string>" + esc(s.StdoutPath) + "</string>\n")
	}
	if s.StderrPath != "" {
		b.WriteString("  <key>StandardErrorPath</key>\n  <string>" + esc(s.StderrPath) + "</string>\n")
	}
	b.WriteString("</dict>\n</plist>\n")
	return b.Bytes(), nil
}

// Install writes the plist and (re)loads the agent into the user's GUI domain.
func (a *Agent) Install(s Spec) error {
	s.Label = a.Label
	data, err := Plist(s)
	if err != nil {
		return err
	}
	for _, p := range []string{s.StdoutPath, s.StderrPath} {
		if p != "" {
			if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
				return err
			}
		}
	}
	if err := os.MkdirAll(a.Dir, 0o755); err != nil {
		return err
	}
	if a.loaded() {
		if out, err := a.exec("bootout", a.service()); err != nil && a.loaded() {
			return fmt.Errorf("unload existing agent: %v: %s", err, out)
		}
		a.waitUnloaded()
	}
	if err := os.WriteFile(a.PlistPath(), data, 0o644); err != nil {
		return err
	}
	var out string
	for attempt := 0; attempt < 5; attempt++ {
		if out, err = a.exec("bootstrap", a.domain(), a.PlistPath()); err == nil {
			break
		}
		time.Sleep(time.Second)
	}
	if err != nil {
		return fmt.Errorf("launchctl bootstrap: %v: %s", err, out)
	}
	if out, err := a.exec("enable", a.service()); err != nil {
		return fmt.Errorf("launchctl enable: %v: %s", err, out)
	}
	return nil
}

// ErrNotInstalled is returned by Uninstall when there is nothing to remove.
var ErrNotInstalled = errors.New("service is not installed")

// Uninstall stops the agent (launchd sends SIGTERM) and removes its plist.
func (a *Agent) Uninstall() error {
	_, statErr := os.Stat(a.PlistPath())
	wasLoaded := a.loaded()
	if errors.Is(statErr, os.ErrNotExist) && !wasLoaded {
		return ErrNotInstalled
	}
	if wasLoaded {
		if out, err := a.exec("bootout", a.service()); err != nil && a.loaded() {
			return fmt.Errorf("launchctl bootout: %v: %s", err, out)
		}
		a.waitUnloaded()
	}
	if err := os.Remove(a.PlistPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// ErrNotLoaded is returned by Restart when the agent is not loaded.
var ErrNotLoaded = errors.New("service is not loaded")

// Restart stops the running agent (gracefully, with SIGTERM) and starts it
// again so that it reads the current configuration. The definition is not
// changed.
func (a *Agent) Restart() error {
	if !a.loaded() {
		return ErrNotLoaded
	}
	if out, err := a.exec("kickstart", "-k", a.service()); err != nil {
		return fmt.Errorf("launchctl kickstart: %v: %s", err, out)
	}
	return nil
}

// ProgramArguments reads the program and arguments from the installed plist.
func (a *Agent) ProgramArguments() ([]string, error) {
	f, err := os.Open(a.PlistPath())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrNotInstalled
		}
		return nil, err
	}
	defer f.Close()
	return parseProgramArguments(f)
}

// parseProgramArguments extracts the ProgramArguments array from a plist.
func parseProgramArguments(r io.Reader) ([]string, error) {
	dec := xml.NewDecoder(r)
	dec.Strict = false
	var (
		lastKey string
		inArray bool
		out     []string
		text    strings.Builder
		depth   int
	)
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("parse plist: %w", err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			depth++
			text.Reset()
			if t.Name.Local == "array" && lastKey == "ProgramArguments" && depth == 3 {
				inArray = true
			}
		case xml.CharData:
			text.Write(t)
		case xml.EndElement:
			depth--
			switch {
			case t.Name.Local == "key" && depth == 2:
				lastKey = text.String()
			case t.Name.Local == "string" && inArray:
				out = append(out, text.String())
			case t.Name.Local == "array" && inArray:
				return out, nil
			}
		}
	}
	return nil, errors.New("plist has no ProgramArguments")
}

type Status struct {
	Installed bool
	Loaded    bool
	Running   bool
	PID       int
	LastExit  string
	PlistPath string
}

var (
	stateRe = regexp.MustCompile(`(?m)^\s*state = (\S+)`)
	pidRe   = regexp.MustCompile(`(?m)^\s*pid = (\d+)`)
	exitRe  = regexp.MustCompile(`(?m)^\s*last exit code = (.+)$`)
)

func (a *Agent) Status() (Status, error) {
	st := Status{PlistPath: a.PlistPath()}
	if _, err := os.Stat(a.PlistPath()); err == nil {
		st.Installed = true
	}
	out, err := a.exec("print", a.service())
	if err != nil {
		return st, nil
	}
	st.Loaded = true
	if m := stateRe.FindStringSubmatch(out); m != nil {
		st.Running = m[1] == "running"
	}
	if m := pidRe.FindStringSubmatch(out); m != nil {
		st.PID, _ = strconv.Atoi(m[1])
	}
	if m := exitRe.FindStringSubmatch(out); m != nil {
		st.LastExit = strings.TrimSpace(m[1])
	}
	return st, nil
}

func (a *Agent) loaded() bool {
	_, err := a.exec("print", a.service())
	return err == nil
}

func (a *Agent) waitUnloaded() {
	for i := 0; i < 50 && a.loaded(); i++ {
		time.Sleep(200 * time.Millisecond)
	}
}
