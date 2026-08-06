package cli

import (
	"fmt"
	"os"

	"github.com/Gere2/neurofs/internal/atomicfile"
)

// regularFileInfo rejects links and special files before a configuration file
// is read or replaced. Callers should still use atomicWriteFile for writes so
// a path change between this check and the replacement cannot redirect bytes
// into another file.
func regularFileInfo(path string) (os.FileInfo, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("%s is a symbolic link", path)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", path)
	}
	return info, nil
}

// atomicWriteFile writes a sibling temporary file and renames it into place.
// Existing links and special files are refused. The temporary file is chmodded
// explicitly so the requested mode is also applied when replacing a file.
func atomicWriteFile(path string, data []byte, perm os.FileMode) error {
	if _, err := regularFileInfo(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return atomicfile.WriteFile(path, data, perm)
}
