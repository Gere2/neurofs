package indexer

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/neuromfs/neuromfs/internal/config"
	"github.com/neuromfs/neuromfs/internal/embeddings"
	"github.com/neuromfs/neuromfs/internal/fsutil"
	"github.com/neuromfs/neuromfs/internal/models"
	"github.com/neuromfs/neuromfs/internal/parser"
	"github.com/neuromfs/neuromfs/internal/storage"
)

// Watcher monitors a repository filesystem for changes and updates the index incrementally.
type Watcher struct {
	cfg        *config.Config
	db         *storage.DB
	logf       func(format string, args ...any)
	watcher    *fsnotify.Watcher
	mu         sync.Mutex
	isWatching bool
	closed     chan struct{}
}

// NewWatcher returns a new filesystem watcher for the repository.
func NewWatcher(cfg *config.Config, db *storage.DB, logf func(format string, args ...any)) (*Watcher, error) {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, fmt.Errorf("watcher: failed to create fsnotify watcher: %w", err)
	}
	return &Watcher{
		cfg:     cfg,
		db:      db,
		logf:    logf,
		watcher: fsw,
		closed:  make(chan struct{}),
	}, nil
}

// Start initiates directory walking, registers all subdirectories to fsnotify,
// and starts the background listening loop.
func (w *Watcher) Start(ctx context.Context) error {
	w.mu.Lock()
	if w.isWatching {
		w.mu.Unlock()
		return nil
	}
	w.isWatching = true
	w.mu.Unlock()

	w.logf("Watcher: starting scan and registration of directories in %s...", w.cfg.RepoRoot)

	// Watch the root and all its subdirectories recursively (excluding ignored ones)
	err := filepath.WalkDir(w.cfg.RepoRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if fsutil.ShouldSkipDirAt(w.cfg.RepoRoot, path) {
				return filepath.SkipDir
			}
			// Register dir to watcher
			if err := w.watcher.Add(path); err != nil {
				w.logf("Watcher warning: failed to watch dir %s: %v", path, err)
			}
		}
		return nil
	})
	if err != nil {
		w.watcher.Close()
		return fmt.Errorf("watcher walk failed: %w", err)
	}

	go w.listen(ctx)
	return nil
}

// Close shuts down the fsnotify watcher.
func (w *Watcher) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.isWatching {
		return nil
	}
	w.isWatching = false
	close(w.closed)
	return w.watcher.Close()
}

// isIgnoredPath checks if a path falls under ignored directories or
// ignored patterns. Walks up the directory chain from path toward the
// repo root and asks ShouldSkipDirAt about each ancestor, so a name
// like "audit" only matters as an ancestor when it sits directly under
// the repo root.
func (w *Watcher) isIgnoredPath(path string) bool {
	cur := path
	for cur != "" && cur != w.cfg.RepoRoot {
		if fsutil.ShouldSkipDirAt(w.cfg.RepoRoot, cur) {
			return true
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			break
		}
		cur = parent
	}
	return false
}

// listen waits for filesystem events and dispatches updates.
func (w *Watcher) listen(ctx context.Context) {
	const debounceDelay = 200 * time.Millisecond
	var (
		timer  *time.Timer
		events []fsnotify.Event
		evMu   sync.Mutex
	)

	processEvents := func() {
		evMu.Lock()
		evs := events
		events = nil
		evMu.Unlock()

		if len(evs) == 0 {
			return
		}

		w.logf("Watcher: processing %d file system events...", len(evs))
		embClient := embeddings.NewClient(w.cfg.HybridMode)
		updated := false

		for _, ev := range evs {
			path := ev.Name

			if w.isIgnoredPath(path) {
				continue
			}

			if ev.Has(fsnotify.Create) || ev.Has(fsnotify.Write) {
				info, err := os.Stat(path)
				if err != nil {
					// File might have been deleted after event was fired
					continue
				}

				if info.IsDir() {
					// Register new directory to fsnotify
					w.mu.Lock()
					if w.isWatching {
						if err := w.watcher.Add(path); err == nil {
							w.logf("Watcher: watching new directory %s", path)
						}
					}
					w.mu.Unlock()
					continue
				}

				// Check exclusions
				if !fsutil.IsSupported(path) || fsutil.ShouldSkipFile(path) || info.Size() > config.MaxFileSize {
					continue
				}

				content, err := os.ReadFile(path)
				if err != nil {
					continue
				}

				lines := fsutil.CountLines(content)
				if lines > config.MaxFileLines {
					continue
				}

				relPath := fsutil.RelPath(w.cfg.RepoRoot, path)
				checksum := fmt.Sprintf("%x", sha256.Sum256(content))
				lang := fsutil.LangForPath(path)
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

				if err := w.db.UpsertFile(record); err != nil {
					w.logf("Watcher error storing %s: %v", relPath, err)
					continue
				}

				chunkCount, err := persistChunks(ctx, w.db, embClient, record, string(content))
				if err != nil {
					w.logf("Watcher error chunking %s: %v", relPath, err)
					continue
				}

				// Generate embedding
				embedText := string(content)
				if len(embedText) > 8000 {
					embedText = embedText[:8000]
				}
				emb, err := embClient.GetEmbedding(ctx, embedText)
				if err == nil {
					_ = w.db.SaveEmbedding(path, emb)
				}
				w.logf("Watcher: incrementally indexed %s (%d chunks)", relPath, chunkCount)
				updated = true

			} else if ev.Has(fsnotify.Remove) || ev.Has(fsnotify.Rename) {
				relPath := fsutil.RelPath(w.cfg.RepoRoot, path)
				if err := w.db.DeleteFile(path); err == nil {
					w.logf("Watcher: removed %s from index", relPath)
					updated = true
				}
			}
		}

		if updated {
			w.logf("Watcher: rebuilding semantic dependency graph...")
			allFiles, err := w.db.AllFiles()
			if err == nil {
				relations := BuildRelations(allFiles)
				if err := w.db.UpdateRelations(relations); err == nil {
					w.logf("Watcher: updated semantic graph with %d relations", len(relations))
				}
			}
		}
	}

	for {
		select {
		case <-ctx.Done():
			w.Close()
			return
		case <-w.closed:
			return
		case err, ok := <-w.watcher.Errors:
			if !ok {
				return
			}
			w.logf("Watcher warning: fsnotify error: %v", err)
		case ev, ok := <-w.watcher.Events:
			if !ok {
				return
			}
			evMu.Lock()
			events = append(events, ev)
			if timer != nil {
				timer.Stop()
			}
			timer = time.AfterFunc(debounceDelay, processEvents)
			evMu.Unlock()
		}
	}
}
