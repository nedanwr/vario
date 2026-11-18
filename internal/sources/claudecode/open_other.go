//go:build !windows

package claudecode

import "os"

// openShared opens a file for reading. On Unix, reading never blocks the
// writer, renames or deletion.
func openShared(path string) (*os.File, error) { return os.Open(path) }
