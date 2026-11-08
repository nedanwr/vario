//go:build !windows

package spool

import (
	"errors"
	"syscall"
)

// processExists reports whether a process with pid exists (signal 0 probes
// without delivering anything). EPERM means it exists but belongs to someone else.
func processExists(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
