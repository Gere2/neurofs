package retrieval

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/Gere2/neurofs/internal/config"
	"github.com/Gere2/neurofs/internal/fsutil"
	"github.com/Gere2/neurofs/internal/indexer"
	"github.com/Gere2/neurofs/internal/storage"
)

// A long-lived process (the MCP server above all — one per agent session,
// pinned to a repo) pays the full index load on every one-shot Search:
// AllFiles + AllChunks + a file read per chunk. SessionFor amortizes that
// by caching one Session per repo and rebuilding it only when the index
// database actually changes on disk (including WAL-only commits). The
// working-set git signal is refreshed on a short TTL so recently edited
// files keep their ranking boost between scans.
//
// Cached file contents always stay consistent with indexed chunk line-ranges.
// DB/WAL revisions invalidate immediately; source-only drift is checked on a
// short TTL and then repaired through NewSession before the entry is reused.
const (
	changedPathsTTL              = 30 * time.Second
	sourceGenerationTTL          = 2 * time.Second
	maxPoolSessions              = 8
	maxExactCacheLen             = 128
	maxStableSessionLoadAttempts = 3
)

type pooledSession struct {
	session  *Session
	revision indexRevision
	gitAt    time.Time
	sourceAt time.Time
	lastUsed time.Time
	mu       sync.Mutex
}

type fileRevision struct {
	exists          bool
	size            int64
	modTimeUnixNano int64
}

type indexRevision struct {
	database fileRevision
	wal      fileRevision
}

var (
	poolMu sync.Mutex
	pool   = map[string]*pooledSession{}
)

// SearchShared runs a query through the shared per-repo session pool.
// Callers that issue many queries against a possibly-changing repo (MCP,
// benchmarks) get amortized index loads; one-shot CLI callers can keep
// using Search.
func SearchShared(ctx context.Context, opts Options) (Response, error) {
	if opts.DisableIndexRefresh {
		session, err := newSession(ctx, opts.Repo, true)
		if err != nil {
			return Response{}, err
		}
		return session.Search(ctx, opts)
	}
	repo, err := resolveRepo(opts.Repo)
	if err != nil {
		return Response{}, err
	}
	entry, err := sessionFor(ctx, repo)
	if err != nil {
		return Response{}, err
	}
	entry.mu.Lock()
	defer entry.mu.Unlock()
	return entry.session.Search(ctx, opts)
}

func sessionFor(ctx context.Context, repo string) (*pooledSession, error) {
	revision := currentIndexRevision(repo)
	now := time.Now()

	poolMu.Lock()
	entry, ok := pool[repo]
	sameRevision := ok && entry.revision == revision
	var sourceAt time.Time
	if sameRevision {
		sourceAt = entry.sourceAt
	}
	poolMu.Unlock()

	// A source edit does not change SQLite or its WAL. Verify the indexed
	// source generation on a short TTL before reusing an otherwise unchanged
	// entry. This bounds source-only staleness without turning every MCP query
	// into a full-tree checksum pass. Any inspection error takes the
	// conservative reload path; NewSession will either repair the generation
	// or return the actionable underlying error.
	sourceAge := now.Sub(sourceAt)
	checkedRecently := sameRevision &&
		!sourceAt.IsZero() &&
		sourceAge >= 0 &&
		sourceAge < sourceGenerationTTL
	validatedSource := false
	sourceFresh := checkedRecently
	if sameRevision && !checkedRecently {
		sourceFresh = pooledSourceGenerationFresh(repo)
		validatedSource = sourceFresh
	}
	sourceFresh = sourceFresh && currentIndexRevision(repo) == revision
	if sourceFresh {
		poolMu.Lock()
		// The entry may have been evicted or replaced while the filesystem
		// check ran. Reuse it only while it is still the canonical pool entry.
		sourceFresh = pool[repo] == entry
		if sourceFresh {
			if validatedSource {
				entry.sourceAt = now
			}
			entry.lastUsed = now
		}
		poolMu.Unlock()
	}

	if sourceFresh {
		entry.mu.Lock()
		if time.Since(entry.gitAt) > changedPathsTTL {
			entry.session.changedPaths = changedPathSet(fsutil.GitChangedFiles(repo))
			entry.gitAt = now
		}
		entry.mu.Unlock()
		return entry, nil
	}

	session, stableRevision, err := loadStableSession(ctx, repo)
	if err != nil {
		return nil, err
	}
	loadedAt := time.Now()
	entry = &pooledSession{
		session:  session,
		revision: stableRevision,
		gitAt:    loadedAt,
		sourceAt: loadedAt,
		lastUsed: loadedAt,
	}
	poolMu.Lock()
	pool[repo] = entry
	evictOldestSession(repo)
	poolMu.Unlock()
	return entry, nil
}

// pooledSourceGenerationFresh compares the complete indexable working tree
// with the checksums stored in the existing index without mutating either.
// OpenReadOnly deliberately rejects a pending WAL; that is also a reason not
// to reuse a cached session because a writer may have committed a generation
// newer than the main database file.
func pooledSourceGenerationFresh(repo string) bool {
	cfg, err := config.New(repo)
	if err != nil {
		return false
	}
	db, err := storage.OpenReadOnly(cfg.DBPath)
	if err != nil {
		return false
	}
	defer func() { _ = db.Close() }()

	stale, err := indexer.RequiresSourceReindex(cfg, db)
	return err == nil && !stale
}

type sessionLoader func(context.Context, string) (*Session, error)
type indexRevisionReader func(string) indexRevision

// loadStableSession retries when the SQLite generation changes while
// NewSession is loading its multi-table snapshot. Without the before/after
// check, an old or mixed snapshot can be labelled with the newest revision and
// remain cached until some unrelated future index write.
func loadStableSession(ctx context.Context, repo string) (*Session, indexRevision, error) {
	return loadStableSessionWith(ctx, repo, NewSession, currentIndexRevision)
}

func loadStableSessionWith(
	ctx context.Context,
	repo string,
	load sessionLoader,
	readRevision indexRevisionReader,
) (*Session, indexRevision, error) {
	for attempt := 0; attempt < maxStableSessionLoadAttempts; attempt++ {
		before := readRevision(repo)
		session, err := load(ctx, repo)
		if err != nil {
			return nil, indexRevision{}, err
		}
		after := readRevision(repo)
		if before == after {
			return session, after, nil
		}
	}
	return nil, indexRevision{}, fmt.Errorf(
		"retrieval: index changed during %d consecutive session loads",
		maxStableSessionLoadAttempts,
	)
}

// currentIndexRevision observes both the main database and its WAL. In WAL
// mode a committed scan/watch update can leave the main database's mtime
// unchanged, so looking only at the .db file serves stale sessions.
func currentIndexRevision(repo string) indexRevision {
	dbPath := filepath.Join(repo, config.DirName, config.DBName)
	return indexRevision{
		database: statRevision(dbPath),
		wal:      statRevision(dbPath + "-wal"),
	}
}

func statRevision(path string) fileRevision {
	fi, err := os.Stat(path)
	if err != nil {
		return fileRevision{}
	}
	return fileRevision{
		exists:          true,
		size:            fi.Size(),
		modTimeUnixNano: fi.ModTime().UnixNano(),
	}
}

// evictOldestSession bounds long-lived MCP processes that touch many repos.
// poolMu must be held by the caller.
func evictOldestSession(keepRepo string) {
	for len(pool) > maxPoolSessions {
		var oldestRepo string
		var oldest time.Time
		for repo, entry := range pool {
			if repo == keepRepo {
				continue
			}
			if oldestRepo == "" || entry.lastUsed.Before(oldest) {
				oldestRepo = repo
				oldest = entry.lastUsed
			}
		}
		if oldestRepo == "" {
			return
		}
		delete(pool, oldestRepo)
	}
}
