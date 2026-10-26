//go:build windows

package internal

import (
	"errors"
	"syscall"
)

const (
	processQueryLimitedInformation = 0x1000
	stillActive                    = 259
)

// processAlive tells whether pid runs on this host: a handle outlives the process, so the exit
// code tells a running one from a dead one. Access denied is a live process of another user.
func processAlive(pid int) bool {
	h, err := syscall.OpenProcess(processQueryLimitedInformation, false, uint32(pid)) //nolint:gosec // pid range checked by the caller
	if err != nil {
		return errors.Is(err, syscall.ERROR_ACCESS_DENIED)
	}
	//nolint:errcheck
	defer syscall.CloseHandle(h)
	var code uint32
	return syscall.GetExitCodeProcess(h, &code) == nil && code == stillActive
}
