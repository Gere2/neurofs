package fsutil

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/Gere2/neurofs/internal/models"
)

var (
	ErrFileTooLarge     = errors.New("file exceeds read size limit")
	ErrNotRegular       = errors.New("path is not a regular non-symlink file")
	ErrFileChanged      = errors.New("file changed while being read")
	ErrInvalidMaxLen    = errors.New("invalid file size limit")
	ErrChecksumMismatch = errors.New("file checksum no longer matches index")
)

// ReadRegularFileBounded revalidates path immediately before opening it,
// confirms that the opened descriptor is the same regular non-symlink file,
// and caps bytes actually consumed. The identity checks close the common
// Lstat/Open rename race without relying on platform-specific O_NOFOLLOW.
func ReadRegularFileBounded(path string, maxBytes int64) ([]byte, os.FileInfo, error) {
	if maxBytes < 0 {
		return nil, nil, ErrInvalidMaxLen
	}
	for attempt := 0; attempt < 2; attempt++ {
		content, info, err := readRegularFileGeneration(path, maxBytes)
		if !errors.Is(err, ErrFileChanged) {
			return content, info, err
		}
	}
	return nil, nil, fmt.Errorf("%w: %s", ErrFileChanged, path)
}

// ReadIndexedFileBounded additionally verifies that bytes still match the
// indexed generation. Empty checksums are accepted for legacy/synthetic
// records, but production records always carry SHA-256.
func ReadIndexedFileBounded(record models.FileRecord, maxBytes int64) ([]byte, os.FileInfo, error) {
	content, info, err := ReadRegularFileBounded(record.Path, maxBytes)
	if err != nil {
		return nil, info, err
	}
	if record.Checksum != "" {
		checksum := fmt.Sprintf("%x", sha256.Sum256(content))
		if checksum != record.Checksum {
			return nil, info, fmt.Errorf("%w: %s", ErrChecksumMismatch, record.RelPath)
		}
	}
	return content, info, nil
}

func readRegularFileGeneration(path string, maxBytes int64) ([]byte, os.FileInfo, error) {
	before, err := os.Lstat(path)
	if err != nil {
		return nil, nil, err
	}
	if before.Mode()&os.ModeSymlink != 0 || !before.Mode().IsRegular() {
		return nil, nil, fmt.Errorf("%w: %s", ErrNotRegular, path)
	}
	if before.Size() > maxBytes {
		return nil, before, fmt.Errorf("%w: %s", ErrFileTooLarge, path)
	}

	file, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = file.Close() }()

	openedBefore, err := file.Stat()
	if err != nil {
		return nil, nil, err
	}
	if !openedBefore.Mode().IsRegular() || !os.SameFile(before, openedBefore) {
		return nil, nil, fmt.Errorf("%w: %s", ErrFileChanged, path)
	}

	content, err := readAllBounded(file, maxBytes)
	if errors.Is(err, ErrFileTooLarge) {
		return nil, openedBefore, fmt.Errorf("%w: %s", ErrFileTooLarge, path)
	}
	if err != nil {
		return nil, nil, err
	}

	openedAfter, err := file.Stat()
	if err != nil {
		return nil, nil, err
	}
	if !openedAfter.Mode().IsRegular() || openedAfter.Size() > maxBytes {
		return nil, openedAfter, fmt.Errorf("%w: %s", ErrFileTooLarge, path)
	}
	if openedBefore.Size() != openedAfter.Size() ||
		openedBefore.ModTime() != openedAfter.ModTime() {
		return nil, nil, fmt.Errorf("%w: %s", ErrFileChanged, path)
	}

	afterPath, err := os.Lstat(path)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %s", ErrFileChanged, path)
	}
	if afterPath.Mode()&os.ModeSymlink != 0 || !afterPath.Mode().IsRegular() {
		return nil, nil, fmt.Errorf("%w: %s", ErrNotRegular, path)
	}
	if !os.SameFile(openedAfter, afterPath) {
		return nil, nil, fmt.Errorf("%w: %s", ErrFileChanged, path)
	}
	return content, openedAfter, nil
}

func readAllBounded(reader io.Reader, maxBytes int64) ([]byte, error) {
	content, err := io.ReadAll(io.LimitReader(reader, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(content)) > maxBytes {
		return nil, ErrFileTooLarge
	}
	return content, nil
}
