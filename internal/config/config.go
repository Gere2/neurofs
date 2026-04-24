// Package config holds NeuroFS configuration with sensible defaults.
package config

import (
	"fmt"
	"os"
	"path/filepath"
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
)

// Config holds runtime configuration for a NeuroFS session.
type Config struct {
	// RepoRoot is the absolute path of the repository being indexed.
	RepoRoot string

	// DBPath is the absolute path to the SQLite database.
	DBPath string

	// Budget is the token budget for bundle generation.
	Budget int
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

	return &Config{
		RepoRoot: abs,
		DBPath:   filepath.Join(abs, DirName, DBName),
		Budget:   DefaultBudget,
	}, nil
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
	return nil
}
