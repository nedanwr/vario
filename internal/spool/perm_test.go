//go:build !windows

package spool

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/nedanwr/vario/internal/model"
)

// The spool holds conversation content in messages/full mode. The database and
// its WAL and shared-memory files must not be readable by other users, even
// when the data directory itself is not private.
func TestSpoolFilesArePrivate(t *testing.T) {
	old := syscall.Umask(0o022)
	defer syscall.Umask(old)
	dir := filepath.Join(t.TempDir(), "shared")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "state.db")
	s, err := Open(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.Commit(ctx, "src", []model.Record{msg("m1", "secret text", 1)}, ptr("c"), CommitOptions{}); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		st, err := os.Stat(p)
		if err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		if st.Mode().Perm()&0o077 != 0 {
			t.Errorf("%s has mode %o; want no group/other access", filepath.Base(p), st.Mode().Perm())
		}
	}
}
