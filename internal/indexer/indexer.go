// Package indexer walks a repository, parses files, and stores the result in
// the NeuroFS SQLite index.
package indexer

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/Gere2/neurofs/internal/config"
	"github.com/Gere2/neurofs/internal/embeddings"
	"github.com/Gere2/neurofs/internal/fsutil"
	"github.com/Gere2/neurofs/internal/models"
	"github.com/Gere2/neurofs/internal/parser"
	"github.com/Gere2/neurofs/internal/project"
	"github.com/Gere2/neurofs/internal/storage"
)

const (
	// ProjectMetaKey is the metadata-table key under which indexer persists
	// the decoded project.Info. Exposed so other packages can read it.
	ProjectMetaKey = "project_info"

	indexerVersionMetaKey = "indexer_version"
	// indexerVersion changes whenever parser or chunk-boundary semantics
	// change. Source checksums alone cannot detect that a newer NeuroFS binary
	// must rebuild unchanged files with a corrected index representation.
	indexerVersion = "4"
)

// RequiresReindex reports whether db was built with different parser/chunker
// semantics. Callers that consume an existing non-empty index (notably
// retrieval.NewSession) use it to avoid serving stale chunks after a NeuroFS
// binary upgrade.
func RequiresReindex(db *storage.DB) (bool, error) {
	storedVersion, ok, err := db.GetMeta(indexerVersionMetaKey)
	if err != nil {
		return false, err
	}
	return !ok || storedVersion != indexerVersion, nil
}

// RequiresEmbeddingReindex reports whether db's persisted vectors belong to
// a different provider/model space than the currently configured client.
// Comparing vectors across spaces silently corrupts semantic ranking even
// when their dimensions happen to match.
func RequiresEmbeddingReindex(db *storage.DB, provider, model string) (bool, error) {
	storedProvider, ok, err := db.GetMeta("embedding_provider")
	if err != nil {
		return false, err
	}
	return !ok || storedProvider != provider+":"+model, nil
}

var errSourceIndexStale = errors.New("source index is stale")

// RequiresSourceReindex reports whether the indexable working tree differs
// from the file generations persisted in db. It applies the same traversal,
// language, size, line-count, and checksum rules as Run, but never mutates the
// database. Retrieval uses this check before loading a new session so a normal
// working-tree edit is indexed before its chunks are searched.
func RequiresSourceReindex(cfg *config.Config, db *storage.DB) (bool, error) {
	files, err := db.AllFiles()
	if err != nil {
		return false, fmt.Errorf("indexer: load index for freshness check: %w", err)
	}
	indexed := make(map[string]models.FileRecord, len(files))
	for _, file := range files {
		indexed[file.Path] = file
	}

	walkErr := fsutil.Walk(cfg.RepoRoot, func(path string, info os.FileInfo) error {
		lang := fsutil.LangForPath(path)
		if lang == models.LangUnknown {
			return nil
		}

		cached, wasIndexed := indexed[path]
		if info.Size() > config.MaxFileSize {
			if wasIndexed {
				return errSourceIndexStale
			}
			return nil
		}

		content, _, err := fsutil.ReadRegularFileBounded(path, config.MaxFileSize)
		if err != nil {
			if wasIndexed {
				return errSourceIndexStale
			}
			return nil
		}
		if fsutil.CountLines(content) > config.MaxFileLines {
			if wasIndexed {
				return errSourceIndexStale
			}
			return nil
		}
		if !wasIndexed {
			return errSourceIndexStale
		}

		checksum := fmt.Sprintf("%x", sha256.Sum256(content))
		if checksum != cached.Checksum {
			return errSourceIndexStale
		}
		delete(indexed, path)
		return nil
	})
	if errors.Is(walkErr, errSourceIndexStale) {
		return true, nil
	}
	if walkErr != nil {
		return false, fmt.Errorf("indexer: check source index freshness: %w", walkErr)
	}
	return len(indexed) > 0, nil
}

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

	// existingPaths tracks files that still exist on disk (for stale cleanup).
	existingPaths := make(map[string]bool)

	embClient := embeddings.NewClient(cfg.HybridMode)
	if err := embClient.Validate(); err != nil {
		return Stats{}, fmt.Errorf("indexer: invalid embedding configuration: %w", err)
	}

	// Check if the embedding provider/model configuration has changed.
	// If it has, we invalidate the cached files and embeddings to force a clean re-scan
	// and ensure vector dimensionality and model coherence.
	providerName := embClient.ProviderName()
	modelName := embClient.ModelName()
	currentProvider := providerName + ":" + modelName
	storedIndexerVersion, hasIndexerVersion, err := db.GetMeta(indexerVersionMetaKey)
	if err != nil {
		return Stats{}, fmt.Errorf("indexer: read indexer version: %w", err)
	}
	if !hasIndexerVersion || storedIndexerVersion != indexerVersion {
		opts.Logf("  indexer version changed from %q to %q; clearing index to rebuild chunks...", storedIndexerVersion, indexerVersion)
		if err := db.ClearIndex(); err != nil {
			return Stats{}, fmt.Errorf("indexer: clear index on version change: %w", err)
		}
	}

	storedProvider, hasStored, err := db.GetMeta("embedding_provider")
	if err != nil {
		return Stats{}, fmt.Errorf("indexer: read embedding provider: %w", err)
	}
	if hasStored && storedProvider != currentProvider {
		opts.Logf("  embedding provider/model changed from %q to %q; clearing index to force fresh re-scan...", storedProvider, currentProvider)
		if err := db.ClearIndex(); err != nil {
			return Stats{}, fmt.Errorf("indexer: clear index on provider change: %w", err)
		}
	}

	// ClearIndex also clears metadata, so publish all metadata only after any
	// provider-driven reset. This keeps one coherent generation visible.
	if err := db.SetMeta("repo_root", cfg.RepoRoot); err != nil {
		return Stats{}, fmt.Errorf("indexer: set meta: %w", err)
	}
	projInfo := project.Scan(cfg.RepoRoot)
	if err := db.SetMeta(ProjectMetaKey, projInfo.Encode()); err != nil {
		return Stats{}, fmt.Errorf("indexer: set project meta: %w", err)
	}
	if err := db.SetMeta("embedding_provider", currentProvider); err != nil {
		return Stats{}, fmt.Errorf("indexer: set embedding provider: %w", err)
	}
	if err := db.SetMeta(indexerVersionMetaKey, indexerVersion); err != nil {
		return Stats{}, fmt.Errorf("indexer: set indexer version: %w", err)
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

		content, currentInfo, err := fsutil.ReadRegularFileBounded(path, config.MaxFileSize)
		if err != nil {
			if errors.Is(err, fsutil.ErrFileTooLarge) || errors.Is(err, fsutil.ErrNotRegular) {
				opts.Logf("  skip (unsafe or too large): %s", path)
				stats.Skipped++
				return nil
			}
			// Preserve the previous generation on transient I/O or concurrent
			// edit errors; a future scan/watch event can retry it.
			existingPaths[path] = true
			opts.Logf("  error reading %s: %v", path, err)
			stats.Errors++
			return nil
		}
		info = currentInfo

		lines := fsutil.CountLines(content)
		if lines > config.MaxFileLines {
			opts.Logf("  skip (too many lines): %s", path)
			stats.Skipped++
			return nil
		}
		existingPaths[path] = true

		checksum := fmt.Sprintf("%x", sha256.Sum256(content))
		relPath := fsutil.RelPath(cfg.RepoRoot, path)

		// Size/mtime are only hints: checkouts and copy tools can preserve both.
		// The checksum is the integrity boundary before reusing persisted chunks
		// and embeddings.
		if cached, ok := cachedFiles[path]; ok && info.Size() == cached.Size && checksum == cached.Checksum {
			chunks, chunkErr := db.GetChunksForFile(path)
			hasEmbedding, embeddingErr := db.HasFileEmbedding(path, checksum, providerName, modelName)
			if chunkErr == nil && embeddingErr == nil && len(chunks) > 0 && hasEmbedding {
				opts.Logf("  cached: %s", relPath)
				stats.Cached++
				return nil
			}
			opts.Logf("  cache incomplete, refreshing: %s", relPath)
		}

		parsed := parser.Parse(lang, string(content))

		record := models.FileRecord{
			Path:            path,
			RelPath:         relPath,
			Lang:            lang,
			Size:            info.Size(),
			ModTimeUnixNano: info.ModTime().UnixNano(),
			Lines:           lines,
			Symbols:         parsed.Symbols,
			Imports:         parsed.Imports,
			Checksum:        checksum,
			IndexedAt:       time.Now().UTC(),
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
			if err := db.SaveEmbeddingWithMetadata(path, emb, checksum, providerName, modelName); err != nil {
				opts.Logf("  warning: failed to save embedding for %s: %v", relPath, err)
			}
		}

		stats.Indexed++
		if _, existed := cachedFiles[path]; existed {
			stats.Updated++
		}
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
	if err := db.PruneUnreferencedChunkEmbeddings(); err != nil {
		return stats, fmt.Errorf("indexer: prune chunk embeddings: %w", err)
	}

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
