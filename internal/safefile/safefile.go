// Package safefile opens persistent local-state files without following a
// final-component symbolic link.
package safefile

import (
	"fmt"
	"os"
)

// OpenAppend opens path for append, creating it with perm when needed. The
// final path component must remain the same regular, non-symlink file between
// inspection and open. No bytes or chmod are applied before that verification.
func OpenAppend(path string, perm os.FileMode) (*os.File, error) {
	if err := inspectExisting(path); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, perm)
	if err != nil {
		return nil, err
	}
	if err := verifyOpened(path, file); err != nil {
		_ = file.Close()
		return nil, err
	}
	if err := file.Chmod(perm); err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}

// OpenRead opens an existing regular, non-symlink file and verifies that the
// descriptor still names the inspected path.
func OpenRead(path string) (*os.File, error) {
	if err := inspectExisting(path); err != nil {
		return nil, err
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	if err := verifyOpened(path, file); err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}

func inspectExisting(path string) error {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular non-symlink file", path)
	}
	return nil
}

func verifyOpened(path string, file *os.File) error {
	opened, err := file.Stat()
	if err != nil {
		return err
	}
	current, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !opened.Mode().IsRegular() ||
		current.Mode()&os.ModeSymlink != 0 ||
		!current.Mode().IsRegular() ||
		!os.SameFile(opened, current) {
		return fmt.Errorf("%s changed or is not a regular non-symlink file", path)
	}
	return nil
}
