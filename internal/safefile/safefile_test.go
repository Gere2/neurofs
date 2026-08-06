package safefile

import (
	"os"
	"path/filepath"
	"testing"
)

func TestOpenAppendCreatesSecureRegularFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "log.jsonl")
	file, err := OpenAppend(path, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString("line\n"); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("permissions = %o", info.Mode().Perm())
	}
}

func TestOpenOperationsRejectSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	link := filepath.Join(dir, "link")
	if err := os.WriteFile(target, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenAppend(link, 0o600); err == nil {
		t.Fatal("append followed symlink")
	}
	if _, err := OpenRead(link); err == nil {
		t.Fatal("read followed symlink")
	}
}
