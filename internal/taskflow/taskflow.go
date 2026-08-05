// Package taskflow is the single source of truth for `neurofs task` —
// the "one intention → paste-ready prompt" flow. Both the CLI command
// and the web UI call Run here so their behaviour never drifts:
// same auto-scan policy, same cache key, same ranker settings, same
// Claude-shaped output.
//
// Nothing in this package touches stdin/stdout or HTTP; it returns a
// Result struct and lets the caller decide how to present it.
package taskflow

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Gere2/neurofs/internal/atomicfile"
	"github.com/Gere2/neurofs/internal/audit"
	"github.com/Gere2/neurofs/internal/config"
	"github.com/Gere2/neurofs/internal/embeddings"
	"github.com/Gere2/neurofs/internal/fsutil"
	"github.com/Gere2/neurofs/internal/indexer"
	"github.com/Gere2/neurofs/internal/models"
	"github.com/Gere2/neurofs/internal/output"
	"github.com/Gere2/neurofs/internal/packager"
	"github.com/Gere2/neurofs/internal/project"
	"github.com/Gere2/neurofs/internal/ranking"
	"github.com/Gere2/neurofs/internal/retrieval"
	"github.com/Gere2/neurofs/internal/runid"
	"github.com/Gere2/neurofs/internal/storage"
)

// Opts configures a Run call.
//
//   - RepoRoot is required; it is resolved through config.New so
//     relative paths work the same way as in the CLI.
//   - Query must be non-empty after trimming.
//   - Budget defaults to config.DefaultBudget when ≤ 0.
//   - Force bypasses the (query, budget) cache lookup.
//   - DisableChunks builds from ranked whole files instead of ranked code chunks.
//   - DisableIndexRefresh consumes an existing index read-only: no age,
//     version, embedding, source-generation, or empty-index rebuild is allowed.
//     Task prompt/bundle/manifest cache files may still be written.
type Opts struct {
	Context             context.Context
	RepoRoot            string
	Query               string
	Budget              int
	Force               bool
	DisableChunks       bool
	DisableIndexRefresh bool
	Ledger              LedgerWriter // Optional dependency injection; skips if nil
	Machine             bool         // New field!
}

// TopPick is the structured form of each line the CLI prints as
// "top[i] : path (tokens, rep)". UIs render it however they want.
type TopPick struct {
	RelPath        string  `json:"rel_path"`
	Tokens         int     `json:"tokens"`
	Representation string  `json:"representation"`
	Score          float64 `json:"score"`
}

// Result is everything a caller needs to present the outcome. Prompt
// is the full Claude-shaped text, ready to copy or pipe. Bundle is
// the raw packager output for replay/audit. Reused reports whether
// the cache served this call without re-ranking; AutoScanned reports
// whether we ran an implicit scan first.
type Result struct {
	Prompt      string             `json:"prompt"`
	PromptPath  string             `json:"prompt_path"`
	BundlePath  string             `json:"bundle_path"`
	JoinKey     *runid.JoinKey     `json:"join_key,omitempty"`
	Bundle      models.Bundle      `json:"-"`
	Stats       models.BundleStats `json:"stats"`
	Reused      bool               `json:"reused"`
	AutoScanned bool               `json:"auto_scanned"`
	TopPicks    []TopPick          `json:"top_picks"`
	Query       string             `json:"query"`
	Budget      int                `json:"budget"`
	RepoRoot    string             `json:"repo_root"`
	ChunkMode   bool               `json:"chunk_mode"`
}

// Run executes the full task flow: resolve config, auto-scan if the
// index is missing, consult the cache, regenerate if needed, and
// return the composed Result. Safe to call concurrently for different
// (RepoRoot, Query) pairs; per-repo concurrency is serialised by
// SQLite's file lock at the storage layer.
func Run(opts Opts) (Result, error) {
	ctx := opts.Context
	if ctx == nil {
		ctx = context.Background()
	}
	// Resolve correlation before any scan/cache write. A malformed ambient id
	// must fail closed rather than produce a partially mislabeled run.
	if _, err := runid.Current(ctx); err != nil {
		return Result{}, fmt.Errorf("taskflow: run identity: %w", err)
	}
	query := strings.TrimSpace(opts.Query)
	if query == "" {
		return Result{}, fmt.Errorf("taskflow: query must not be empty")
	}
	cfg, err := config.New(opts.RepoRoot)
	if err != nil {
		return Result{}, fmt.Errorf("taskflow: config: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return Result{}, fmt.Errorf("taskflow: config: %w", err)
	}
	if opts.Budget <= 0 {
		opts.Budget = cfg.Budget
	}

	autoScanned := false
	if !opts.DisableIndexRefresh && needsScan(cfg.DBPath) {
		autoScanned = true
		if err := autoScan(cfg); err != nil {
			return Result{}, fmt.Errorf("taskflow: auto-scan: %w", err)
		}
	}
	if opts.DisableChunks {
		refreshed, err := ensureNoChunkIndexGeneration(cfg, opts.DisableIndexRefresh)
		if err != nil {
			return Result{}, fmt.Errorf("taskflow: no-chunks index: %w", err)
		}
		autoScanned = autoScanned || refreshed
	}

	taskDir := filepath.Join(cfg.DBDir(), "task")
	if err := os.MkdirAll(taskDir, 0o755); err != nil {
		return Result{}, fmt.Errorf("taskflow: create cache dir: %w", err)
	}
	base := BaseName(query, opts.Budget)
	if !opts.DisableChunks {
		base = "chunks-" + base
	}
	if opts.Machine {
		base = "machine-" + base
	}
	promptPath := filepath.Join(taskDir, base+".prompt.txt")
	bundlePath := filepath.Join(taskDir, base+".bundle.json")
	manifestPath := filepath.Join(taskDir, base+".manifest.json")
	revision, err := cacheRevision(cfg, !opts.DisableChunks, opts.Machine)
	if err != nil {
		return Result{}, fmt.Errorf("taskflow: cache revision: %w", err)
	}

	reused := false
	if !opts.Force && isCacheFreshWithManifest(promptPath, bundlePath, manifestPath, revision) {
		reused = true
	} else {
		if err := generate(
			ctx,
			cfg,
			query,
			opts.Budget,
			!opts.DisableChunks,
			opts.Machine,
			opts.DisableIndexRefresh,
			promptPath,
			bundlePath,
			manifestPath,
			revision,
		); err != nil {
			return Result{}, fmt.Errorf("taskflow: %w", err)
		}
	}

	promptBytes, _, err := fsutil.ReadRegularFileBounded(promptPath, maxTaskArtifactBytes)
	if err != nil {
		return Result{}, fmt.Errorf("taskflow: read prompt: %w", err)
	}
	bundle, err := readBundleJSON(bundlePath)
	if err != nil {
		return Result{}, fmt.Errorf("taskflow: read bundle: %w", err)
	}
	resultBundlePath := bundlePath
	var joinKey *runid.JoinKey
	if key, snapshotPath, available, err := PersistRunBundle(ctx, cfg.RepoRoot, bundle); err != nil {
		return Result{}, fmt.Errorf("taskflow: persist run bundle: %w", err)
	} else if available {
		resultBundlePath = snapshotPath
		joinKey = &key
	}

	// Log run to memory ledger
	if opts.Ledger != nil {
		files := make([]string, len(bundle.Fragments))
		for i, f := range bundle.Fragments {
			files[i] = f.RelPath
		}
		notes := "Auto-logged from taskflow run (fresh generation)"
		if reused {
			notes = "Auto-logged from taskflow run (cache reused)"
		}
		logTimestamp := time.Now().UTC()
		entry := models.LedgerEntry{
			Timestamp:  logTimestamp,
			Query:      query,
			BundleHash: bundle.BundleHash,
			Files:      files,
			Notes:      notes,
		}
		if joinKey != nil {
			entry.BundlePath = joinKey.BundlePath
		}
		_ = opts.Ledger.AppendEntry(ctx, entry)
	}

	return Result{
		Prompt:      string(promptBytes),
		PromptPath:  promptPath,
		BundlePath:  resultBundlePath,
		JoinKey:     joinKey,
		Bundle:      bundle,
		Stats:       bundle.Stats,
		Reused:      reused,
		AutoScanned: autoScanned,
		TopPicks:    TopPicks(bundle, 5),
		Query:       query,
		Budget:      opts.Budget,
		RepoRoot:    cfg.RepoRoot,
		ChunkMode:   !opts.DisableChunks,
	}, nil
}

// needsScan returns true when there is no index file yet, or the file
// exists but is empty, or the index file is older than 24 hours.
func needsScan(dbPath string) bool {
	info, err := os.Stat(dbPath)
	if err != nil {
		return true
	}
	if info.Size() == 0 {
		return true
	}
	// Trigger scan if index is older than 24 hours
	if time.Since(info.ModTime()) > 24*time.Hour {
		return true
	}
	return false
}

// EnsureFreshIndex checks if the index database is missing, empty, or older
// than 24 hours. If it is, it triggers an incremental scan inline.
func EnsureFreshIndex(cfg *config.Config) error {
	if cfg == nil {
		return fmt.Errorf("taskflow: config is required")
	}
	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("taskflow: config: %w", err)
	}
	if needsScan(cfg.DBPath) {
		if err := autoScan(cfg); err != nil {
			return err
		}
		now := time.Now()
		if err := os.Chtimes(cfg.DBPath, now, now); err != nil {
			return fmt.Errorf("taskflow: refresh index timestamp: %w", err)
		}
	}
	return nil
}

// autoScan runs the indexer inline so task is usable in a fresh repo.
// The per-file log stream is swallowed; completion is implied by a
// nil return.
func autoScan(cfg *config.Config) (retErr error) {
	db, err := storage.Open(cfg.DBPath)
	if err != nil {
		return err
	}
	defer func() {
		if err := db.Close(); err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("close index: %w", err))
		}
	}()
	_, retErr = indexer.Run(cfg, db, indexer.Options{Logf: func(string, ...any) {}})
	return retErr
}

// ensureNoChunkIndexGeneration gives whole-file taskflow the same generation
// guarantees as chunk retrieval. Chunk mode delegates these checks to
// retrieval.NewSession; whole-file mode opens storage directly and otherwise
// has no opportunity to notice parser-version, embedding-space, or source
// drift.
//
// When index refresh is disabled, the function opens SQLite immutably and
// rejects stale state before task cache files are created. This preserves the
// measurement contract: callers either consume the exact current indexed
// generation or receive an error, and never repair evidence in place.
func ensureNoChunkIndexGeneration(
	cfg *config.Config,
	disableIndexRefresh bool,
) (refreshed bool, retErr error) {
	var (
		db  *storage.DB
		err error
	)
	if disableIndexRefresh {
		db, err = storage.OpenReadOnly(cfg.DBPath)
	} else {
		db, err = storage.Open(cfg.DBPath)
	}
	if err != nil {
		return false, err
	}
	defer func() {
		if err := db.Close(); err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("close index: %w", err))
		}
	}()

	requiresRefresh, err := indexer.RequiresReindex(db)
	if err != nil {
		return false, fmt.Errorf("check indexer version: %w", err)
	}
	client := embeddings.NewClient(cfg.HybridMode)
	if err := client.Validate(); err != nil {
		return false, fmt.Errorf("validate embedding configuration: %w", err)
	}
	if !requiresRefresh {
		requiresRefresh, err = indexer.RequiresEmbeddingReindex(
			db,
			client.ProviderName(),
			client.ModelName(),
		)
		if err != nil {
			return false, fmt.Errorf("check embedding index freshness: %w", err)
		}
	}
	if !requiresRefresh {
		requiresRefresh, err = indexer.RequiresSourceReindex(cfg, db)
		if err != nil {
			return false, fmt.Errorf("check source index freshness: %w", err)
		}
	}
	if !requiresRefresh {
		return false, nil
	}
	if disableIndexRefresh {
		return false, fmt.Errorf("index is stale and automatic refresh is disabled")
	}
	if _, err := indexer.Run(cfg, db, indexer.Options{Logf: func(string, ...any) {}}); err != nil {
		return false, fmt.Errorf("rebuild stale index: %w", err)
	}
	return true, nil
}

// BaseName produces a deterministic, filesystem-safe base for cache
// files. The 16-hex prefix identifies (budget, query) with enough collision
// resistance for long-lived caches; the
// slug tail is purely for human recognition when browsing
// .neurofs/task/.
func BaseName(query string, budget int) string {
	h := sha256.Sum256([]byte(fmt.Sprintf("%d|%s", budget, query)))
	return hex.EncodeToString(h[:])[:16] + "-" + Slugify(query)
}

var slugNonAlnum = regexp.MustCompile(`[^a-z0-9]+`)

// Slugify collapses non-alnum runs into single hyphens, caps at 40
// chars, and falls back to "task" when the input empties out. It is
// exported so tests and other callers can reproduce filenames.
func Slugify(s string) string {
	s = strings.ToLower(s)
	s = slugNonAlnum.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	if len(s) > 40 {
		s = strings.TrimRight(s[:40], "-")
	}
	if s == "" {
		s = "task"
	}
	return s
}

// IsCacheFresh is the legacy timestamp check retained for callers outside Run.
// It observes the WAL as well as the main database; Run additionally validates
// a revision manifest and content hashes.
func IsCacheFresh(dbPath, promptPath, bundlePath string) bool {
	indexTime, ok := newestIndexModTime(dbPath)
	if !ok {
		return false
	}
	for _, p := range []string{promptPath, bundlePath} {
		st, err := os.Stat(p)
		if err != nil || st.Size() == 0 {
			return false
		}
		if st.ModTime().Before(indexTime) {
			return false
		}
	}
	return true
}

const (
	cacheManifestVersion              = 4
	renderedPromptHashAlgorithm       = "sha256:neurofs-claude-prompt-v1"
	maxTaskArtifactBytes        int64 = 64 << 20
	maxCacheMetadataBytes       int64 = 1 << 20
)

type cacheManifest struct {
	Version      int    `json:"version"`
	Revision     string `json:"revision"`
	PromptSHA256 string `json:"prompt_sha256"`
	BundleSHA256 string `json:"bundle_sha256"`
	BundleHash   string `json:"bundle_hash"`
}

func newestIndexModTime(dbPath string) (time.Time, bool) {
	var newest time.Time
	found := false
	for _, path := range []string{dbPath, dbPath + "-wal", dbPath + "-shm"} {
		info, err := os.Stat(path)
		if err != nil {
			continue
		}
		found = true
		if info.ModTime().After(newest) {
			newest = info.ModTime()
		}
	}
	return newest, found
}

// cacheRevision fingerprints every mutable input that can change ranking or
// prompt rendering without changing the query: SQLite/WAL state, per-repo
// configuration and weights, the complete content of every visible modified
// or untracked worktree file, output mode and the cache schema version.
func cacheRevision(cfg *config.Config, useChunks, machine bool) (string, error) {
	h := sha256.New()
	if _, err := fmt.Fprintf(h, "task-cache-v%d|chunks=%t|machine=%t|go=%s|binary=%s\n",
		cacheManifestVersion, useChunks, machine, runtime.Version(), executableIdentity()); err != nil {
		return "", fmt.Errorf("write header: %w", err)
	}

	for _, path := range []string{cfg.DBPath, cfg.DBPath + "-wal", cfg.DBPath + "-shm"} {
		info, err := os.Stat(path)
		if err != nil {
			if _, err := fmt.Fprintf(h, "%s|missing\n", filepath.Base(path)); err != nil {
				return "", fmt.Errorf("write missing index component: %w", err)
			}
			continue
		}
		if _, err := fmt.Fprintf(h, "%s|%d|%d\n", filepath.Base(path), info.Size(), info.ModTime().UnixNano()); err != nil {
			return "", fmt.Errorf("write index component: %w", err)
		}
	}

	for _, path := range []string{
		filepath.Join(cfg.DBDir(), "config.json"),
		ranking.WeightsPath(cfg.RepoRoot),
		retrieval.WeightsPath(cfg.RepoRoot),
	} {
		if _, err := fmt.Fprintf(h, "%s\x00", filepath.Base(path)); err != nil {
			return "", fmt.Errorf("write mutable input name: %w", err)
		}
		data, _, err := fsutil.ReadRegularFileBounded(path, maxCacheMetadataBytes)
		if err == nil {
			h.Write(data)
		} else if os.IsNotExist(err) {
			h.Write([]byte("<missing>"))
		} else {
			return "", fmt.Errorf("read mutable input %s: %w", filepath.Base(path), err)
		}
		h.Write([]byte{0})
	}
	if err := writeVisibleWorktreeFingerprint(h, cfg.RepoRoot); err != nil {
		return "", fmt.Errorf("fingerprint working tree: %w", err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

type worktreeFingerprintEntry struct {
	status string
	rel    string
}

// writeVisibleWorktreeFingerprint hashes cache state independently from the
// deliberately truncated GitStatus/GitDiff strings embedded in prompts. Status
// supplies the complete changed/untracked path set; every allowed regular file
// is then streamed in full, so edits with an unchanged porcelain status or
// beyond the prompt's diff prefix still invalidate the cache.
func writeVisibleWorktreeFingerprint(dst io.Writer, repoRoot string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "git", "-C", repoRoot,
		"status", "--porcelain=v1", "-z", "--untracked-files=all", "--no-renames")
	raw, err := cmd.Output()
	if err != nil {
		if !isGitWorktree(repoRoot) {
			_, writeErr := io.WriteString(dst, "<not-a-git-worktree>\n")
			return writeErr
		}
		return fmt.Errorf("git status: %w", err)
	}

	matcher := fsutil.LoadIgnoreMatcher(repoRoot)
	entries := make([]worktreeFingerprintEntry, 0)
	for _, record := range bytes.Split(raw, []byte{0}) {
		if len(record) < 4 {
			continue
		}
		rel := string(record[3:])
		if !gitDiffPathAllowed(repoRoot, rel, matcher) {
			continue
		}
		entries = append(entries, worktreeFingerprintEntry{
			status: string(record[:2]),
			rel:    filepath.ToSlash(filepath.Clean(rel)),
		})
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].rel == entries[j].rel {
			return entries[i].status < entries[j].status
		}
		return entries[i].rel < entries[j].rel
	})

	for _, entry := range entries {
		if _, err := fmt.Fprintf(dst, "entry\x00%s\x00%s\x00", entry.status, entry.rel); err != nil {
			return fmt.Errorf("write path identity: %w", err)
		}
		if err := writeWorktreePathContent(dst, repoRoot, entry.rel); err != nil {
			return fmt.Errorf("%s: %w", entry.rel, err)
		}
		if _, err := io.WriteString(dst, "\x00"); err != nil {
			return fmt.Errorf("write path separator: %w", err)
		}
	}
	return nil
}

func isGitWorktree(repoRoot string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "-C", repoRoot, "rev-parse", "--is-inside-work-tree")
	out, err := cmd.Output()
	return err == nil && strings.TrimSpace(string(out)) == "true"
}

func writeWorktreePathContent(dst io.Writer, repoRoot, rel string) error {
	path := filepath.Join(repoRoot, filepath.FromSlash(rel))
	before, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			_, writeErr := io.WriteString(dst, "missing")
			return writeErr
		}
		return err
	}

	if before.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink(path)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(dst, "symlink\x00%s", target)
		return err
	}
	if !before.Mode().IsRegular() {
		_, err := fmt.Fprintf(dst, "non-regular\x00%s", before.Mode().String())
		return err
	}

	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()

	openedBefore, err := file.Stat()
	if err != nil {
		return err
	}
	if !openedBefore.Mode().IsRegular() || !os.SameFile(before, openedBefore) {
		return fsutil.ErrFileChanged
	}
	if _, err := fmt.Fprintf(dst, "regular\x00%d\x00%o\x00", openedBefore.Size(), openedBefore.Mode().Perm()); err != nil {
		return err
	}
	if _, err := io.Copy(dst, file); err != nil {
		return err
	}

	openedAfter, err := file.Stat()
	if err != nil {
		return err
	}
	afterPath, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !openedAfter.Mode().IsRegular() ||
		openedBefore.Size() != openedAfter.Size() ||
		openedBefore.ModTime() != openedAfter.ModTime() ||
		afterPath.Mode()&os.ModeSymlink != 0 ||
		!afterPath.Mode().IsRegular() ||
		!os.SameFile(openedAfter, afterPath) {
		return fsutil.ErrFileChanged
	}
	return nil
}

var (
	executableIdentityOnce sync.Once
	executableIdentityHash string
)

// executableIdentity makes normal task caches follow the code that produced
// them. Query, index, weights and git state can all remain unchanged across a
// NeuroFS upgrade; without the binary identity, that upgrade would reuse a
// bundle rendered by older retrieval/chunking semantics.
func executableIdentity() string {
	executableIdentityOnce.Do(func() {
		path, err := os.Executable()
		if err != nil {
			executableIdentityHash = "unknown"
			return
		}
		f, err := os.Open(path)
		if err != nil {
			executableIdentityHash = "unknown"
			return
		}
		defer func() { _ = f.Close() }()
		h := sha256.New()
		if _, err := io.Copy(h, f); err != nil {
			executableIdentityHash = "unknown"
			return
		}
		executableIdentityHash = hex.EncodeToString(h.Sum(nil))
	})
	return executableIdentityHash
}

func isCacheFreshWithManifest(promptPath, bundlePath, manifestPath, revision string) bool {
	manifestBytes, _, err := fsutil.ReadRegularFileBounded(manifestPath, maxCacheMetadataBytes)
	if err != nil {
		return false
	}
	var manifest cacheManifest
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil ||
		manifest.Version != cacheManifestVersion ||
		manifest.Revision != revision {
		return false
	}
	promptBytes, _, err := fsutil.ReadRegularFileBounded(promptPath, maxTaskArtifactBytes)
	if err != nil || len(promptBytes) == 0 || sha256Hex(promptBytes) != manifest.PromptSHA256 {
		return false
	}
	bundleBytes, _, err := fsutil.ReadRegularFileBounded(bundlePath, maxTaskArtifactBytes)
	if err != nil || len(bundleBytes) == 0 || sha256Hex(bundleBytes) != manifest.BundleSHA256 {
		return false
	}
	var bundle models.Bundle
	if err := json.Unmarshal(bundleBytes, &bundle); err != nil ||
		bundle.BundleHash == "" ||
		bundle.HashAlgorithm != audit.BundleHashAlgorithm ||
		bundle.RenderedPromptHash != manifest.PromptSHA256 ||
		bundle.RenderedPromptHashAlgorithm != renderedPromptHashAlgorithm ||
		bundle.BundleHash != manifest.BundleHash ||
		bundle.BundleHash != audit.BundleHash(bundle) {
		return false
	}
	return true
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// generate runs rank → pack → write-claude and persists both the
// prompt and the bundle JSON. We write to disk first, then the
// caller re-reads from the file so cache-hit and cache-miss paths
// share the same return values.
func generate(
	ctx context.Context,
	cfg *config.Config,
	query string,
	budget int,
	useChunks bool,
	useMachine bool,
	disableIndexRefresh bool,
	promptPath, bundlePath, manifestPath, revision string,
) error {
	var (
		db  *storage.DB
		err error
	)
	if disableIndexRefresh {
		db, err = storage.OpenReadOnly(cfg.DBPath)
	} else {
		db, err = storage.Open(cfg.DBPath)
	}
	if err != nil {
		return fmt.Errorf("open index: %w", err)
	}
	closed := false
	defer func() {
		if !closed {
			_ = db.Close()
		}
	}()

	count, err := db.FileCount()
	if err != nil {
		return err
	}
	if count == 0 {
		return fmt.Errorf("index is empty after scan — nothing to rank")
	}

	files, err := db.AllFiles()
	if err != nil {
		return fmt.Errorf("load index: %w", err)
	}
	info := LoadProjectInfo(db)

	var bundle models.Bundle
	if useChunks {
		if err := db.Close(); err != nil {
			return fmt.Errorf("close index before chunk search: %w", err)
		}
		closed = true
		// Limit 24 (not 12): with method-sized chunks the token budget, not
		// the hit count, should decide where the bundle stops — PackChunks
		// trims greedily by budget, so extra candidates cost nothing when
		// the budget is already full and recover facts when it is not.
		searchRes, err := retrieval.Search(ctx, retrieval.Options{
			Query:                   query,
			Repo:                    cfg.RepoRoot,
			Limit:                   24,
			Mode:                    "task",
			DisableIndexRefresh:     disableIndexRefresh,
			ExpandStructuralContext: true,
		})
		if err != nil {
			return fmt.Errorf("chunk search: %w", err)
		}
		bundle, err = packager.PackChunks(chunkHitsFromRetrieval(searchRes, files), query, packager.Options{
			Budget:           budget,
			MaxFragments:     28,
			PreferSignatures: true,
			UpgradeWithSlack: true,
		})
		if err != nil {
			return fmt.Errorf("pack chunks: %w", err)
		}
	} else {
		embClient := embeddings.NewClient(cfg.HybridMode)
		queryEmb, _ := embClient.GetEmbedding(ctx, query)
		fileEmbs, _ := db.AllEmbeddings()
		rels, _ := db.AllRelations()

		rankWeights, _, _ := ranking.LoadWeights(cfg.RepoRoot)
		ranked := ranking.RankWithOptions(files, query, ranking.Options{
			Project:        info,
			QueryEmbedding: queryEmb,
			Embeddings:     fileEmbs,
			Relations:      rels,
			Weights:        &rankWeights,
		})
		bundle, err = packager.Pack(ranked, query, packager.Options{
			Budget:           budget,
			PreferSignatures: true,
			UpgradeWithSlack: true,
			// Same terms the ranker used; lets the packager extract just the
			// symbol bodies the query is actually asking about for the top
			// few files instead of forcing all-or-nothing per file.
			QueryTerms: ranking.Tokenise(query),
		})
		if err != nil {
			return fmt.Errorf("pack: %w", err)
		}
	}

	var prompt bytes.Buffer
	if err := output.WriteClaudeWithOptions(&prompt, bundle, BuildRepoSummary(cfg.RepoRoot, files, info), output.Options{Machine: useMachine}); err != nil {
		return fmt.Errorf("write prompt: %w", err)
	}

	bundle = EnrichBundle(bundle, cfg.RepoRoot)
	bundle.RenderedPromptHash = sha256Hex(prompt.Bytes())
	bundle.RenderedPromptHashAlgorithm = renderedPromptHashAlgorithm
	if err := atomicfile.WriteFile(promptPath, prompt.Bytes(), 0o644); err != nil {
		return fmt.Errorf("save prompt: %w", err)
	}
	if err := WriteBundleJSON(bundlePath, bundle); err != nil {
		return fmt.Errorf("save bundle: %w", err)
	}
	bundleBytes, _, err := fsutil.ReadRegularFileBounded(bundlePath, maxTaskArtifactBytes)
	if err != nil {
		return fmt.Errorf("read saved bundle: %w", err)
	}
	manifest := cacheManifest{
		Version:      cacheManifestVersion,
		Revision:     revision,
		PromptSHA256: sha256Hex(prompt.Bytes()),
		BundleSHA256: sha256Hex(bundleBytes),
		BundleHash:   bundle.BundleHash,
	}
	manifestBytes, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal cache manifest: %w", err)
	}
	if err := atomicfile.WriteFile(manifestPath, append(manifestBytes, '\n'), 0o644); err != nil {
		return fmt.Errorf("save cache manifest: %w", err)
	}
	return nil
}

func chunkHitsFromRetrieval(searchRes retrieval.Response, files []models.FileRecord) []packager.ChunkHit {
	langByPath := make(map[string]models.Lang, len(files))
	for _, file := range files {
		langByPath[file.RelPath] = file.Lang
	}
	hits := make([]packager.ChunkHit, 0, len(searchRes.Results))
	for _, hit := range searchRes.Results {
		hits = append(hits, packager.ChunkHit{
			RelPath:       hit.Path,
			Lang:          langByPath[hit.Path],
			StartLine:     hit.StartLine,
			EndLine:       hit.EndLine,
			Kind:          hit.Kind,
			Symbol:        hit.Symbol,
			Score:         hit.Score,
			Reasons:       hit.Reasons,
			TokenEstimate: hit.TokenEstimate,
			ContentHash:   hit.ContentHash,
			Snippet:       hit.Snippet,
		})
	}
	return hits
}

// LoadProjectInfo reads project.Info from the metadata table populated by
// `scan`. Returns nil when absent or invalid — callers treat nil as "no
// project metadata available". Exported so cli/ and ui/ share one copy.
func LoadProjectInfo(db *storage.DB) *project.Info {
	raw, ok, err := db.GetMeta(indexer.ProjectMetaKey)
	if err != nil || !ok {
		return nil
	}
	return project.Decode(raw)
}

// GitDiff returns the working-tree diff for source files that NeuroFS is
// allowed to inspect. It deliberately obtains the changed path list first and
// applies the same secret/ignore rules as scanning before asking git for any
// content, so a tracked .env, private key, or ignored credential file cannot
// leak into every generated prompt.
//
// The result is capped at 6000 characters to prevent budget exhaustion.
// Returns "" on error, when all changed files are sensitive, or when clean.
func GitDiff(repoRoot string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	namesCmd := exec.CommandContext(ctx, "git", "-C", repoRoot, "diff", "--name-only", "-z", "HEAD")
	names, err := namesCmd.Output()
	if err != nil {
		return ""
	}

	matcher := fsutil.LoadIgnoreMatcher(repoRoot)
	var paths []string
	for _, raw := range bytes.Split(names, []byte{0}) {
		if len(raw) == 0 {
			continue
		}
		rel := string(raw)
		if gitDiffPathAllowed(repoRoot, rel, matcher) {
			paths = append(paths, rel)
		}
	}
	if len(paths) == 0 {
		return ""
	}

	args := []string{"-C", repoRoot, "diff", "HEAD", "--"}
	args = append(args, paths...)
	cmd := exec.CommandContext(ctx, "git", args...)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &bytes.Buffer{}

	if err := cmd.Run(); err != nil {
		return ""
	}
	res := out.String()
	if len(res) > 6000 {
		return res[:6000] + "\n\n[... git diff truncated to 6000 chars due to budget constraint ...]"
	}
	return res
}

func gitDiffPathAllowed(repoRoot, rel string, matcher *fsutil.IgnoreMatcher) bool {
	clean := filepath.Clean(rel)
	if clean == "." || filepath.IsAbs(clean) || clean == ".." ||
		strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return false
	}
	slashPath := filepath.ToSlash(clean)
	if matcher.Match(slashPath, false) || fsutil.ShouldSkipFile(clean) {
		return false
	}

	for dir := filepath.Dir(clean); dir != "."; dir = filepath.Dir(dir) {
		slashDir := filepath.ToSlash(dir)
		if matcher.Match(slashDir, true) ||
			fsutil.ShouldSkipDirAt(repoRoot, filepath.Join(repoRoot, dir)) {
			return false
		}
	}
	return true
}

// GitStatus returns a filtered porcelain status. It applies the same
// repository visibility rules as GitDiff so even sensitive filenames are not
// copied into generated prompts.
func GitStatus(repoRoot string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "git", "-C", repoRoot,
		"status", "--porcelain=v1", "-z", "--untracked-files=all", "--no-renames")
	raw, err := cmd.Output()
	if err != nil {
		return ""
	}
	matcher := fsutil.LoadIgnoreMatcher(repoRoot)
	var filtered strings.Builder
	for _, record := range bytes.Split(raw, []byte{0}) {
		if len(record) < 4 {
			continue
		}
		status := string(record[:2])
		rel := string(record[3:])
		if !gitDiffPathAllowed(repoRoot, rel, matcher) {
			continue
		}
		fmt.Fprintf(&filtered, "%s %s\n", status, filepath.ToSlash(rel))
	}
	res := filtered.String()
	if len(res) > 2000 {
		return res[:2000] + "\n\n[... git status truncated to 2000 chars due to budget constraint ...]"
	}
	return res
}

// BuildRepoSummary assembles the repo orientation block carried in every
// Claude prompt. Exported so cli/ and ui/ share one canonical implementation;
// callers that already depend on taskflow (cli/pack, ui/api, ui/proxy) use
// this directly instead of duplicating it.
func BuildRepoSummary(repoRoot string, files []models.FileRecord, info *project.Info) output.RepoSummary {
	langs := make(map[string]int, 8)
	symbols := 0
	for _, f := range files {
		langs[string(f.Lang)]++
		symbols += len(f.Symbols)
	}
	s := output.RepoSummary{
		Files:     len(files),
		Symbols:   symbols,
		Languages: langs,
		GitDiff:   GitDiff(repoRoot),
		GitStatus: GitStatus(repoRoot),
	}
	if info != nil {
		s.Name = info.Label()
		if entries := info.EntryPoints(); len(entries) > 0 {
			s.Entry = filepath.ToSlash(entries[0])
		}
	}
	return s
}

// WriteBundleJSON serialises a bundle to disk as indented JSON. Exported so
// cli/ and ui/ share one copy.
func WriteBundleJSON(path string, b models.Bundle) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		return err
	}
	return atomicfile.WriteFile(path, data, 0o644)
}

// EnrichBundle populates the audit-identity fields a compliance consumer
// needs: repo path, current git commit (empty when not a git worktree),
// generation timestamp, and a content hash that matches the audit-replay
// record. Callers invoke this immediately before WriteBundleJSON.
//
// The hash deliberately does NOT cover GeneratedAt — otherwise two
// bundles built from identical content would hash differently and the
// hash would lose its "same context" meaning.
func EnrichBundle(b models.Bundle, repoRoot string) models.Bundle {
	abs, err := filepath.Abs(repoRoot)
	if err == nil {
		b.Repo = abs
	} else {
		b.Repo = repoRoot
	}
	b.CommitSHA = currentCommitSHA(repoRoot)
	b.GeneratedAt = time.Now().UTC()
	b.BundleHash = audit.BundleHash(b)
	b.HashAlgorithm = audit.BundleHashAlgorithm
	return b
}

// currentCommitSHA returns the short HEAD SHA via the local git binary,
// or "" when the directory is not a git worktree or git is unavailable.
// The bundle is meant to identify the snapshot of code that produced it,
// so "no git" is a real and acceptable state — not an error.
func currentCommitSHA(repoRoot string) string {
	cmd := exec.Command("git", "-C", repoRoot, "rev-parse", "HEAD")
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// readBundleJSON parses a persisted bundle. Used on cache hits so the
// summary matches a regenerated run byte-for-byte.
func readBundleJSON(path string) (models.Bundle, error) {
	data, _, err := fsutil.ReadRegularFileBounded(path, maxTaskArtifactBytes)
	if err != nil {
		return models.Bundle{}, err
	}
	var b models.Bundle
	if err := json.Unmarshal(data, &b); err != nil {
		return models.Bundle{}, err
	}
	return b, nil
}

// TopPicks returns up to n highest-scoring fragments as structured
// records. Fragments arrive in score order from the packager.
func TopPicks(b models.Bundle, n int) []TopPick {
	if n <= 0 || len(b.Fragments) == 0 {
		return nil
	}
	if n > len(b.Fragments) {
		n = len(b.Fragments)
	}
	out := make([]TopPick, 0, n)
	for i := 0; i < n; i++ {
		f := b.Fragments[i]
		out = append(out, TopPick{
			RelPath:        f.RelPath,
			Tokens:         f.Tokens,
			Representation: string(f.Representation),
			Score:          f.Score,
		})
	}
	return out
}

// Clipboard copies payload to the host clipboard via the best
// available helper for the OS (pbcopy / wl-copy / xclip / xsel /
// clip). Returns a short status string. Strictly best-effort — a
// missing helper is cosmetic, not fatal, because the prompt is
// always on disk.
func Clipboard(payload []byte) string {
	for _, argv := range clipboardCommands() {
		if _, err := exec.LookPath(argv[0]); err != nil {
			continue
		}
		cmd := exec.Command(argv[0], argv[1:]...)
		cmd.Stdin = strings.NewReader(string(payload))
		if err := cmd.Run(); err == nil {
			return "copied via " + argv[0]
		}
	}
	return "unavailable (no clipboard tool found)"
}

func clipboardCommands() [][]string {
	switch runtime.GOOS {
	case "darwin":
		return [][]string{{"pbcopy"}}
	case "windows":
		return [][]string{{"clip"}}
	default:
		return [][]string{
			{"wl-copy"},
			{"xclip", "-selection", "clipboard"},
			{"xsel", "--clipboard", "--input"},
		}
	}
}
