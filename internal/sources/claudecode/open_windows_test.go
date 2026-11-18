//go:build windows

package claudecode

import (
	"io"
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// While Vario holds a transcript open, Claude Code can still append to,
// rename and delete it.
func TestOpenSharedAllowsWriteRenameDelete(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s.jsonl")
	if err := os.WriteFile(path, []byte("{\"a\":1}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := openShared(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	w, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("append while open: %v", err)
	}
	if _, err := w.WriteString("{\"b\":2}\n"); err != nil {
		t.Fatal(err)
	}
	w.Close()
	b, err := io.ReadAll(f)
	if err != nil || string(b) != "{\"a\":1}\n{\"b\":2}\n" {
		t.Fatalf("read while shared: %q, %v", b, err)
	}

	moved := filepath.Join(dir, "moved.jsonl")
	if err := os.Rename(path, moved); err != nil {
		t.Fatalf("rename while open: %v", err)
	}
	if err := os.Remove(moved); err != nil {
		t.Fatalf("delete while open: %v", err)
	}
	if _, err := openShared(path); !os.IsNotExist(err) {
		t.Fatalf("missing file: %v", err)
	}
}

func TestMachineIDFromRegistry(t *testing.T) {
	id := readMachineID()
	if !regexp.MustCompile(`^[0-9a-fA-F-]{36}$`).MatchString(id) {
		t.Fatalf("MachineGuid not read: %q", id)
	}
}
