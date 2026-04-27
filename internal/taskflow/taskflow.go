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
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	"github.com/neuromfs/neuromfs/internal/config"
	"github.com/neuromfs/neuromfs/internal/indexer"
	"github.com/neuromfs/neuromfs/internal/models"
	"github.com/neuromfs/neuromfs/internal/output"
	"github.com/neuromfs/neuromfs/internal/packager"
	"github.com/neuromfs/neuromfs/internal/project"
	"github.com/neuromfs/neuromfs/internal/ranking"
	"github.com/neuromfs/neuromfs/internal/storage"
)

// Opts configures a Run call.
//
//   - RepoRoot is required; it is resolved through config.New so
//     relative paths work the same way as in the CLI.
//   - Query must be non-empty after trimming.
//   - Budget defaults to config.DefaultBudget when ≤ 0.
//   - Force bypasses the (query, budget) cache lookup.
type Opts struct {
	RepoRoot string
	Query    string
	Budget   int
	Force    bool
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
	Bundle      models.Bundle      `json:"-"`
	Stats       models.BundleStats `json:"stats"`
	Reused      bool               `json:"reused"`
	AutoScanned bool               `json:"auto_scanned"`
	TopPicks    []TopPick          `json:"top_picks"`
	Query       string             `json:"query"`
	Budget      int                `json:"budget"`
	RepoRoot    string             `json:"repo_root"`
}

// Run executes the full task flow: resolve config, auto-scan if the
// index is missing, consult the cache, regenerate if needed, and
// return the composed Result. Safe to call concurrently for different
// (RepoRoot, Query) pairs; per-repo concurrency is serialised by
// SQLite's file lock at the storage layer.
func Run(opts Opts) (Result, error) {
	query := strings.TrimSpace(opts.Query)
	if query == "" {
		return Result{}, fmt.Errorf("taskflow: query must not be empty")
	}
	if opts.Budget <= 0 {
		opts.Budget = config.DefaultBudget
	}

	cfg, err := config.New(opts.RepoRoot)
	if err != nil {
		return Result{}, fmt.Errorf("taskflow: config: %w", err)
	}

	autoScanned := false
	if needsScan(cfg.DBPath) {
		autoScanned = true
		if err := autoScan(cfg); err != nil {
			return Result{}, fmt.Errorf("taskflow: auto-scan: %w", err)
		}
	}

	taskDir := filepath.Join(cfg.DBDir(), "task")
	if err := os.MkdirAll(taskDir, 0o755); err != nil {
		return Result{}, fmt.Errorf("taskflow: create cache dir: %w", err)
	}
	base := BaseName(query, opts.Budget)
	promptPath := filepath.Join(taskDir, base+".prompt.txt")
	bundlePath := filepath.Join(taskDir, base+".bundle.json")

	reused := false
	if !opts.Force && IsCacheFresh(cfg.DBPath, promptPath, bundlePath) {
		reused = true
	} else {
		if err := generate(cfg, query, opts.Budget, promptPath, bundlePath); err != nil {
			return Result{}, fmt.Errorf("taskflow: %w", err)
		}
	}

	promptBytes, err := os.ReadFile(promptPath)
	if err != nil {
		return Result{}, fmt.Errorf("taskflow: read prompt: %w", err)
	}
	bundle, err := readBundleJSON(bundlePath)
	if err != nil {
		return Result{}, fmt.Errorf("taskflow: read bundle: %w", err)
	}

	return Result{
		Prompt:      string(promptBytes),
		PromptPath:  promptPath,
		BundlePath:  bundlePath,
		Bundle:      bundle,
		Stats:       bundle.Stats,
		Reused:      reused,
		AutoScanned: autoScanned,
		TopPicks:    TopPicks(bundle, 5),
		Query:       query,
		Budget:      opts.Budget,
		RepoRoot:    cfg.RepoRoot,
	}, nil
}

// needsScan returns true when there is no index file yet, or the file
// exists but is empty. We do not peek inside — an existing non-empty
// DB is trusted and autoScan is idempotent anyway.
func needsScan(dbPath string) bool {
	info, err := os.Stat(dbPath)
	if err != nil {
		return true
	}
	return info.Size() == 0
}

// autoScan runs the indexer inline so task is usable in a fresh repo.
// The per-file log stream is swallowed; completion is implied by a
// nil return.
func autoScan(cfg *config.Config) error {
	db, err := storage.Open(cfg.DBPath)
	if err != nil {
		return err
	}
	defer db.Close()
	_, err = indexer.Run(cfg, db, indexer.Options{Logf: func(string, ...any) {}})
	return err
}

// BaseName produces a deterministic, filesystem-safe base for cache
// files. The 8-hex prefix uniquely identifies (budget, query); the
// slug tail is purely for human recognition when browsing
// .neurofs/task/.
func BaseName(query string, budget int) string {
	h := sha1.Sum([]byte(fmt.Sprintf("%d|%s", budget, query)))
	return hex.EncodeToString(h[:])[:8] + "-" + Slugify(query)
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

// IsCacheFresh reports whether both artefacts exist, are non-empty,
// and are at least as new as the index. Any successful scan bumps
// index.db's mtime and therefore invalidates every cached prompt —
// that is the whole invalidation rule.
func IsCacheFresh(dbPath, promptPath, bundlePath string) bool {
	dbInfo, err := os.Stat(dbPath)
	if err != nil {
		return false
	}
	for _, p := range []string{promptPath, bundlePath} {
		st, err := os.Stat(p)
		if err != nil || st.Size() == 0 {
			return false
		}
		if st.ModTime().Before(dbInfo.ModTime()) {
			return false
		}
	}
	return true
}

// generate runs rank → pack → write-claude and persists both the
// prompt and the bundle JSON. We write to disk first, then the
// caller re-reads from the file so cache-hit and cache-miss paths
// share the same return values.
func generate(cfg *config.Config, query string, budget int, promptPath, bundlePath string) error {
	db, err := storage.Open(cfg.DBPath)
	if err != nil {
		return fmt.Errorf("open index: %w", err)
	}
	defer db.Close()

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
	info := loadProjectInfo(db)

	ranked := ranking.RankWithOptions(files, query, ranking.Options{Project: info})
	bundle, err := packager.Pack(ranked, query, packager.Options{
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

	pf, err := os.Create(promptPath)
	if err != nil {
		return fmt.Errorf("create prompt: %w", err)
	}
	if err := output.WriteClaude(pf, bundle, buildRepoSummary(files, info)); err != nil {
		pf.Close()
		return fmt.Errorf("write prompt: %w", err)
	}
	if err := pf.Close(); err != nil {
		return fmt.Errorf("close prompt: %w", err)
	}

	if err := writeBundleJSON(bundlePath, bundle); err != nil {
		return fmt.Errorf("save bundle: %w", err)
	}
	return nil
}

// loadProjectInfo mirrors internal/cli/projectload.go — we duplicate
// this 4-line helper instead of exposing it from cli, because cli
// depends on taskflow and the other direction would create a cycle.
func loadProjectInfo(db *storage.DB) *project.Info {
	raw, ok, err := db.GetMeta(indexer.ProjectMetaKey)
	if err != nil || !ok {
		return nil
	}
	return project.Decode(raw)
}

// buildRepoSummary mirrors internal/cli/pack.go for the same
// reason as loadProjectInfo — avoiding a cli→taskflow import cycle.
// A few lines of dupe beats a shared "miscellany" package.
func buildRepoSummary(files []models.FileRecord, info *project.Info) output.RepoSummary {
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
	}
	if info != nil {
		s.Name = info.Label()
		if entries := info.EntryPoints(); len(entries) > 0 {
			s.Entry = filepath.ToSlash(entries[0])
		}
	}
	return s
}

// writeBundleJSON mirrors internal/cli/pack.go writeBundleJSON for
// the same avoid-cycle reason; keeping it here means taskflow.Run
// has no external dependencies on the cli package.
func writeBundleJSON(path string, b models.Bundle) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

// readBundleJSON parses a persisted bundle. Used on cache hits so the
// summary matches a regenerated run byte-for-byte.
func readBundleJSON(path string) (models.Bundle, error) {
	data, err := os.ReadFile(path)
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
