//go:build windows

package internal

import "os"

// ponytail: no advisory lock on windows; LockFileEx via x/sys if a windows agent needs it.
func lockFile(*os.File, bool) error { return nil }

func unlockFile(*os.File) error { return nil }
