package cli

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nedanwr/vario/internal/config"
	"github.com/nedanwr/vario/internal/sources/t3code/t3codetest"
)

// runAsService runs vario in a separate process the way the LaunchAgent
// does: with --config and without $VARIO_HOME, $VARIO_CONFIG or the password
// variable of this shell.
func runAsService(t *testing.T, cfgPath string, args ...string) result {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if k != config.EnvHome && k != config.EnvConfig && k != config.EnvPassword {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	cmd.Env = append(cmd.Env, "VARIO_TEST_HELPER=1", "VARIO_TEST_ARGS="+strings.Join(append([]string{"--config", cfgPath}, args...), "\x1f"))
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return result{code, out.String(), errb.String()}
}

// F2: with $VARIO_HOME, the CLI and the background service must use the same
// spool, cursors and lease, whichever way the configuration was created.
func TestServiceAndCLIShareStateDirectory(t *testing.T) {
	home := isolateHome(t)
	vh := filepath.Join(home, "isolated")
	t.Setenv(config.EnvHome, vh)
	fix := t3codetest.New(t)
	fix.Standard()
	cfgPath := filepath.Join(vh, "config.toml")
	want := filepath.Join(vh, "data")

	for name, init := range map[string]func() result{
		"interactive": func() result {
			return vario(t, answers(unreachable, "2", "vario", "default", "", "n", "", "", "", ""), "init", "--t3code-path", fix.Path)
		},
		"non-interactive": func() result {
			return vario(t, "", "init", "--non-interactive", "--force", "--clickhouse-url", unreachable, "--skip-connection-check", "--t3code-path", fix.Path)
		},
	} {
		os.Remove(cfgPath)
		if r := init(); r.code != 0 {
			t.Fatalf("%s init: %+v", name, r)
		}
		if !strings.Contains(readFile(t, cfgPath), `data_dir = "`+want+`"`) {
			t.Fatalf("%s init must write the data directory explicitly:\n%s", name, readFile(t, cfgPath))
		}
		if r := runAsService(t, cfgPath, "config", "get", "collector.data_dir"); strings.TrimSpace(r.stdout) != want {
			t.Fatalf("%s: the service would use %q, the CLI %q", name, r.stdout, want)
		}
	}
	// State written by the CLI is the state the service sees.
	if r := vario(t, "", "backfill", "--source", "t3code", "--no-deliver"); r.code != 0 {
		t.Fatalf("backfill: %+v", r)
	}
	// Both report the same state file, cursor and queue (the "Service" line
	// differs only because the test replaces the service manager).
	dropService := func(s string) string {
		var out []string
		for _, l := range strings.Split(s, "\n") {
			if !strings.HasPrefix(l, "Service ") {
				out = append(out, l)
			}
		}
		return strings.Join(out, "\n")
	}
	cli := dropService(vario(t, "", "status").stdout)
	svc := dropService(runAsService(t, cfgPath, "status").stdout)
	if cli != svc || !strings.Contains(svc, filepath.Join(want, "state.db")) || !strings.Contains(svc, "messages=2") {
		t.Fatalf("the service must see the CLI's spool and cursors:\n%s\n---\n%s", cli, svc)
	}
}

// A configuration that leaves data_dir empty (hand-written, or from an
// earlier version) is refused by `service install` when the service would
// resolve a different directory, and reported by doctor.
func TestServiceInstallRefusesAmbiguousStateDirectory(t *testing.T) {
	home := isolateHome(t)
	vh := filepath.Join(home, "isolated")
	t.Setenv(config.EnvHome, vh)
	fix := t3codetest.New(t)
	cfgPath := filepath.Join(vh, "config.toml")
	if r := vario(t, "", "init", "--non-interactive", "--clickhouse-url", unreachable, "--skip-connection-check", "--t3code-path", fix.Path); r.code != 0 {
		t.Fatalf("init: %+v", r)
	}
	text := readFile(t, cfgPath)
	i := strings.Index(text, "data_dir = ")
	j := i + strings.IndexByte(text[i:], '\n')
	if err := os.WriteFile(cfgPath, []byte(text[:i]+`data_dir = ""`+text[j:]), 0o600); err != nil {
		t.Fatal(err)
	}
	svc := &fakeService{}
	useFakeService(t, svc)
	r := vario(t, "", "service", "install")
	if r.code != ExitUsage || svc.installed || !strings.Contains(r.stderr, "vario config set collector.data_dir "+filepath.Join(vh, "data")) {
		t.Fatalf("install must be refused: %+v", r)
	}
	// After setting it explicitly, both resolve the same directory.
	if r := vario(t, "", "config", "set", "collector.data_dir", filepath.Join(vh, "data")); r.code != 0 {
		t.Fatalf("config set: %+v", r)
	}
	if r := runAsService(t, cfgPath, "config", "get", "collector.data_dir"); strings.TrimSpace(r.stdout) != filepath.Join(vh, "data") {
		t.Fatalf("service data dir: %+v", r)
	}
	// doctor reports an installed service whose state directory would differ.
	if err := os.WriteFile(cfgPath, []byte(text[:i]+`data_dir = ""`+text[j:]), 0o600); err != nil {
		t.Fatal(err)
	}
	useFakeService(t, &fakeService{installed: true, args: []string{"run", "--config", cfgPath}})
	if r := vario(t, "", "doctor"); !strings.Contains(r.stdout, "collector.data_dir is empty: the background service") {
		t.Fatalf("doctor: %s", r.stdout)
	}
}
