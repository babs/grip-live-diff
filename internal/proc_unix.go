//go:build unix

package internal

import (
	"errors"
	"syscall"
)

// processAlive tells whether pid runs on this host. EPERM is a live process of another user.
func processAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
