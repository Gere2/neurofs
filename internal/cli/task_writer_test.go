package cli

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestTaskUsesInjectedOutputWriter(t *testing.T) {
	t.Setenv("NEUROFS_EMBEDDING_PROVIDER", "mock")
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, "go.mod"), []byte("module example.com/taskwriter\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "main.go"), []byte("package main\nfunc main() {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	writeErr := errors.New("injected output failure")
	cmd := newTaskCmd()
	cmd.SetOut(errorWriter{err: writeErr})
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"where is main?", "--repo", repo, "--no-chunks"})

	err := cmd.Execute()
	if !errors.Is(err, writeErr) {
		t.Fatalf("task error = %v, want injected writer error %v", err, writeErr)
	}
}
