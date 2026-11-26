package claudecode

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/nedanwr/vario/internal/config"
)

func TestMain(m *testing.M) {
	// Keep the stored machine identifier out of the real Vario home.
	home, err := os.MkdirTemp("", "vario-claudecode-home")
	if err != nil {
		panic(err)
	}
	os.Setenv(config.EnvHome, home)
	code := m.Run()
	os.RemoveAll(home)
	os.Exit(code)
}

func TestLoadMachineIDIsStoredAndReused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "home", machineIDFile)
	if got := loadMachineID(path, func() string { return "first" }); got != "first" {
		t.Fatalf("first resolution: %q", got)
	}
	// Later lookups that differ or fail must not change the identifier.
	for _, read := range []func() string{
		func() string { return "second" },
		func() string { return "" },
	} {
		if got := loadMachineID(path, read); got != "first" {
			t.Fatalf("reused identifier: %q", got)
		}
	}
	if st, err := os.Stat(path); err != nil {
		t.Fatal(err)
	} else if st.Mode().Perm()&0o077 != 0 && os.PathSeparator == '/' {
		t.Fatalf("machine-id mode %v", st.Mode().Perm())
	}
}

func TestLoadMachineIDFallsBackToHostName(t *testing.T) {
	host, err := os.Hostname()
	if err != nil || host == "" {
		t.Skip("no host name")
	}
	path := filepath.Join(t.TempDir(), machineIDFile)
	if got := loadMachineID(path, func() string { return "" }); got != host {
		t.Fatalf("fallback: %q, want %q", got, host)
	}
	if got := loadMachineID(path, func() string { return "later" }); got != host {
		t.Fatalf("fallback not kept: %q", got)
	}
}

func TestLoadMachineIDKeepsConcurrentlyStoredID(t *testing.T) {
	path := filepath.Join(t.TempDir(), machineIDFile)
	if got := loadMachineID(path, func() string {
		// Another process stores its identifier while this one resolves.
		if err := os.WriteFile(path, []byte("other\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		return "mine"
	}); got != "other" {
		t.Fatalf("got %q, want the stored identifier", got)
	}
}

func TestLoadMachineIDWithoutHome(t *testing.T) {
	if got := loadMachineID("", func() string { return "id" }); got != "id" {
		t.Fatalf("got %q", got)
	}
}
