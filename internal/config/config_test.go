package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Gere2/neurofs/internal/config"
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

func TestConfigJSONLoading(t *testing.T) {
	dir := t.TempDir()
	neurofsDir := filepath.Join(dir, config.DirName)
	if err := os.MkdirAll(neurofsDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	configJSON := `{"hybrid_mode": true, "budget": 12000}`
	if err := os.WriteFile(filepath.Join(neurofsDir, "config.json"), []byte(configJSON), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	cfg, err := config.New(dir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if !cfg.HybridMode {
		t.Errorf("expected HybridMode true, got false")
	}
	if cfg.Budget != 12000 {
		t.Errorf("expected Budget 12000, got %d", cfg.Budget)
	}
}

func TestNewRejectsSymlinkConfig(t *testing.T) {
	dir := t.TempDir()
	neurofsDir := filepath.Join(dir, config.DirName)
	if err := os.Mkdir(neurofsDir, 0o700); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	target := filepath.Join(dir, "outside-config.json")
	if err := os.WriteFile(target, []byte(`{"budget": 1}`), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := os.Symlink(target, filepath.Join(neurofsDir, "config.json")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	if _, err := config.New(dir); err == nil || !strings.Contains(err.Error(), "regular non-symlink") {
		t.Fatalf("New error = %v, want symlink rejection", err)
	}
}

func TestNewRejectsOversizedConfig(t *testing.T) {
	dir := t.TempDir()
	neurofsDir := filepath.Join(dir, config.DirName)
	if err := os.Mkdir(neurofsDir, 0o700); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	oversized := strings.Repeat("x", 65<<10)
	if err := os.WriteFile(filepath.Join(neurofsDir, "config.json"), []byte(oversized), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if _, err := config.New(dir); err == nil || !strings.Contains(err.Error(), "size limit") {
		t.Fatalf("New error = %v, want bounded-read rejection", err)
	}
}

func TestValidateRejectsNeuroFSSymlink(t *testing.T) {
	repo := t.TempDir()
	outside := t.TempDir()
	link := filepath.Join(repo, config.DirName)
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	if _, err := config.New(repo); err == nil || !strings.Contains(err.Error(), "not a symlink") {
		t.Fatalf("New error = %v, want .neurofs symlink rejection", err)
	}

	cfg := &config.Config{
		RepoRoot: repo,
		DBPath:   filepath.Join(link, config.DBName),
		Budget:   config.DefaultBudget,
	}
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "not a symlink") {
		t.Fatalf("Validate error = %v, want .neurofs symlink rejection", err)
	}
	if _, statErr := os.Stat(filepath.Join(outside, config.DBName)); !os.IsNotExist(statErr) {
		t.Fatalf("Validate must not create an external database; stat=%v", statErr)
	}
}
