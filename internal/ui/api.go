package ui

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/neuromfs/neuromfs/internal/audit"
	"github.com/neuromfs/neuromfs/internal/config"
	"github.com/neuromfs/neuromfs/internal/indexer"
	"github.com/neuromfs/neuromfs/internal/models"
	"github.com/neuromfs/neuromfs/internal/output"
	"github.com/neuromfs/neuromfs/internal/packager"
	"github.com/neuromfs/neuromfs/internal/project"
	"github.com/neuromfs/neuromfs/internal/ranking"
	"github.com/neuromfs/neuromfs/internal/storage"
	"github.com/neuromfs/neuromfs/internal/taskflow"
)

// registerAPI wires every endpoint onto mux. The routes are flat and match
// the client's fetch() calls 1:1 — no versioning, no middleware layering,
// because the client and server ship in the same binary.
func registerAPI(mux *http.ServeMux) {
	mux.HandleFunc("/api/scan", postOnly(handleScan))
	mux.HandleFunc("/api/pack", postOnly(handlePack))
	mux.HandleFunc("/api/replay", postOnly(handleReplay))
	mux.HandleFunc("/api/records", getOnly(handleRecords))
	mux.HandleFunc("/api/record", getOnly(handleRecord))
	mux.HandleFunc("/api/search", getOnly(handleSearch))
	mux.HandleFunc("/api/diff", postOnly(handleDiff))
	mux.HandleFunc("/api/stats", getOnly(handleStats))
	mux.HandleFunc("/api/resume-seed", getOnly(handleResumeSeed))
	mux.HandleFunc("/api/task", postOnly(handleTask))
	mux.HandleFunc("/api/bootstrap", getOnly(handleBootstrap))
}

// --------------------- method gates ---------------------

func postOnly(fn http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeErr(w, http.StatusMethodNotAllowed, "POST required")
			return
		}
		fn(w, r)
	}
}

func getOnly(fn http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeErr(w, http.StatusMethodNotAllowed, "GET required")
			return
		}
		fn(w, r)
	}
}

// --------------------- helpers ---------------------

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// mustRepo validates the repo path and returns a Config. Empty or relative
// paths are rejected so we never accidentally operate against the server's
// own working directory — this is a local tool, but still a web surface.
func mustRepo(w http.ResponseWriter, repo string) (*config.Config, bool) {
	repo = strings.TrimSpace(repo)
	if repo == "" {
		writeErr(w, http.StatusBadRequest, "repo is required")
		return nil, false
	}
	if !filepath.IsAbs(repo) {
		writeErr(w, http.StatusBadRequest, "repo must be an absolute path")
		return nil, false
	}
	cfg, err := config.New(repo)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return nil, false
	}
	if err := cfg.Validate(); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return nil, false
	}
	return cfg, true
}

// decode reads JSON body into v with a generous size limit. Keeping the
// limit tight protects against accidentally pasting a multi-MB file into
// a textarea — replay responses are rarely more than ~200KB.
//
// The contract is "exactly one JSON object per body": DisallowUnknownFields
// rejects typos, and the post-Decode EOF check rejects trailing content so a
// client that smuggles a second object (or junk) after the payload cannot
// slip past a handler that only reads the first.
func decode(r *http.Request, v any) error {
	r.Body = http.MaxBytesReader(nil, r.Body, 8<<20) // 8 MB
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return fmt.Errorf("body must contain a single JSON object")
	}
	return nil
}

// confineToRepo resolves raw against root (joining if relative, otherwise
// using raw as-is) and guarantees the result lives inside root. Symlinks
// are followed before the containment check so an attacker cannot tunnel
// out via `audit/records/evil.json -> /etc/passwd`. The returned absolute
// path is safe to pass to os.Open / os.ReadFile / os.WriteFile.
//
// Both paths are canonicalised by resolving their deepest-existing
// ancestor — this matters on macOS where /var is a symlink to /private/var
// and asymmetric resolution would make perfectly valid paths fail the
// containment check. Missing leaves are acceptable (we may be writing).
func confineToRepo(root, raw string, _ bool) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("path is required")
	}
	abs := raw
	if !filepath.IsAbs(raw) {
		abs = filepath.Join(root, raw)
	}
	abs = filepath.Clean(abs)

	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("resolve repo root: %w", err)
	}
	rootResolved := resolveExistingPrefix(rootAbs)
	absResolved := resolveExistingPrefix(abs)

	rel, err := filepath.Rel(rootResolved, absResolved)
	if err != nil {
		return "", fmt.Errorf("resolve path: %w", err)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path must live inside the repo: %s", raw)
	}
	return absResolved, nil
}

// resolveExistingPrefix canonicalises the deepest existing prefix of p
// (following symlinks) and re-attaches the remaining non-existent tail
// verbatim. Never returns an error: if nothing resolves, the input is
// returned unchanged. This keeps confineToRepo usable for paths that
// are about to be created.
func resolveExistingPrefix(p string) string {
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		return resolved
	}
	parent := filepath.Dir(p)
	if parent == p {
		return p
	}
	return filepath.Join(resolveExistingPrefix(parent), filepath.Base(p))
}

// --------------------- /api/scan ---------------------

type scanReq struct {
	Repo    string `json:"repo"`
	Verbose bool   `json:"verbose"`
}
type scanResp struct {
	OK      bool             `json:"ok"`
	Summary map[string]any   `json:"summary"`
	Lang    map[string]int   `json:"lang,omitempty"`
	Errors  []string         `json:"errors,omitempty"`
}

func handleScan(w http.ResponseWriter, r *http.Request) {
	var req scanReq
	if err := decode(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad JSON: "+err.Error())
		return
	}
	cfg, ok := mustRepo(w, req.Repo)
	if !ok {
		return
	}

	db, err := storage.Open(cfg.DBPath)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "open db: "+err.Error())
		return
	}
	defer db.Close()

	start := time.Now()
	stats, err := indexer.Run(cfg, db, indexer.Options{})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "scan: "+err.Error())
		return
	}

	writeJSON(w, http.StatusOK, scanResp{
		OK: true,
		Summary: map[string]any{
			"discovered": stats.Discovered,
			"indexed":    stats.Indexed,
			"skipped":    stats.Skipped,
			"removed":    stats.Removed,
			"errors":     stats.Errors,
			"symbols":    stats.Symbols,
			"imports":    stats.Imports,
			"db_path":    cfg.DBPath,
			"elapsed_ms": time.Since(start).Milliseconds(),
		},
	})
}

// --------------------- /api/pack ---------------------

type packReq struct {
	Repo             string `json:"repo"`
	Query            string `json:"query"`
	Budget           int    `json:"budget"`
	Focus            string `json:"focus"`
	Changed          bool   `json:"changed"`
	MaxFiles         int    `json:"max_files"`
	MaxFragments     int    `json:"max_fragments"`
	PreferSignatures bool   `json:"prefer_signatures"`
	SnapshotName     string `json:"snapshot_name"` // optional — when empty, a default path under .neurofs/ui/ is used
	// InheritedFocus carries the focus paths the user kept from a parent
	// record. Merged with Focus server-side so the ranker sees one list and
	// legacy clients (no parent) remain unchanged.
	InheritedFocus []string `json:"inherited_focus"`
}

type packResp struct {
	Prompt     string               `json:"prompt"`
	Stats      models.BundleStats   `json:"stats"`
	BundlePath string               `json:"bundle_path,omitempty"`
	Fragments  []fragmentView       `json:"fragments"`
	Query      string               `json:"query"`
}

type fragmentView struct {
	RelPath        string  `json:"rel_path"`
	Representation string  `json:"representation"`
	Tokens         int     `json:"tokens"`
	Score          float64 `json:"score"`
}

func handlePack(w http.ResponseWriter, r *http.Request) {
	var req packReq
	if err := decode(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad JSON: "+err.Error())
		return
	}
	if strings.TrimSpace(req.Query) == "" {
		writeErr(w, http.StatusBadRequest, "query is required")
		return
	}
	cfg, ok := mustRepo(w, req.Repo)
	if !ok {
		return
	}
	if req.Budget <= 0 {
		req.Budget = config.DefaultBudget
	}

	db, err := storage.Open(cfg.DBPath)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "open index (did you run scan?): "+err.Error())
		return
	}
	defer db.Close()

	files, err := db.AllFiles()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "load files: "+err.Error())
		return
	}
	if len(files) == 0 {
		writeErr(w, http.StatusBadRequest, "index is empty — run scan first")
		return
	}

	rankOpts := ranking.Options{
		Project: loadProjectInfo(db),
		Focus:   mergeFocus(req.Focus, req.InheritedFocus),
	}
	if req.Changed {
		rankOpts.ChangedFiles = gitChangedFiles(cfg.RepoRoot)
	}
	ranked := ranking.RankWithOptions(files, req.Query, rankOpts)

	bundle, err := packager.Pack(ranked, req.Query, packager.Options{
		Budget:           req.Budget,
		MaxFiles:         req.MaxFiles,
		MaxFragments:     req.MaxFragments,
		PreferSignatures: req.PreferSignatures,
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "pack: "+err.Error())
		return
	}

	// Render the Claude-shaped prompt so the UI can let the user copy it
	// verbatim. Using the same output.WriteClaude path means what the UI
	// shows is exactly what the CLI would emit.
	var promptBuf bytes.Buffer
	if err := output.WriteClaude(&promptBuf, bundle, buildRepoSummary(files, loadProjectInfo(db))); err != nil {
		writeErr(w, http.StatusInternalServerError, "render prompt: "+err.Error())
		return
	}

	// Always persist the bundle to a snapshot so replay can audit it later,
	// even across page reloads. If the user supplied a name, we also copy
	// the snapshot there for long-term record-keeping.
	defaultPath := filepath.Join(cfg.DBDir(), "ui", "last.bundle.json")
	if err := writeBundleJSON(defaultPath, bundle); err != nil {
		writeErr(w, http.StatusInternalServerError, "save default bundle: "+err.Error())
		return
	}
	snapshotPath := defaultPath
	if name := strings.TrimSpace(req.SnapshotName); name != "" {
		target, err := confineToRepo(cfg.RepoRoot, name, true)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "snapshot_name: "+err.Error())
			return
		}
		if err := writeBundleJSON(target, bundle); err != nil {
			writeErr(w, http.StatusInternalServerError, "save snapshot: "+err.Error())
			return
		}
		snapshotPath = target
	}

	frags := make([]fragmentView, 0, len(bundle.Fragments))
	for _, f := range bundle.Fragments {
		frags = append(frags, fragmentView{
			RelPath:        f.RelPath,
			Representation: string(f.Representation),
			Tokens:         f.Tokens,
			Score:          f.Score,
		})
	}

	writeJSON(w, http.StatusOK, packResp{
		Prompt:     promptBuf.String(),
		Stats:      bundle.Stats,
		BundlePath: snapshotPath,
		Fragments:  frags,
		Query:      req.Query,
	})
}

// --------------------- /api/replay ---------------------

type replayReq struct {
	Repo       string `json:"repo"`
	BundlePath string `json:"bundle_path"`
	Response   string `json:"response"`
	Model      string `json:"model"`
	Mode       string `json:"mode"`  // strategy | build | review | ""
	Facts      string `json:"facts"` // comma-separated
	Save       bool   `json:"save"`

	// Human annotations captured from the UI. All optional; empty strings
	// simply leave the corresponding field on the record unset. Length
	// caps are applied server-side so a textarea paste cannot bloat the
	// on-disk JSON past a reasonable limit.
	Title string `json:"title"`
	Brief string `json:"brief"`
	Note  string `json:"note"`

	// Parent linkage. Both optional; an empty ParentRecord leaves the
	// child record unstamped (legacy behaviour). Validated server-side
	// against the repo's records dir — a forged path is silently dropped.
	ParentRecord   string   `json:"parent_record"`
	InheritedFocus []string `json:"inherited_focus"`
}

type replayResp struct {
	Record     audit.AuditRecord `json:"record"`
	SavedPath  string            `json:"saved_path,omitempty"`
}

func handleReplay(w http.ResponseWriter, r *http.Request) {
	var req replayReq
	if err := decode(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad JSON: "+err.Error())
		return
	}
	cfg, ok := mustRepo(w, req.Repo)
	if !ok {
		return
	}
	if strings.TrimSpace(req.Response) == "" {
		writeErr(w, http.StatusBadRequest, "response text is required")
		return
	}
	if strings.TrimSpace(req.BundlePath) == "" {
		writeErr(w, http.StatusBadRequest, "bundle_path is required")
		return
	}

	bundlePath, err := confineToRepo(cfg.RepoRoot, req.BundlePath, false)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bundle_path: "+err.Error())
		return
	}
	bundle, err := loadBundleJSON(bundlePath)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "load bundle: "+err.Error())
		return
	}

	facts := splitCSV(req.Facts)
	model := strings.TrimSpace(req.Model)
	if model == "" {
		model = "claude-manual"
	}

	ctx, cancel := contextFor(r)
	defer cancel()
	rec, err := audit.Run(ctx,
		audit.StubModel{Label: model, Response: req.Response},
		bundle,
		audit.Options{ExpectsFacts: facts, Mode: normaliseMode(req.Mode)},
	)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "replay: "+err.Error())
		return
	}

	// Annotations are applied after audit.Run so the audit package stays
	// focused on evaluation — these fields are pure record metadata. We
	// trim + cap to keep pathological pastes off disk.
	rec.Title = clampAnnotation(req.Title, 200)
	rec.Brief = clampAnnotation(req.Brief, 4000)
	rec.Note = clampAnnotation(req.Note, 4000)

	// Parent linkage. resolveParentRecord enforces containment inside the
	// repo's records dir, so a malicious client cannot stamp a child with
	// a path pointing anywhere on disk. If the parent cannot be resolved
	// we drop the link silently — the audit still succeeds, it just won't
	// carry a breadcrumb.
	if pr := strings.TrimSpace(req.ParentRecord); pr != "" {
		if abs, title, ok := resolveParentRecord(cfg.RepoRoot, pr); ok {
			rec.ParentRecord = abs
			rec.ParentTitle = title
			rec.InheritedFocus = cleanInheritedFocus(req.InheritedFocus)
		}
	}

	resp := replayResp{Record: rec}
	if req.Save {
		dir := filepath.Join(cfg.RepoRoot, audit.DefaultRecordsDir)
		path, err := audit.SaveRecord(dir, rec)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "save record: "+err.Error())
			return
		}
		resp.SavedPath = path
	}
	writeJSON(w, http.StatusOK, resp)
}

// --------------------- /api/records ---------------------

type recordRow struct {
	Path          string  `json:"path"`
	Timestamp     string  `json:"timestamp"`
	Question      string  `json:"question"`
	Model         string  `json:"model"`
	Mode          string  `json:"mode"` // "" for legacy records — always emitted so the UI can filter
	BundleHash    string  `json:"bundle_hash"`
	GroundedRatio float64 `json:"grounded_ratio"`
	DriftRate     float64 `json:"drift_rate"`
	AnswerRecall  float64 `json:"answer_recall"`
	ExpectsFacts  bool    `json:"expects_facts"`

	// Human annotations. Always emitted (possibly ""), trimmed server-side
	// to keep the records listing payload small; the UI truncates further
	// for its table rows and gets the full text via the Compare endpoint.
	Title string `json:"title"`
	Brief string `json:"brief"`
	Note  string `json:"note"`

	// Parent linkage. Always emitted (possibly ""). ParentRecord is the
	// absolute path of the record this run was resumed from, so the UI can
	// match it against another row's Path to render a clickable breadcrumb.
	// ParentTitle is a frozen snapshot captured at resume time — survives
	// if the parent record is later renamed or deleted.
	ParentRecord string `json:"parent_record"`
	ParentTitle  string `json:"parent_title"`
}

func handleRecords(w http.ResponseWriter, r *http.Request) {
	repo := r.URL.Query().Get("repo")
	cfg, ok := mustRepo(w, repo)
	if !ok {
		return
	}
	dir := filepath.Join(cfg.RepoRoot, audit.DefaultRecordsDir)
	paths, err := audit.ListRecords(dir)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	rows := make([]recordRow, 0, len(paths))
	for _, p := range paths {
		rec, err := audit.LoadRecord(p)
		if err != nil {
			continue
		}
		rows = append(rows, recordRow{
			Path:          p,
			Timestamp:     rec.Timestamp.Local().Format("2006-01-02 15:04:05"),
			Question:      rec.Question,
			Model:         rec.Model,
			Mode:          rec.Mode,
			BundleHash:    rec.BundleHash,
			GroundedRatio: rec.GroundedRatio,
			DriftRate:     rec.Drift.Rate,
			AnswerRecall:  rec.AnswerRecall,
			ExpectsFacts:  len(rec.ExpectsFacts) > 0,
			Title:         rec.Title,
			// Brief/Note can be long; ship a short preview on the list
			// endpoint. The Compare endpoint loads the full record so the
			// UI never truly loses the original text.
			Brief:        previewText(rec.Brief, 280),
			Note:         previewText(rec.Note, 280),
			ParentRecord: rec.ParentRecord,
			ParentTitle:  rec.ParentTitle,
		})
	}
	// Most recent first — ListRecords returns sorted ascending by name/time.
	sort.Slice(rows, func(i, j int) bool { return rows[i].Path > rows[j].Path })

	writeJSON(w, http.StatusOK, map[string]any{"records": rows})
}

// --------------------- /api/record ---------------------
//
// Returns a single AuditRecord as JSON. Used by the Journal's expand
// action: the list endpoint ships previews, this one ships the whole
// thing. Kept as a distinct endpoint (rather than a ?full=1 flag on
// /api/records) so callers never pay for the full payload when they
// only needed the summary.
//
// Access is scoped to the repo's records directory — we don't want a
// local server to double as an arbitrary-file reader. A client trying
// to escape with "..", an absolute path outside the repo, or a symlink
// pointing somewhere else gets a 400.

func handleRecord(w http.ResponseWriter, r *http.Request) {
	repo := r.URL.Query().Get("repo")
	raw := r.URL.Query().Get("path")
	cfg, ok := mustRepo(w, repo)
	if !ok {
		return
	}
	if strings.TrimSpace(raw) == "" {
		writeErr(w, http.StatusBadRequest, "path is required")
		return
	}
	abs, err := filepath.Abs(raw)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad path: "+err.Error())
		return
	}
	// Follow symlinks before the containment check so you cannot escape
	// by symlinking audit/records/evil.json → /etc/passwd.
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolved
	}
	recordsDir, err := filepath.Abs(filepath.Join(cfg.RepoRoot, audit.DefaultRecordsDir))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "resolve records dir: "+err.Error())
		return
	}
	rel, err := filepath.Rel(recordsDir, abs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		writeErr(w, http.StatusBadRequest, "path must live inside the repo records directory")
		return
	}
	rec, err := audit.LoadRecord(abs)
	if err != nil {
		writeErr(w, http.StatusNotFound, "load record: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, rec)
}

// --------------------- /api/search ---------------------
//
// Global substring search across every audit record in the repo. Walks
// the records directory, loads each JSON, and collects matches in:
//
//   - metadata  : title / brief / note / question / mode / bundle_hash
//   - paths     : fragment rel_path
//   - content   : fragment content
//
// Case-insensitive substring — no regex, no stemming, no index. A linear
// scan over O(records) files is perfectly fine for the few-hundred-
// records-max we expect locally; if the store ever grows we'll add an
// mtime-keyed in-memory cache. Today's cost: ~1 MB of JSON for 100
// records, which modern SSDs read in single-digit ms.
//
// Scoring is deliberately crude — a sum with metadata=3, path=2,
// content=1 — so metadata wins ties but fragment hits still float up
// above a single-field match. Recency (path sorts desc because
// filenames start with unix-sec) breaks equal scores. No ranking
// engineering beyond that; this is a daily-use filter, not a retrieval
// system.

type searchMatch struct {
	Field   string `json:"field"`              // title|brief|note|question|mode|bundle_hash|fragment_path|fragment_content
	Snippet string `json:"snippet"`            // ~80 chars around the first hit, whitespace-collapsed
	RelPath string `json:"rel_path,omitempty"` // set only for fragment_* matches
}

type searchResult struct {
	Path          string        `json:"path"`
	Title         string        `json:"title"`
	Question      string        `json:"question"`
	Mode          string        `json:"mode"`
	Timestamp     string        `json:"timestamp"`
	BundleHash    string        `json:"bundle_hash"`
	GroundedRatio float64       `json:"grounded_ratio"`
	DriftRate     float64       `json:"drift_rate"`
	Score         int           `json:"score"`
	Matches       []searchMatch `json:"matches"`
}

func handleSearch(w http.ResponseWriter, r *http.Request) {
	repo := r.URL.Query().Get("repo")
	cfg, ok := mustRepo(w, repo)
	if !ok {
		return
	}
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		writeErr(w, http.StatusBadRequest, "q is required")
		return
	}
	scope := strings.TrimSpace(r.URL.Query().Get("scope"))
	if scope == "" {
		scope = "all"
	}
	mode := strings.TrimSpace(r.URL.Query().Get("mode"))
	if mode == "" {
		mode = "all"
	}

	dir := filepath.Join(cfg.RepoRoot, audit.DefaultRecordsDir)
	paths, err := audit.ListRecords(dir)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	needle := strings.ToLower(q)
	results := make([]searchResult, 0, 16)
	for _, p := range paths {
		rec, err := audit.LoadRecord(p)
		if err != nil {
			// One bad file must not break search across the rest of the
			// history — skip silently and keep going.
			continue
		}
		if !matchesModeFilter(rec.Mode, mode) {
			continue
		}
		matches := collectSearchMatches(rec, needle, scope)
		if len(matches) == 0 {
			continue
		}
		results = append(results, searchResult{
			Path:          p,
			Title:         rec.Title,
			Question:      rec.Question,
			Mode:          rec.Mode,
			Timestamp:     rec.Timestamp.Local().Format("2006-01-02 15:04:05"),
			BundleHash:    rec.BundleHash,
			GroundedRatio: rec.GroundedRatio,
			DriftRate:     rec.Drift.Rate,
			Score:         scoreMatches(matches),
			Matches:       matches,
		})
	}

	sort.Slice(results, func(i, j int) bool {
		if results[i].Score != results[j].Score {
			return results[i].Score > results[j].Score
		}
		// Paths start with unix-sec so lexical desc = newer first.
		return results[i].Path > results[j].Path
	})

	// A sloppy one-char query over a big store could otherwise ship
	// megabytes of JSON — cap at 100. The UI reports total matches
	// separately so the user knows more exist.
	total := len(results)
	const maxResults = 100
	if len(results) > maxResults {
		results = results[:maxResults]
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"query":         q,
		"scope":         scope,
		"mode":          mode,
		"total_records": len(paths),
		"total_matches": total,
		"results":       results,
	})
}

// matchesModeFilter returns true iff the record's mode is allowed by
// the UI's mode pill. "all" passes everything, "unknown" matches the
// legacy empty-string case so records from before the mode field was
// added stay reachable via this filter.
func matchesModeFilter(recMode, filter string) bool {
	switch filter {
	case "", "all":
		return true
	case "unknown":
		return recMode == ""
	default:
		return recMode == filter
	}
}

// collectSearchMatches runs the scope-gated field sweep. We cap at 3
// hits per fragment-kind per record so a content-heavy bundle doesn't
// flood the result card — the user can open the record to see the rest.
// The caller is expected to pass a lowercase needle; we lowercase again
// here defensively so direct callers (tests, future callers) don't have
// to remember the contract.
func collectSearchMatches(rec audit.AuditRecord, needle, scope string) []searchMatch {
	needle = strings.ToLower(needle)
	wantMeta := scope == "all" || scope == "metadata"
	wantPath := scope == "all" || scope == "paths"
	wantCont := scope == "all" || scope == "content"

	out := make([]searchMatch, 0, 4)

	if wantMeta {
		fields := []struct{ field, text string }{
			{"title", rec.Title},
			{"brief", rec.Brief},
			{"note", rec.Note},
			{"question", rec.Question},
			{"mode", rec.Mode},
			{"bundle_hash", rec.BundleHash},
		}
		for _, f := range fields {
			if f.text == "" {
				continue
			}
			if strings.Contains(strings.ToLower(f.text), needle) {
				out = append(out, searchMatch{
					Field:   f.field,
					Snippet: snippetAround(f.text, needle, 80),
				})
			}
		}
	}

	const perKindCap = 3
	pathHits, contentHits := 0, 0
	for _, frag := range rec.Fragments {
		if wantPath && pathHits < perKindCap &&
			strings.Contains(strings.ToLower(frag.RelPath), needle) {
			out = append(out, searchMatch{
				Field:   "fragment_path",
				Snippet: frag.RelPath,
				RelPath: frag.RelPath,
			})
			pathHits++
		}
		if wantCont && contentHits < perKindCap && frag.Content != "" &&
			strings.Contains(strings.ToLower(frag.Content), needle) {
			out = append(out, searchMatch{
				Field:   "fragment_content",
				Snippet: snippetAround(frag.Content, needle, 80),
				RelPath: frag.RelPath,
			})
			contentHits++
		}
	}
	return out
}

func scoreMatches(matches []searchMatch) int {
	s := 0
	for _, m := range matches {
		switch m.Field {
		case "title", "brief", "note", "question", "mode", "bundle_hash":
			s += 3
		case "fragment_path":
			s += 2
		case "fragment_content":
			s++
		}
	}
	return s
}

// snippetAround returns ~window bytes of context around the first
// match of needle in haystack, with whitespace collapsed so the
// snippet renders as a single line in the result card. Two invariants:
//
//  1. The full needle is ALWAYS preserved — if window is shorter than
//     the match we give up on context rather than truncate the match.
//  2. start/end sit on rune boundaries so multi-byte runes in briefs
//     or notes (accents, emoji) never produce replacement chars.
func snippetAround(haystack, needle string, window int) string {
	if haystack == "" {
		return ""
	}
	low := strings.ToLower(haystack)
	nl := strings.ToLower(needle)
	i := strings.Index(low, nl)
	if i < 0 {
		// Defensive: caller proved Contains, but if the cased index
		// disagrees for some Unicode reason, return a head window.
		return collapseWS(clampHead(haystack, window))
	}
	matchEnd := i + len(nl)
	matchLen := matchEnd - i
	// Split any leftover window evenly around the match. If the needle
	// itself already exceeds window, both pads are zero.
	pad := window - matchLen
	if pad < 0 {
		pad = 0
	}
	leftPad := pad / 2
	rightPad := pad - leftPad
	start := i - leftPad
	end := matchEnd + rightPad
	if start < 0 {
		start = 0
	}
	if end > len(haystack) {
		end = len(haystack)
	}
	start = clampRuneBack(haystack, start)
	end = clampRuneFwd(haystack, end)
	// clampRuneFwd stops at the next rune start, which may be the first
	// byte of a multi-byte rune that our match partially covered. If
	// that chopped the needle, extend past the current rune to include
	// it in full.
	if end < matchEnd {
		end = matchEnd
		for end < len(haystack) && !utf8.RuneStart(haystack[end]) {
			end++
		}
	}
	prefix, suffix := "", ""
	if start > 0 {
		prefix = "…"
	}
	if end < len(haystack) {
		suffix = "…"
	}
	return prefix + collapseWS(haystack[start:end]) + suffix
}

func collapseWS(s string) string { return strings.Join(strings.Fields(s), " ") }

func clampRuneBack(s string, i int) int {
	if i < 0 {
		return 0
	}
	if i >= len(s) {
		return len(s)
	}
	for i > 0 && !utf8.RuneStart(s[i]) {
		i--
	}
	return i
}

func clampRuneFwd(s string, i int) int {
	if i <= 0 {
		return 0
	}
	if i >= len(s) {
		return len(s)
	}
	for i < len(s) && !utf8.RuneStart(s[i]) {
		i++
	}
	return i
}

func clampHead(s string, window int) string {
	if len(s) <= window {
		return s
	}
	return s[:clampRuneBack(s, window)]
}

// --------------------- /api/diff ---------------------

type diffReq struct {
	Repo string `json:"repo"`
	A    string `json:"a"`
	B    string `json:"b"`
}

func handleDiff(w http.ResponseWriter, r *http.Request) {
	var req diffReq
	if err := decode(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad JSON: "+err.Error())
		return
	}
	cfg, ok := mustRepo(w, req.Repo)
	if !ok {
		return
	}
	if strings.TrimSpace(req.A) == "" || strings.TrimSpace(req.B) == "" {
		writeErr(w, http.StatusBadRequest, "both 'a' and 'b' paths are required")
		return
	}
	// Both operands must resolve inside the repo's records directory —
	// otherwise the diff endpoint would double as an arbitrary-file
	// reader (a: /etc/passwd, b: /etc/hostname would read system files
	// the user never asked this tool to touch).
	recordsRoot := filepath.Join(cfg.RepoRoot, audit.DefaultRecordsDir)
	aPath, err := confineToRepo(recordsRoot, req.A, false)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "a: "+err.Error())
		return
	}
	bPath, err := confineToRepo(recordsRoot, req.B, false)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "b: "+err.Error())
		return
	}
	a, err := audit.LoadRecord(aPath)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "load a: "+err.Error())
		return
	}
	b, err := audit.LoadRecord(bPath)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "load b: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, audit.DiffRecords(a, b))
}

// --------------------- /api/stats ---------------------

type statsResp struct {
	RepoRoot  string           `json:"repo_root"`
	Files     int              `json:"files"`
	Symbols   int              `json:"symbols"`
	Imports   int              `json:"imports"`
	DBBytes   int64            `json:"db_bytes"`
	Languages map[string]int   `json:"languages"`
	Audit     *audit.Aggregate `json:"audit,omitempty"`
}

func handleStats(w http.ResponseWriter, r *http.Request) {
	repo := r.URL.Query().Get("repo")
	cfg, ok := mustRepo(w, repo)
	if !ok {
		return
	}
	db, err := storage.Open(cfg.DBPath)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "open index: "+err.Error())
		return
	}
	defer db.Close()

	files, err := db.AllFiles()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	breakdown, _ := db.LangBreakdown()
	dbBytes, _ := db.DBSize()

	langs := make(map[string]int, len(breakdown))
	for k, v := range breakdown {
		langs[string(k)] = v
	}

	symbols, imports := 0, 0
	for _, f := range files {
		symbols += len(f.Symbols)
		imports += len(f.Imports)
	}

	resp := statsResp{
		RepoRoot:  cfg.RepoRoot,
		Files:     len(files),
		Symbols:   symbols,
		Imports:   imports,
		DBBytes:   dbBytes,
		Languages: langs,
	}

	// Audit aggregate is best-effort: a repo without any records still
	// returns a valid stats payload.
	paths, _ := audit.ListRecords(filepath.Join(cfg.RepoRoot, audit.DefaultRecordsDir))
	recs := make([]audit.AuditRecord, 0, len(paths))
	for _, p := range paths {
		if rec, err := audit.LoadRecord(p); err == nil {
			recs = append(recs, rec)
		}
	}
	if len(recs) > 0 {
		agg := audit.AggregateFrom(recs)
		resp.Audit = &agg
	}

	writeJSON(w, http.StatusOK, resp)
}

// --------------------- inlined CLI helpers ---------------------
//
// The CLI package has equivalent copies of these (loadProjectInfo,
// gitChangedFiles, writeBundleJSON, buildRepoSummary). We inline them here
// rather than extracting a shared package to keep this iteration small: the
// UI is additive and shouldn't force a cross-package refactor. If a third
// caller appears, move these to internal/ctxutil.

func loadProjectInfo(db *storage.DB) *project.Info {
	raw, ok, err := db.GetMeta(indexer.ProjectMetaKey)
	if err != nil || !ok {
		return nil
	}
	return project.Decode(raw)
}

func gitChangedFiles(repoRoot string) []string {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "-C", repoRoot, "status", "--porcelain", "--untracked-files=normal")
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	var files []string
	seen := make(map[string]bool)
	for _, line := range strings.Split(string(out), "\n") {
		if len(line) < 4 {
			continue
		}
		path := strings.TrimSpace(line[3:])
		if arrow := strings.Index(path, " -> "); arrow >= 0 {
			path = path[arrow+4:]
		}
		path = strings.Trim(path, `"`)
		path = filepath.ToSlash(path)
		if path == "" || seen[path] {
			continue
		}
		seen[path] = true
		files = append(files, path)
	}
	return files
}

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

func loadBundleJSON(path string) (models.Bundle, error) {
	var b models.Bundle
	data, err := os.ReadFile(path)
	if err != nil {
		return b, fmt.Errorf("read %s: %w", path, err)
	}
	if err := json.Unmarshal(data, &b); err != nil {
		return b, fmt.Errorf("parse %s: %w", path, err)
	}
	if len(b.Fragments) == 0 {
		return b, fmt.Errorf("bundle at %s has no fragments", path)
	}
	return b, nil
}

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

// normaliseMode trims + lowercases the mode string so records stay
// canonical even when a caller sends "Strategy " or "STRATEGY". Unknown
// values pass through verbatim (lowercased) — we do not reject them:
// the field is intentionally open-ended for future modes.
func normaliseMode(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	return s
}

// clampAnnotation trims whitespace and hard-caps a human annotation so a
// runaway paste cannot bloat a record's on-disk JSON. The cap is generous
// enough to hold a multi-paragraph brief but tight enough to stay under the
// 8MB body limit even if every field is maxed out. Empty input returns "",
// which combined with the omitempty JSON tag leaves the field unpersisted.
//
// Truncation walks back to the nearest rune boundary so multi-byte runes
// (emoji, accents, CJK) never get split — writing a record with a
// half-rune would produce invalid UTF-8 JSON that later loads would fail.
func clampAnnotation(s string, max int) string {
	s = strings.TrimSpace(s)
	if max > 0 && len(s) > max {
		s = s[:clampRuneBack(s, max)]
	}
	return s
}

// previewText returns a single-line preview suitable for a table cell:
// collapses internal whitespace, trims, and truncates with an ellipsis when
// the source exceeds max bytes. Cheap rune-safe truncation — we cut on a
// byte boundary after a rune, not mid-rune, so UTF-8 stays valid.
func previewText(s string, max int) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	s = strings.Join(strings.Fields(s), " ")
	if max <= 0 || len(s) <= max {
		return s
	}
	// Walk back to the last rune boundary ≤ max so we never split a rune.
	cut := max
	for cut > 0 && (s[cut]&0xC0) == 0x80 {
		cut--
	}
	return s[:cut] + "…"
}

func splitCSV(s string) []string {
	var out []string
	for _, f := range strings.Split(s, ",") {
		if t := strings.TrimSpace(f); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// --------------------- parent-context helpers ---------------------

// mergeFocus joins the user-typed focus CSV with the inherited list carried
// over from a parent record. Dedupe is case-sensitive (paths are case-
// sensitive on most SSD checkouts) and order is preserved: user entries
// first, inherited after. The ranker's parseFocusPrefixes handles whitespace
// and empty segments, so we just emit a clean comma-joined string.
func mergeFocus(user string, inherited []string) string {
	out := make([]string, 0, len(inherited)+4)
	seen := make(map[string]bool)
	add := func(s string) {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			return
		}
		seen[s] = true
		out = append(out, s)
	}
	for _, part := range strings.Split(user, ",") {
		add(part)
	}
	for _, p := range inherited {
		add(p)
	}
	return strings.Join(out, ",")
}

// cleanInheritedFocus normalises the list the client submitted: trims
// whitespace, drops empties and artefact paths, dedupes, and caps the
// total count so a pathological paste cannot bloat a record.
func cleanInheritedFocus(in []string) []string {
	const maxPaths = 64
	if len(in) == 0 {
		return nil
	}
	out := make([]string, 0, len(in))
	seen := make(map[string]bool)
	for _, p := range in {
		p = strings.TrimSpace(p)
		if p == "" || seen[p] || isArtifactPath(p) {
			continue
		}
		seen[p] = true
		out = append(out, p)
		if len(out) >= maxPaths {
			break
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// isArtifactPath is true for paths that live under NeuroFS' own bookkeeping
// directories — worktrees, the index DB, and the audit trail itself. These
// are never useful focus seeds: they either don't exist in a fresh checkout
// or point back at the very records we're auditing. Normalises to forward
// slashes and strips a leading "./" so either form matches.
func isArtifactPath(rel string) bool {
	p := strings.TrimPrefix(filepath.ToSlash(rel), "./")
	if p == "" {
		return false
	}
	prefixes := []string{
		".claude/worktrees/",
		".neurofs/",
		"audit/bundles/",
		"audit/records/",
		"audit/responses/",
	}
	for _, pre := range prefixes {
		if strings.HasPrefix(p, pre) {
			return true
		}
	}
	return false
}

// resolveParentRecord validates a client-supplied parent record path,
// resolves symlinks, enforces containment inside <repo>/audit/records, and
// returns the absolute path + a breadcrumb title pulled from the parent
// (Title, falling back to Question). The bool is false when the path is
// invalid, escapes the records dir, or cannot be decoded — callers drop
// the parent link silently in that case.
func resolveParentRecord(repoRoot, raw string) (string, string, bool) {
	abs, err := filepath.Abs(raw)
	if err != nil {
		return "", "", false
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolved
	}
	recordsDir, err := filepath.Abs(filepath.Join(repoRoot, audit.DefaultRecordsDir))
	if err != nil {
		return "", "", false
	}
	rel, err := filepath.Rel(recordsDir, abs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", "", false
	}
	parent, err := audit.LoadRecord(abs)
	if err != nil {
		return "", "", false
	}
	title := strings.TrimSpace(parent.Title)
	if title == "" {
		title = strings.TrimSpace(parent.Question)
	}
	if len(title) > 160 {
		title = title[:160]
	}
	return abs, title, true
}

// --------------------- /api/resume-seed ---------------------
//
// Returns the minimum data New task needs to render a "resuming from…"
// state: the parent's human annotations for prefill, plus the rel_paths
// of every fragment in the parent's bundle as a shortlist the user can
// edit before packing. We deliberately do NOT ship fragment content —
// stale content would be a silent error; a stale path merely fails to
// boost anything in the ranker, which is benign.

type resumeSeedResp struct {
	ParentPath          string   `json:"parent_path"`
	Title               string   `json:"title"`
	Brief               string   `json:"brief"`
	Question            string   `json:"question"`
	Mode                string   `json:"mode"`
	SuggestedFocusPaths []string `json:"suggested_focus_paths"`
	ParentTokens        int      `json:"parent_tokens"`
}

func handleResumeSeed(w http.ResponseWriter, r *http.Request) {
	repo := r.URL.Query().Get("repo")
	raw := r.URL.Query().Get("path")
	cfg, ok := mustRepo(w, repo)
	if !ok {
		return
	}
	if strings.TrimSpace(raw) == "" {
		writeErr(w, http.StatusBadRequest, "path is required")
		return
	}
	abs, title, ok := resolveParentRecord(cfg.RepoRoot, raw)
	if !ok {
		writeErr(w, http.StatusBadRequest, "parent path must live inside the repo records directory")
		return
	}
	rec, err := audit.LoadRecord(abs)
	if err != nil {
		writeErr(w, http.StatusNotFound, "load parent: "+err.Error())
		return
	}

	paths := make([]string, 0, len(rec.Fragments))
	seen := make(map[string]bool)
	tokens := 0
	for _, f := range rec.Fragments {
		tokens += f.Tokens
		p := strings.TrimSpace(f.RelPath)
		if p == "" || seen[p] || isArtifactPath(p) {
			continue
		}
		// Drop paths that no longer exist in the current checkout. A stale
		// path is not catastrophic (the ranker just fails to boost it) but
		// it is noise in the UI — quietly filtering keeps the shortlist
		// actionable. Stat errors other than ENOENT still let the path
		// through; we don't want a transient fs hiccup to silently shrink
		// the list.
		if abs, err := filepath.Abs(filepath.Join(cfg.RepoRoot, p)); err == nil {
			if _, statErr := os.Stat(abs); os.IsNotExist(statErr) {
				continue
			}
		}
		seen[p] = true
		paths = append(paths, p)
	}

	writeJSON(w, http.StatusOK, resumeSeedResp{
		ParentPath:          abs,
		Title:               title,
		Brief:               rec.Brief,
		Question:            rec.Question,
		Mode:                rec.Mode,
		SuggestedFocusPaths: paths,
		ParentTokens:        tokens,
	})
}

// --------------------- /api/task ---------------------
//
// One-shot task flow: a single POST that auto-scans (if needed), ranks,
// packs, and returns the Claude-shaped prompt. This is the UI mirror of
// `neurofs task <query>` — both routes call taskflow.Run so the prompt
// the UI shows is byte-for-byte what the CLI emits, and the cache
// (.neurofs/task/) is shared.

type taskReq struct {
	Repo   string `json:"repo"`
	Query  string `json:"query"`
	Budget int    `json:"budget"`
	Force  bool   `json:"force"`
}

func handleTask(w http.ResponseWriter, r *http.Request) {
	var req taskReq
	if err := decode(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad JSON: "+err.Error())
		return
	}
	if strings.TrimSpace(req.Query) == "" {
		writeErr(w, http.StatusBadRequest, "query is required")
		return
	}
	// mustRepo enforces the same absolute-path / valid-config rules as
	// every other handler, so the bootstrap-suggested cwd cannot be
	// silently massaged into something exotic.
	if _, ok := mustRepo(w, req.Repo); !ok {
		return
	}

	res, err := taskflow.Run(taskflow.Opts{
		RepoRoot: req.Repo,
		Query:    req.Query,
		Budget:   req.Budget,
		Force:    req.Force,
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "task: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// --------------------- /api/bootstrap ---------------------
//
// Returns the directory the binary was launched from. The UI uses this
// to prefill the repo input on first run — `cd <project> && neurofs ui`
// then opens at /, sees an empty localStorage, and quietly fills the
// path so the user can hit Pack without typing.

type bootstrapResp struct {
	Cwd string `json:"cwd"`
}

func handleBootstrap(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, bootstrapResp{Cwd: startupDir})
}
