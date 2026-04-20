// Package indexer walks a repository, parses files, and stores the result in
// the NeuroFS SQLite index.
package indexer

import (
	"crypto/sha256"
	"fmt"
	"os"
	"time"

	"github.com/neuromfs/neuromfs/internal/config"
	"github.com/neuromfs/neuromfs/internal/fsutil"
	"github.com/neuromfs/neuromfs/internal/models"
	"github.com/neuromfs/neuromfs/internal/parser"
	"github.com/neuromfs/neuromfs/internal/project"
	"github.com/neuromfs/neuromfs/internal/storage"
)

// ProjectMetaKey is the metadata-table key under which indexer persists the
// decoded project.Info. Exposed so other packages (CLI, ranking) can read it.
const ProjectMetaKey = "project_info"

// Stats summarises what happened during an indexing run.
type Stats struct {
	Discovered int // all files visited
	Skipped    int // unsupported or ignored
	Indexed    int // successfully written to the DB
	Updated    int // existing records refreshed
	Removed    int // stale records deleted
	Symbols    int // total symbols extracted
	Imports    int // total unique imports extracted
	Errors     int // files that produced errors (skipped)
	Duration   time.Duration
}

// Options configures an indexing run.
type Options struct {
	// Verbose enables per-file logging via the provided function.
	// If nil, no per-file output is produced.
	Logf func(format string, args ...any)
}

// Run indexes the repository rooted at cfg.RepoRoot and stores results in
// the database at cfg.DBPath. It returns indexing statistics.
func Run(cfg *config.Config, db *storage.DB, opts Options) (Stats, error) {
	if opts.Logf == nil {
		opts.Logf = func(string, ...any) {}
	}

	start := time.Now()

	// Record repo root in metadata.
	if err := db.SetMeta("repo_root", cfg.RepoRoot); err != nil {
		return Stats{}, fmt.Errorf("indexer: set meta: %w", err)
	}

	// Extract and persist project info (package.json, tsconfig.json) so
	// ranking can consume it without re-reading disk on every query.
	projInfo := project.Scan(cfg.RepoRoot)
	if err := db.SetMeta(ProjectMetaKey, projInfo.Encode()); err != nil {
		return Stats{}, fmt.Errorf("indexer: set project meta: %w", err)
	}

	// existingPaths tracks files that still exist on disk (for stale cleanup).
	existingPaths := make(map[string]bool)

	var stats Stats

	walkErr := fsutil.Walk(cfg.RepoRoot, func(path string, info os.FileInfo) error {
		stats.Discovered++

		lang := fsutil.LangForPath(path)
		if lang == models.LangUnknown {
			stats.Skipped++
			return nil
		}
		if info.Size() > config.MaxFileSize {
			opts.Logf("  skip (too large): %s", path)
			stats.Skipped++
			return nil
		}

		existingPaths[path] = true

		content, err := os.ReadFile(path)
		if err != nil {
			opts.Logf("  error reading %s: %v", path, err)
			stats.Errors++
			return nil
		}

		lines := fsutil.CountLines(content)
		if lines > config.MaxFileLines {
			opts.Logf("  skip (too many lines): %s", path)
			stats.Skipped++
			return nil
		}

		checksum := fmt.Sprintf("%x", sha256.Sum256(content))
		relPath := fsutil.RelPath(cfg.RepoRoot, path)

		parsed := parser.Parse(lang, string(content))

		record := models.FileRecord{
			Path:      path,
			RelPath:   relPath,
			Lang:      lang,
			Size:      info.Size(),
			Lines:     lines,
			Symbols:   parsed.Symbols,
			Imports:   parsed.Imports,
			Checksum:  checksum,
			IndexedAt: time.Now().UTC(),
		}

		if err := db.UpsertFile(record); err != nil {
			opts.Logf("  error storing %s: %v", path, err)
			stats.Errors++
			return nil
		}

		stats.Indexed++
		stats.Symbols += len(parsed.Symbols)
		stats.Imports += len(parsed.Imports)
		opts.Logf("  indexed: %s (%s, %d symbols)", relPath, lang, len(parsed.Symbols))
		return nil
	})

	if walkErr != nil {
		return stats, fmt.Errorf("indexer: walk: %w", walkErr)
	}

	// Clean up stale records.
	removed, err := db.DeleteRemovedFiles(existingPaths)
	if err != nil {
		return stats, fmt.Errorf("indexer: cleanup: %w", err)
	}
	stats.Removed = removed
	stats.Duration = time.Since(start)

	return stats, nil
}
