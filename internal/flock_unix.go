//go:build unix

package internal

import (
	"os"
	"syscall"
)

// lockFile takes an advisory lock on f, shared for readers, exclusive for writers. An agent
// that takes the same lock never reads a half-written sidecar or overwrites a save.
func lockFile(f *os.File, exclusive bool) error {
	how := syscall.LOCK_SH
	if exclusive {
		how = syscall.LOCK_EX
	}
	return syscall.Flock(int(f.Fd()), how)
}

func unlockFile(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}
