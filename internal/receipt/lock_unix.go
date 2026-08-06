//go:build !windows

package receipt

import (
	"os"
	"syscall"
)

// lockFile takes an advisory lock on the open descriptor itself — the same
// descriptor later used to read, validate and write, so there is no window
// to validate one inode and write another.
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
