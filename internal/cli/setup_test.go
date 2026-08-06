package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func runSetup(t *testing.T, repo string) string {
	t.Helper()
	cmd := New()
	buf := new(bytes.Buffer)
	cmd.SetOut(buf)
	cmd.SetErr(buf)
	cmd.SetArgs([]string{"setup", repo})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("setup: %v\n%s", err, buf.String())
	}
	return buf.String()
}

func TestSetupWiresARepoIdempotently(t *testing.T) {
	t.Setenv("NEUROFS_EMBEDDING_PROVIDER", "mock")
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, "main.go"),
		[]byte("package main\n\nfunc Hello() string { return \"hi\" }\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	runSetup(t, repo)

	if _, err := os.Stat(filepath.Join(repo, ".neurofs", "index.db")); err != nil {
		t.Fatalf("index not built: %v", err)
	}
	claude, err := os.ReadFile(filepath.Join(repo, "CLAUDE.md"))
	if err != nil {
		t.Fatalf("CLAUDE.md not created: %v", err)
	}
	for _, want := range []string{"neurofs_context", "neurofs_feedback"} {
		if !strings.Contains(string(claude), want) {
			t.Errorf("CLAUDE.md missing %q:\n%s", want, claude)
		}
	}

	// Second run must not duplicate the block.
	runSetup(t, repo)
	again, _ := os.ReadFile(filepath.Join(repo, "CLAUDE.md"))
	if strings.Count(string(again), "## Retrieval (NeuroFS)") != 1 {
		t.Fatalf("retrieval block duplicated:\n%s", again)
	}
}

func TestSetupAppendsToExistingClaudeMD(t *testing.T) {
	t.Setenv("NEUROFS_EMBEDDING_PROVIDER", "mock")
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, "main.go"),
		[]byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	original := "# My project\n\nBuild with make.\n"
	if err := os.WriteFile(filepath.Join(repo, "CLAUDE.md"), []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}

	runSetup(t, repo)

	claude, _ := os.ReadFile(filepath.Join(repo, "CLAUDE.md"))
	if !strings.HasPrefix(string(claude), original) {
		t.Fatalf("existing CLAUDE.md content not preserved:\n%s", claude)
	}
	if !strings.Contains(string(claude), "neurofs_context") {
		t.Fatalf("retrieval block not appended:\n%s", claude)
	}
}

func TestWireClaudeMDRejectsSymlink(t *testing.T) {
	repo := t.TempDir()
	target := filepath.Join(t.TempDir(), "outside.md")
	const original = "# External file\n"
	if err := os.WriteFile(target, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(repo, "CLAUDE.md")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	if _, err := wireClaudeMD(repo); err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("wireClaudeMD error = %v, want symbolic-link rejection", err)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != original {
		t.Fatalf("symlink target changed: %q", got)
	}
}
