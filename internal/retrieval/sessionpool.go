package retrieval

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/neuromfs/neuromfs/internal/config"
	"github.com/neuromfs/neuromfs/internal/fsutil"
)

// A long-lived process (the MCP server above all — one per agent session,
// pinned to a repo) pays the full index load on every one-shot Search:
// AllFiles + AllChunks + a file read per chunk. SessionFor amortizes that
// by caching one Session per repo and rebuilding it only when the index
// database actually changes on disk (scan/watch bump its mtime). The
// working-set git signal is refreshed on a short TTL so recently edited
// files keep their ranking boost between scans.
//
// Staleness semantics are deliberate: between scans, cached file contents
// stay consistent with the indexed chunk line-ranges (fresh reads against
// a stale index would be no less wrong — see the gate's stale-index
// warning). A rescan invalidates the pool entry atomically via the mtime.
const changedPathsTTL = 30 * time.Second

type pooledSession struct {
	session   *Session
	dbModTime time.Time
	gitAt     time.Time
	mu        sync.Mutex
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
	mod := indexModTime(repo)

	poolMu.Lock()
	entry, ok := pool[repo]
	poolMu.Unlock()

	if ok && entry.dbModTime.Equal(mod) {
		entry.mu.Lock()
		if time.Since(entry.gitAt) > changedPathsTTL {
			entry.session.changedPaths = changedPathSet(fsutil.GitChangedFiles(repo))
			entry.gitAt = time.Now()
		}
		entry.mu.Unlock()
		return entry, nil
	}

	session, err := NewSession(ctx, repo)
	if err != nil {
		return nil, err
	}
	// NewSession may have just built the index (empty-index path); read the
	// mtime after, so the entry matches what the session actually loaded.
	entry = &pooledSession{
		session:   session,
		dbModTime: indexModTime(repo),
		gitAt:     time.Now(),
	}
	poolMu.Lock()
	pool[repo] = entry
	poolMu.Unlock()
	return entry, nil
}

// indexModTime returns the index database's mtime, or the zero time when
// it does not exist yet (NewSession will then create it via scan).
func indexModTime(repo string) time.Time {
	fi, err := os.Stat(filepath.Join(repo, config.DirName, config.DBName))
	if err != nil {
		return time.Time{}
	}
	return fi.ModTime()
}
