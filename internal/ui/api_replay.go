package ui

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/neuromfs/neuromfs/internal/audit"
	"github.com/neuromfs/neuromfs/internal/fsutil"
	"github.com/neuromfs/neuromfs/internal/models"
)

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
	Record    audit.AuditRecord `json:"record"`
	SavedPath string            `json:"saved_path,omitempty"`
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

	bundlePath, err := fsutil.ConfineToRepo(cfg.RepoRoot, req.BundlePath)
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

// --------------------- /api/resume-seed ---------------------

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

// --------------------- replay helpers ---------------------

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
