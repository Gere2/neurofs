// Package atomicfile provides crash-safe replacement of small generated
// artefacts. Callers never expose a partially-written target: bytes are written
// and synced in the destination directory before a final rename.
package atomicfile

import (
	"fmt"
	"os"
	"path/filepath"
)

// WriteFile atomically replaces path with data using perm for newly-created
// files. The temporary file is always created beside path so rename stays on
// one filesystem.
func WriteFile(path string, data []byte, perm os.FileMode) (err error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("atomicfile: create directory: %w", err)
	}

	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("atomicfile: create temp file: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
	}()

	if err := tmp.Chmod(perm); err != nil {
		return fmt.Errorf("atomicfile: chmod temp file: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		return fmt.Errorf("atomicfile: write temp file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("atomicfile: sync temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("atomicfile: close temp file: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("atomicfile: replace target: %w", err)
	}

	// Best effort: syncing the directory makes the rename durable on filesystems
	// that support directory fsync. Failure here does not mean partial content
	// was exposed, so do not turn a successful replacement into an error.
	if d, openErr := os.Open(dir); openErr == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}
