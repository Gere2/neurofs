// Package config holds NeuroFS configuration with sensible defaults.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/Gere2/neurofs/internal/fsutil"
)

const (
	// DirName is the hidden directory created inside a scanned repository.
	DirName = ".neurofs"
	// DBName is the SQLite database file name.
	DBName = "index.db"

	// DefaultBudget is the default token budget for context bundles.
	DefaultBudget = 8000

	// MaxFileSize is the largest file the indexer will process (bytes).
	// Files larger than this are recorded but not parsed in depth.
	MaxFileSize = 512 * 1024 // 512 KB

	// MaxFileLines is the largest file (in lines) that will be fully parsed.
	MaxFileLines = 5000

	// maxConfigFileBytes bounds the amount of local configuration that can
	// be pulled into memory. NeuroFS' config is intentionally tiny; 64 KiB
	// leaves ample room for future fields without making config.json an
	// unbounded read surface.
	maxConfigFileBytes = int64(64 << 10)
)

// Config holds runtime configuration for a NeuroFS session.
type Config struct {
	// RepoRoot is the absolute path of the repository being indexed.
	RepoRoot string

	// DBPath is the absolute path to the SQLite database.
	DBPath string

	// Budget is the token budget for bundle generation.
	Budget int

	// HybridMode configures local embeddings and cloud reasoning.
	HybridMode bool
}

// New returns a Config rooted at the given directory.
// If root is empty it defaults to the current working directory.
func New(root string) (*Config, error) {
	if root == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return nil, err
		}
		root = cwd
	}

	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}

	cfg := &Config{
		RepoRoot: abs,
		DBPath:   filepath.Join(abs, DirName, DBName),
		Budget:   DefaultBudget,
	}

	// Preserve New's historical ability to construct a Config for a path
	// that Validate will later reject. Only inspect repository metadata when
	// root currently resolves to a directory.
	if rootInfo, statErr := os.Stat(abs); statErr == nil && rootInfo.IsDir() {
		if err := validateNeuroFSDir(abs); err != nil {
			return nil, err
		}

		// Try to load config from .neurofs/config.json if it exists.
		configFile := filepath.Join(abs, DirName, "config.json")
		data, _, readErr := fsutil.ReadRegularFileBounded(configFile, maxConfigFileBytes)
		if readErr != nil && !os.IsNotExist(readErr) {
			return nil, fmt.Errorf("read config %s: %w", configFile, readErr)
		}
		if readErr == nil {
			var raw struct {
				HybridMode bool `json:"hybrid_mode"`
				Budget     int  `json:"budget"`
			}
			if err := json.Unmarshal(data, &raw); err == nil {
				cfg.HybridMode = raw.HybridMode
				if raw.Budget > 0 {
					cfg.Budget = raw.Budget
				}
			}
		}
	}

	return cfg, nil
}

// DBDir returns the directory that contains the database file.
func (c *Config) DBDir() string {
	return filepath.Dir(c.DBPath)
}

// Validate checks that RepoRoot points at an existing directory. Without
// this, storage.Open would silently MkdirAll a .neurofs/ tree inside any
// path the caller supplies — including ones that were typos — leaving
// stray directories across the filesystem. The CLI and UI both call this
// before opening the index.
func (c *Config) Validate() error {
	info, err := os.Stat(c.RepoRoot)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("repo root does not exist: %s", c.RepoRoot)
		}
		return fmt.Errorf("stat repo root: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("repo root must be a directory: %s", c.RepoRoot)
	}
	return validateNeuroFSDir(c.RepoRoot)
}

// validateNeuroFSDir rejects a metadata path that could redirect database,
// cache, or index writes outside the selected repository. A missing path is
// valid: first-time setup creates it later.
func validateNeuroFSDir(repoRoot string) error {
	path := filepath.Join(repoRoot, DirName)
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("lstat %s: %w", path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s must be a real directory, not a symlink", path)
	}
	if !info.IsDir() {
		return fmt.Errorf("%s must be a directory", path)
	}
	return nil
}
