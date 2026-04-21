package ui

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/neuromfs/neuromfs/internal/audit"
	"github.com/neuromfs/neuromfs/internal/config"
	"github.com/neuromfs/neuromfs/internal/indexer"
	"github.com/neuromfs/neuromfs/internal/models"
	"github.com/neuromfs/neuromfs/internal/output"
	"github.com/neuromfs/neuromfs/internal/packager"
	"github.com/neuromfs/neuromfs/internal/project"
	"github.com/neuromfs/neuromfs/internal/ranking"
	"github.com/neuromfs/neuromfs/internal/storage"
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
	mux.HandleFunc("/api/diff", postOnly(handleDiff))
	mux.HandleFunc("/api/stats", getOnly(handleStats))
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
	return cfg, true
}

// decode reads JSON body into v with a generous size limit. Keeping the
// limit tight protects against accidentally pasting a multi-MB file into
// a textarea — replay responses are rarely more than ~200KB.
func decode(r *http.Request, v any) error {
	r.Body = http.MaxBytesReader(nil, r.Body, 8<<20) // 8 MB
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	return dec.Decode(v)
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
		Focus:   req.Focus,
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
		target := name
		if !filepath.IsAbs(target) {
			target = filepath.Join(cfg.RepoRoot, target)
		}
		if err := writeBundleJSON(target, bundle); err != nil {
			writeErr(w, http.StatusInternalServerError, "save snapshot: "+err.Error())
			return
		}
		snapshotPath = target
	}

	cache.mu.Lock()
	cache.entries[cfg.RepoRoot] = cachedBundle{path: snapshotPath, savedAt: time.Now()}
	cache.mu.Unlock()

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

	bundlePath := req.BundlePath
	if !filepath.IsAbs(bundlePath) {
		bundlePath = filepath.Join(cfg.RepoRoot, bundlePath)
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
			Brief: previewText(rec.Brief, 280),
			Note:  previewText(rec.Note, 280),
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

// --------------------- /api/diff ---------------------

type diffReq struct {
	A string `json:"a"`
	B string `json:"b"`
}

func handleDiff(w http.ResponseWriter, r *http.Request) {
	var req diffReq
	if err := decode(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad JSON: "+err.Error())
		return
	}
	if strings.TrimSpace(req.A) == "" || strings.TrimSpace(req.B) == "" {
		writeErr(w, http.StatusBadRequest, "both 'a' and 'b' paths are required")
		return
	}
	a, err := audit.LoadRecord(req.A)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "load a: "+err.Error())
		return
	}
	b, err := audit.LoadRecord(req.B)
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
func clampAnnotation(s string, max int) string {
	s = strings.TrimSpace(s)
	if max > 0 && len(s) > max {
		s = s[:max]
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
