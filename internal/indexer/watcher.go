package indexer

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Gere2/neurofs/internal/config"
	"github.com/Gere2/neurofs/internal/embeddings"
	"github.com/Gere2/neurofs/internal/fsutil"
	"github.com/Gere2/neurofs/internal/models"
	"github.com/Gere2/neurofs/internal/parser"
	"github.com/Gere2/neurofs/internal/storage"
	"github.com/fsnotify/fsnotify"
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
	watched    map[string]struct{}
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
		watched: make(map[string]struct{}),
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
			w.addDirectory(path)
		}
		return nil
	})
	if err != nil {
		w.mu.Lock()
		w.isWatching = false
		w.mu.Unlock()
		if closeErr := w.watcher.Close(); closeErr != nil {
			return fmt.Errorf("watcher walk failed: %w (close watcher: %v)", err, closeErr)
		}
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

func (w *Watcher) addDirectory(path string) {
	w.mu.Lock()
	if !w.isWatching {
		w.mu.Unlock()
		return
	}
	path = filepath.Clean(path)
	if _, ok := w.watched[path]; ok {
		w.mu.Unlock()
		return
	}
	err := w.watcher.Add(path)
	if err == nil {
		w.watched[path] = struct{}{}
	}
	w.mu.Unlock()
	if err != nil {
		w.logf("Watcher warning: failed to watch dir %s: %v", path, err)
	}
}

func (w *Watcher) forgetDirectoryTree(path string) {
	root := filepath.Clean(path)
	prefix := root + string(os.PathSeparator)
	w.mu.Lock()
	defer w.mu.Unlock()
	for watchedPath := range w.watched {
		if watchedPath == root || strings.HasPrefix(watchedPath, prefix) {
			delete(w.watched, watchedPath)
		}
	}
}

// indexFile publishes one complete file/chunk generation and then attaches
// embeddings carrying the exact checksum and vector-space provenance.
func (w *Watcher) indexFile(ctx context.Context, embClient *embeddings.Client, path string) (bool, error) {
	if !fsutil.IsSupported(path) || fsutil.ShouldSkipFile(path) {
		deleted, deleteErr := w.db.DeletePathTree(path)
		return deleted > 0, deleteErr
	}

	content, info, err := fsutil.ReadRegularFileBounded(path, config.MaxFileSize)
	if err != nil {
		if errors.Is(err, fsutil.ErrFileTooLarge) || errors.Is(err, fsutil.ErrNotRegular) {
			deleted, deleteErr := w.db.DeletePathTree(path)
			return deleted > 0, deleteErr
		}
		return false, err
	}
	lines := fsutil.CountLines(content)
	if lines > config.MaxFileLines {
		deleted, deleteErr := w.db.DeletePathTree(path)
		return deleted > 0, deleteErr
	}

	relPath := fsutil.RelPath(w.cfg.RepoRoot, path)
	checksum := fmt.Sprintf("%x", sha256.Sum256(content))
	lang := fsutil.LangForPath(path)
	parsed := parser.Parse(lang, string(content))
	record := models.FileRecord{
		Path:            path,
		RelPath:         relPath,
		Lang:            lang,
		Size:            int64(len(content)),
		ModTimeUnixNano: info.ModTime().UnixNano(),
		Lines:           lines,
		Symbols:         parsed.Symbols,
		Imports:         parsed.Imports,
		Checksum:        checksum,
		IndexedAt:       time.Now().UTC(),
	}

	chunkCount, err := persistChunks(ctx, w.db, embClient, record, string(content))
	if err != nil {
		return true, fmt.Errorf("store chunks: %w", err)
	}

	embedText := string(content)
	if len(embedText) > 8000 {
		embedText = embedText[:8000]
	}
	emb, err := embClient.GetEmbedding(ctx, embedText)
	if err != nil {
		w.logf("Watcher warning: embedding failed for %s: %v", relPath, err)
	} else if err := w.db.SaveEmbeddingWithMetadata(
		path,
		emb,
		checksum,
		embClient.ProviderName(),
		embClient.ModelName(),
	); err != nil {
		w.logf("Watcher warning: failed to save embedding for %s: %v", relPath, err)
	}

	w.logf("Watcher: incrementally indexed %s (%d chunks)", relPath, chunkCount)
	return true, nil
}

// indexTree handles directory moves atomically observed as a single create
// event: every descendant directory is registered and every supported file is
// indexed, including files that existed before the directory entered the repo.
func (w *Watcher) indexTree(ctx context.Context, embClient *embeddings.Client, root string) (bool, error) {
	updated := false
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			w.logf("Watcher warning: cannot inspect %s: %v", path, walkErr)
			return nil
		}
		if d.IsDir() {
			if fsutil.ShouldSkipDirAt(w.cfg.RepoRoot, path) {
				return filepath.SkipDir
			}
			w.addDirectory(path)
			return nil
		}
		if w.isIgnoredPath(path) {
			return nil
		}
		changed, err := w.indexFile(ctx, embClient, path)
		if err != nil {
			w.logf("Watcher error indexing %s: %v", fsutil.RelPath(w.cfg.RepoRoot, path), err)
			return nil
		}
		updated = updated || changed
		return nil
	})
	if err != nil {
		return updated, fmt.Errorf("walk new directory %s: %w", root, err)
	}
	return updated, nil
}

// listen waits for filesystem events and dispatches updates.
func (w *Watcher) listen(ctx context.Context) {
	const debounceDelay = 200 * time.Millisecond
	var (
		timer   *time.Timer
		timerCh <-chan time.Time
		events  = make(map[string]fsnotify.Op)
	)

	processEvents := func() {
		if len(events) == 0 {
			return
		}
		evs := events
		events = make(map[string]fsnotify.Op)

		w.logf("Watcher: processing %d file system events...", len(evs))
		embClient := embeddings.NewClient(w.cfg.HybridMode)
		if err := embClient.Validate(); err != nil {
			w.logf("Watcher error: invalid embedding configuration: %v", err)
			return
		}
		updated := false

		for path, op := range evs {
			if w.isIgnoredPath(path) {
				continue
			}
			if op&(fsnotify.Create|fsnotify.Write|fsnotify.Remove|fsnotify.Rename) == 0 {
				continue
			}
			if op&(fsnotify.Remove|fsnotify.Rename) != 0 {
				// A path recreated within the debounce window needs a fresh OS
				// watch even though its lexical path is unchanged.
				w.forgetDirectoryTree(path)
			}

			info, err := os.Lstat(path)
			if err != nil {
				if !os.IsNotExist(err) {
					w.logf("Watcher warning: cannot stat %s: %v", path, err)
					continue
				}
				relPath := fsutil.RelPath(w.cfg.RepoRoot, path)
				deleted, deleteErr := w.db.DeletePathTree(path)
				if deleteErr != nil {
					w.logf("Watcher error removing %s: %v", relPath, deleteErr)
					continue
				}
				if deleted > 0 {
					w.logf("Watcher: removed %s from index (%d files)", relPath, deleted)
					updated = true
				}
				continue
			}

			if info.IsDir() {
				changed, err := w.indexTree(ctx, embClient, path)
				if err != nil {
					w.logf("Watcher error indexing directory %s: %v", path, err)
					continue
				}
				updated = updated || changed
				continue
			}

			changed, err := w.indexFile(ctx, embClient, path)
			if err != nil {
				w.logf("Watcher error indexing %s: %v", fsutil.RelPath(w.cfg.RepoRoot, path), err)
				continue
			}
			updated = updated || changed
		}

		if updated {
			if err := w.db.PruneUnreferencedChunkEmbeddings(); err != nil {
				w.logf("Watcher warning: failed to prune stale chunk embeddings: %v", err)
			}
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
			if timer != nil {
				timer.Stop()
			}
			_ = w.Close()
			return
		case <-w.closed:
			if timer != nil {
				timer.Stop()
			}
			return
		case <-timerCh:
			timerCh = nil
			processEvents()
		case err, ok := <-w.watcher.Errors:
			if !ok {
				return
			}
			w.logf("Watcher warning: fsnotify error: %v", err)
		case ev, ok := <-w.watcher.Events:
			if !ok {
				return
			}
			events[ev.Name] |= ev.Op
			if timer == nil {
				timer = time.NewTimer(debounceDelay)
			} else {
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				timer.Reset(debounceDelay)
			}
			timerCh = timer.C
		}
	}
}
