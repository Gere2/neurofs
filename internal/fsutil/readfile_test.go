package fsutil

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Gere2/neurofs/internal/models"
)

func TestReadRegularFileBounded(t *testing.T) {
	path := filepath.Join(t.TempDir(), "source.go")
	if err := os.WriteFile(path, []byte("1234"), 0o644); err != nil {
		t.Fatal(err)
	}

	content, info, err := ReadRegularFileBounded(path, 4)
	if err != nil {
		t.Fatalf("read bounded file: %v", err)
	}
	if string(content) != "1234" {
		t.Fatalf("content = %q, want 1234", content)
	}
	if info.Size() != 4 || !info.Mode().IsRegular() {
		t.Fatalf("unexpected file info: size=%d mode=%v", info.Size(), info.Mode())
	}
}

func TestReadRegularFileBoundedRejectsActualBytesOverLimit(t *testing.T) {
	if _, err := readAllBounded(strings.NewReader("12345"), 4); !errors.Is(err, ErrFileTooLarge) {
		t.Fatalf("error = %v, want ErrFileTooLarge", err)
	}
}

func TestReadRegularFileBoundedRejectsSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target.go")
	link := filepath.Join(dir, "link.go")
	if err := os.WriteFile(target, []byte("package target"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}

	if _, _, err := ReadRegularFileBounded(link, 1024); !errors.Is(err, ErrNotRegular) {
		t.Fatalf("error = %v, want ErrNotRegular", err)
	}
}

func TestReadIndexedFileBoundedRejectsStaleGeneration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "source.go")
	if err := os.WriteFile(path, []byte("replacement"), 0o644); err != nil {
		t.Fatal(err)
	}
	record := models.FileRecord{
		Path: path, RelPath: "source.go",
		Checksum: "not-the-replacement-checksum",
	}
	if _, _, err := ReadIndexedFileBounded(record, 1024); !errors.Is(err, ErrChecksumMismatch) {
		t.Fatalf("error = %v, want ErrChecksumMismatch", err)
	}
}
