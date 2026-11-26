//go:build windows

package claudecode

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// openShared opens a file for reading while letting Claude Code keep
// writing, renaming and deleting it (os.Open does not share deletion).
func openShared(path string) (*os.File, error) {
	p, err := syscall.UTF16PtrFromString(extendedPath(path))
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	h, err := syscall.CreateFile(p, syscall.GENERIC_READ,
		syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE|syscall.FILE_SHARE_DELETE,
		nil, syscall.OPEN_EXISTING, syscall.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		if err == syscall.ERROR_FILE_NOT_FOUND || err == syscall.ERROR_PATH_NOT_FOUND {
			err = os.ErrNotExist
		}
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	return os.NewFile(uintptr(h), path), nil
}

// extendedPath returns path in the \\?\ form so CreateFile accepts paths
// longer than MAX_PATH. Paths already in an extended or device form, and
// paths that are neither drive nor UNC paths, are returned unchanged.
func extendedPath(path string) string {
	if strings.HasPrefix(path, `\\?\`) || strings.HasPrefix(path, `\??\`) ||
		strings.HasPrefix(path, `\\.\`) {
		return path
	}
	// The \\?\ form skips normalisation, so the path must be absolute,
	// cleaned and use backslashes.
	abs, err := filepath.Abs(path)
	if err != nil {
		return path
	}
	switch vol := filepath.VolumeName(abs); {
	case len(vol) == 2 && vol[1] == ':':
		return `\\?\` + abs
	case strings.HasPrefix(vol, `\\`):
		return `\\?\UNC\` + abs[2:]
	}
	return path
}
