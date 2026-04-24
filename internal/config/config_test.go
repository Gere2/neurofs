package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/neuromfs/neuromfs/internal/config"
)

func TestNewResolvesAbsolutePath(t *testing.T) {
	cfg, err := config.New("/tmp")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if cfg.RepoRoot != "/tmp" {
		t.Fatalf("RepoRoot: got %q want /tmp", cfg.RepoRoot)
	}
	if !strings.HasSuffix(cfg.DBPath, "/tmp/"+config.DirName+"/"+config.DBName) {
		t.Fatalf("DBPath: got %q", cfg.DBPath)
	}
}

func TestValidateAcceptsDirectory(t *testing.T) {
	dir := t.TempDir()
	cfg, err := config.New(dir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: unexpected error %v", err)
	}
}

func TestValidateRejectsMissingPath(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "does-not-exist")
	cfg, err := config.New(missing)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	err = cfg.Validate()
	if err == nil {
		t.Fatalf("Validate: expected error for missing path")
	}
	if !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("Validate: error %q should mention 'does not exist'", err)
	}
	// And — crucially — Validate must not have created any side effects.
	if _, statErr := os.Stat(missing); !os.IsNotExist(statErr) {
		t.Fatalf("Validate must not create the missing path; stat=%v", statErr)
	}
}

func TestValidateRejectsFile(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "notadir")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	cfg, err := config.New(file)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	err = cfg.Validate()
	if err == nil {
		t.Fatalf("Validate: expected error for file path")
	}
	if !strings.Contains(err.Error(), "must be a directory") {
		t.Fatalf("Validate: error %q should mention 'must be a directory'", err)
	}
}

func TestDBDirLivesInsideRepoRoot(t *testing.T) {
	dir := t.TempDir()
	cfg, err := config.New(dir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if !strings.HasPrefix(cfg.DBDir(), dir) {
		t.Fatalf("DBDir %q should live under repo root %q", cfg.DBDir(), dir)
	}
}
