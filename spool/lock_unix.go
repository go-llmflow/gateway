//go:build unix

package spool

import (
	"os"
	"syscall"
)

// lock takes an exclusive advisory lock on f: blocking for the process's
// own file, non-blocking (an error when held) for an orphan candidate. The
// kernel releases it when the process dies, however it dies.
func lock(f *os.File, wait bool) error {
	how := syscall.LOCK_EX
	if !wait {
		how |= syscall.LOCK_NB
	}
	return syscall.Flock(int(f.Fd()), how)
}
