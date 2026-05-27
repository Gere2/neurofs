// Package indexer walks a repository, parses files, and stores the result in
// the NeuroFS SQLite index.
package indexer

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"time"

	"github.com/neuromfs/neuromfs/internal/config"
	"github.com/neuromfs/neuromfs/internal/embeddings"
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
	Cached     int // files skipped because they are already indexed and unmodified
	Updated    int // existing records refreshed
	Removed    int // stale records deleted
	Symbols    int // total symbols extracted
	Imports    int // total unique imports extracted
	Chunks     int // total chunks extracted
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

	embClient := embeddings.NewClient(cfg.HybridMode)

	// Check if the embedding provider/model configuration has changed.
	// If it has, we invalidate the cached files and embeddings to force a clean re-scan
	// and ensure vector dimensionality and model coherence.
	currentProvider := embClient.ProviderName() + ":" + embClient.ModelName()
	storedProvider, hasStored, _ := db.GetMeta("embedding_provider")
	if hasStored && storedProvider != currentProvider {
		opts.Logf("  embedding provider/model changed from %q to %q; clearing index to force fresh re-scan...", storedProvider, currentProvider)
		if err := db.ClearIndex(); err != nil {
			return Stats{}, fmt.Errorf("indexer: clear index on provider change: %w", err)
		}
	}

	// Persist the current provider name in metadata.
	if err := db.SetMeta("embedding_provider", currentProvider); err != nil {
		return Stats{}, fmt.Errorf("indexer: set embedding provider: %w", err)
	}

	dbFiles, err := db.AllFiles()
	if err != nil {
		return Stats{}, fmt.Errorf("indexer: load index: %w", err)
	}

	cachedFiles := make(map[string]models.FileRecord, len(dbFiles))
	for _, f := range dbFiles {
		cachedFiles[f.Path] = f
	}

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

		relPath := fsutil.RelPath(cfg.RepoRoot, path)
		if cached, ok := cachedFiles[path]; ok {
			if info.Size() == cached.Size && info.ModTime().Unix() <= cached.IndexedAt.Unix() {
				chunks, err := db.GetChunksForFile(path)
				if err == nil && len(chunks) > 0 {
					opts.Logf("  cached: %s", relPath)
					stats.Cached++
					return nil
				}
				opts.Logf("  cache missing chunks, refreshing: %s", relPath)
			}
		}

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

		chunkCount, err := persistChunks(context.Background(), db, embClient, record, string(content))
		if err != nil {
			opts.Logf("  error chunking %s: %v", relPath, err)
			stats.Errors++
			return nil
		}

		// Generate and save embedding
		embedText := string(content)
		if len(embedText) > 8000 {
			embedText = embedText[:8000]
		}
		emb, err := embClient.GetEmbedding(context.Background(), embedText)
		if err != nil {
			opts.Logf("  warning: embedding failed for %s: %v", relPath, err)
		} else {
			if err := db.SaveEmbedding(path, emb); err != nil {
				opts.Logf("  warning: failed to save embedding for %s: %v", relPath, err)
			}
		}

		stats.Indexed++
		stats.Symbols += len(parsed.Symbols)
		stats.Imports += len(parsed.Imports)
		stats.Chunks += chunkCount
		opts.Logf("  indexed: %s (%s, %d symbols, %d chunks)", relPath, lang, len(parsed.Symbols), chunkCount)
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

	// Rebuild and save semantic dependency graph
	opts.Logf("  building semantic dependency graph...")
	allFiles, err := db.AllFiles()
	if err != nil {
		return stats, fmt.Errorf("indexer: load all files for dependency graph: %w", err)
	}
	relations := BuildRelations(allFiles)
	if err := db.UpdateRelations(relations); err != nil {
		return stats, fmt.Errorf("indexer: update file relations: %w", err)
	}
	opts.Logf("  semantic dependency graph: %d relationships persisted", len(relations))

	stats.Duration = time.Since(start)

	return stats, nil
}
