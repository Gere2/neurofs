//go:build windows

package receipt

import (
	"os"

	"golang.org/x/sys/windows"
)

// lockFile locks the byte range [0,1) via LockFileEx. All ledger writers and
// readers lock the same range, which makes it a cross-process mutex with
// shared/exclusive semantics equivalent to flock on Unix.
func lockFile(f *os.File, exclusive bool) error {
	var flags uint32
	if exclusive {
		flags = windows.LOCKFILE_EXCLUSIVE_LOCK
	}
	return windows.LockFileEx(windows.Handle(f.Fd()), flags, 0, 1, 0, new(windows.Overlapped))
}

func unlockFile(f *os.File) error {
	return windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, new(windows.Overlapped))
}
