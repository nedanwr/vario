package launchd

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func spec() Spec {
	return Spec{Label: "io.example.vario", Program: "/opt/vario & co/vario", Args: []string{"run", "--config", "/Users/me/Library/Application Support/vario/config.toml"},
		StdoutPath: "/tmp/out.log", StderrPath: "/tmp/err.log"}
}

func TestPlist(t *testing.T) {
	b, err := Plist(spec())
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	for _, want := range []string{
		"<string>io.example.vario</string>",
		"<string>/opt/vario &amp; co/vario</string>",
		"<string>run</string>",
		"<key>RunAtLoad</key>\n  <true/>",
		"<key>SuccessfulExit</key>\n    <false/>",
		"<string>Background</string>",
		"<key>StandardErrorPath</key>\n  <string>/tmp/err.log</string>",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("plist missing %q", want)
		}
	}
	if strings.Contains(strings.ToLower(s), "password") {
		t.Error("plist must not contain credentials")
	}
	if runtime.GOOS == "darwin" {
		p := filepath.Join(t.TempDir(), "x.plist")
		os.WriteFile(p, b, 0o644)
		if out, err := exec.Command("plutil", "-lint", p).CombinedOutput(); err != nil {
			t.Fatalf("plutil: %v %s", err, out)
		}
	}
	if _, err := Plist(Spec{}); err == nil {
		t.Fatal("empty spec should fail")
	}
}

type fakeLaunchctl struct {
	loaded bool
	calls  []string
}

func (f *fakeLaunchctl) run(args ...string) (string, error) {
	f.calls = append(f.calls, args[0])
	switch args[0] {
	case "print":
		if f.loaded {
			return "state = running\n\tpid = 4242\n\tlast exit code = 0", nil
		}
		return "Could not find service", errors.New("exit status 113")
	case "bootstrap":
		f.loaded = true
	case "bootout":
		f.loaded = false
	}
	return "", nil
}

func TestInstallStatusUninstall(t *testing.T) {
	fake := &fakeLaunchctl{}
	a := &Agent{Label: "io.example.vario", Dir: filepath.Join(t.TempDir(), "LaunchAgents"), uid: 501, exec: fake.run}
	s := spec()
	s.StdoutPath = filepath.Join(t.TempDir(), "logs", "out.log")
	s.StderrPath = s.StdoutPath
	if err := a.Install(s); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(a.PlistPath()); err != nil {
		t.Fatal("plist not written")
	}
	st, _ := a.Status()
	if !st.Installed || !st.Running || st.PID != 4242 {
		t.Fatalf("status: %+v", st)
	}
	// Reinstalling unloads the existing agent first.
	fake.calls = nil
	if err := a.Install(s); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(fake.calls, ","), "bootout") {
		t.Fatalf("reinstall should boot out the old agent: %v", fake.calls)
	}
	if err := a.Uninstall(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(a.PlistPath()); !os.IsNotExist(err) {
		t.Fatal("plist not removed")
	}
	if err := a.Uninstall(); !errors.Is(err, ErrNotInstalled) {
		t.Fatalf("second uninstall: %v", err)
	}
}
