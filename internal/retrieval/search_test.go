package retrieval

import (
	"path/filepath"
	"testing"
)

func TestResolveRepoReturnsAbsolutePath(t *testing.T) {
	got, err := resolveRepo(".")
	if err != nil {
		t.Fatalf("resolve repo: %v", err)
	}
	want, err := filepath.Abs(".")
	if err != nil {
		t.Fatalf("abs cwd: %v", err)
	}
	if got != want {
		t.Fatalf("resolveRepo(.) = %q, want %q", got, want)
	}
}
