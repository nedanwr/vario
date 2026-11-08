//go:build windows

package spool

// processExists is conservative on Windows: the lease is respected until it expires.
func processExists(int) bool { return true }
