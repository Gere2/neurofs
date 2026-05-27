package ui

import (
	"net/http"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/neuromfs/neuromfs/internal/audit"
	"github.com/neuromfs/neuromfs/internal/fsutil"
	"github.com/neuromfs/neuromfs/internal/storage"
)

// --------------------- /api/search ---------------------

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
	aPath, err := fsutil.ConfineToRepo(recordsRoot, req.A)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "a: "+err.Error())
		return
	}
	bPath, err := fsutil.ConfineToRepo(recordsRoot, req.B)
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
