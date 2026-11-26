//go:build windows

package claudecode

import (
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
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

// Transcript paths can exceed MAX_PATH under deep project directories.
func TestOpenSharedLongPath(t *testing.T) {
	path := t.TempDir()
	for len(path) <= 300 {
		path = filepath.Join(path, strings.Repeat("d", 50))
	}
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatal(err)
	}
	path = filepath.Join(path, "s.jsonl")
	if err := os.WriteFile(path, []byte("{\"a\":1}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := openShared(path)
	if err != nil {
		t.Fatalf("open %d-char path: %v", len(path), err)
	}
	defer f.Close()
	b, err := io.ReadAll(f)
	if err != nil || string(b) != "{\"a\":1}\n" {
		t.Fatalf("read: %q, %v", b, err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatalf("delete while open: %v", err)
	}
}

func TestExtendedPath(t *testing.T) {
	for in, want := range map[string]string{
		`C:\a\b.jsonl`:              `\\?\C:\a\b.jsonl`,
		`C:/a/./c/../b.jsonl`:       `\\?\C:\a\b.jsonl`,
		`\\srv\share\b.jsonl`:       `\\?\UNC\srv\share\b.jsonl`,
		`\\?\C:\a\b.jsonl`:          `\\?\C:\a\b.jsonl`,
		`\\?\UNC\srv\share\b.jsonl`: `\\?\UNC\srv\share\b.jsonl`,
		`\\.\pipe\x`:                `\\.\pipe\x`,
	} {
		if got := extendedPath(in); got != want {
			t.Errorf("extendedPath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestMachineIDFromRegistry(t *testing.T) {
	id := readMachineID()
	if !regexp.MustCompile(`^[0-9a-fA-F-]{36}$`).MatchString(id) {
		t.Fatalf("MachineGuid not read: %q", id)
	}
}
