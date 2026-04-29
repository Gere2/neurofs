// Package ranking scores indexed files by relevance to a query.
//
// Scoring is purely structural and lexical — no embeddings, no LLM calls.
// Signals and their weights:
//
//	filename_match   (+3.0)  query term appears in the file name
//	path_match       (+1.5)  query term appears anywhere in the path
//	symbol_match     (+2.5)  query term appears in a symbol name
//	import_match     (+1.0)  query term appears in an import path
//	import_expansion (+0.8)  file is imported by a high-scoring file
//	lang_bonus       (+0.3)  file is in a preferred language (TS/JS/Python)
package ranking

import (
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/neuromfs/neuromfs/internal/models"
	"github.com/neuromfs/neuromfs/internal/project"
	"github.com/neuromfs/neuromfs/internal/tokenbudget"
)

const (
	weightFilename        = 3.0
	weightPath            = 1.5
	weightSymbol          = 2.5
	weightImport          = 1.0
	weightImportExpansion = 0.8
	weightLangBonus       = 0.3
	weightContentMatch    = 0.5
	weightEntryPoint      = 1.5
	weightDependencyMatch = 1.2
	weightFocus           = 4.0
	weightChanged         = 2.0
	// weightRootDoc is a floor for canonical project docs at the repo root
	// (README, ARCHITECTURE, CONTRIBUTING, CHANGELOG). Without it, queries in
	// languages other than the doc's language — or any query that does not
	// happen to lexically match the doc's contents — leave the README at zero
	// score and out of every bundle, which is exactly the wrong behaviour for
	// "what is this project" style questions. Sized between weightSymbol and
	// weightFilename so a strong specific match still beats it, but a weak or
	// non-matching query still surfaces the README.
	weightRootDoc = 3.5

	// expansionLimit is the number of top files used as seeds for import expansion.
	expansionLimit = 12
)

// rootDocStems is the set of canonical doc filename stems (no extension,
// lowercase) that earn weightRootDoc when they sit at the repo root.
var rootDocStems = map[string]bool{
	"readme":       true,
	"architecture": true,
	"contributing": true,
	"changelog":    true,
}

// Options tunes the ranker. A zero Options is equivalent to plain Rank.
type Options struct {
	// Project carries package.json / tsconfig.json signals. May be nil.
	Project *project.Info
	// Focus, when non-empty, is a path prefix (or comma-separated list of
	// prefixes) that receives a strong additive boost. It does not filter:
	// non-matching files can still appear, just lower-ranked. This is the
	// knob users reach for when "the interesting bit lives in X/" and they
	// want to avoid spending tokens on the rest of the repo.
	Focus string
	// ChangedFiles, when non-empty, lists repo-relative paths the ranker
	// should treat as recently modified. Matches receive an additive boost,
	// so active work surfaces above unrelated historical files at the same
	// match quality.
	ChangedFiles []string
}

// Rank scores all files against the query and returns them sorted by score
// (highest first). Files with a score of zero are still returned so callers can
// see what was considered.
//
// This is a convenience wrapper around RankWithOptions for callers that have
// no project metadata — tests and older callers still compile unchanged.
func Rank(files []models.FileRecord, query string) []models.ScoredFile {
	return RankWithOptions(files, query, Options{})
}

// RankWithOptions scores files against a query, letting callers enrich the
// run with project metadata. When Options.Project is nil the behaviour is
// identical to Rank.
func RankWithOptions(files []models.FileRecord, query string, opts Options) []models.ScoredFile {
	terms := tokenise(query)
	if len(terms) == 0 {
		out := make([]models.ScoredFile, len(files))
		for i, f := range files {
			out[i] = models.ScoredFile{Record: f}
			// Even with no terms, entry-point, focus, and changed bonuses
			// still give structure to "what's in this repo" or
			// "review my edits" queries.
			if opts.Project != nil {
				addEntryPointBonus(&out[i], opts.Project)
			}
		}
		applyFocusBoost(out, opts.Focus)
		applyChangedBoost(out, opts.ChangedFiles)
		sort.Slice(out, func(i, j int) bool { return out[i].Score > out[j].Score })
		return out
	}

	scored := make([]models.ScoredFile, len(files))
	for i, f := range files {
		sc, reasons := scoreFile(f, terms)
		scored[i] = models.ScoredFile{Record: f, Score: sc, Reasons: reasons}
	}

	if opts.Project != nil {
		applyProjectSignals(scored, terms, opts.Project)
	}

	applyFocusBoost(scored, opts.Focus)
	applyChangedBoost(scored, opts.ChangedFiles)

	expandByImports(scored, terms, opts.Project)
	enrichWithContent(scored, terms)

	applyTestPenalty(scored, query)

	sort.Slice(scored, func(i, j int) bool {
		return scored[i].Score > scored[j].Score
	})

	return scored
}

// scoreFile computes the base score for a single file.
func scoreFile(f models.FileRecord, terms []string) (float64, []models.InclusionReason) {
	score := 0.0
	var reasons []models.InclusionReason

	baseName := strings.ToLower(filepath.Base(f.RelPath))
	baseStem := stripExt(baseName)
	relPath := strings.ToLower(f.RelPath)

	for _, term := range terms {
		variants := termVariants(term)

		// Filename match — highest value signal.
		// Also treat reverse containment (e.g. stem "auth" inside term
		// "authentication") as a filename match, so descriptive queries
		// still find files with abbreviated names.
		if anyContains(baseName, variants) ||
			(len(baseStem) >= 3 && anyContainsReverse(variants, baseStem)) {
			score += weightFilename
			reasons = append(reasons, models.InclusionReason{
				Signal: "filename_match",
				Detail: term,
				Weight: weightFilename,
			})
		} else if anyContains(relPath, variants) {
			// Path match (directory components, not just filename).
			score += weightPath
			reasons = append(reasons, models.InclusionReason{
				Signal: "path_match",
				Detail: term,
				Weight: weightPath,
			})
		}

		// Symbol match.
		for _, sym := range f.Symbols {
			symLower := strings.ToLower(sym.Name)
			if anyContains(symLower, variants) {
				score += weightSymbol
				reasons = append(reasons, models.InclusionReason{
					Signal: "symbol_match",
					Detail: sym.Name,
					Weight: weightSymbol,
				})
				break // one bonus per term per file
			}
		}

		// Import match.
		for _, imp := range f.Imports {
			if anyContains(strings.ToLower(imp), variants) {
				score += weightImport
				reasons = append(reasons, models.InclusionReason{
					Signal: "import_match",
					Detail: imp,
					Weight: weightImport,
				})
				break
			}
		}
	}

	// Language bonus: prioritise code files over config/data.
	switch f.Lang {
	case models.LangTypeScript, models.LangJavaScript, models.LangPython, models.LangGo:
		score += weightLangBonus
		reasons = append(reasons, models.InclusionReason{
			Signal: "lang_bonus",
			Detail: string(f.Lang),
			Weight: weightLangBonus,
		})
	}

	// Root-doc floor: README et al. at the repo root are always relevant
	// enough to earn a slot. Subdirectory READMEs (per-package docs) do
	// not get the boost — they're scope-local, not project-level.
	if isRootDoc(f.RelPath) {
		score += weightRootDoc
		reasons = append(reasons, models.InclusionReason{
			Signal: "root_doc",
			Detail: filepath.Base(f.RelPath),
			Weight: weightRootDoc,
		})
	}

	return score, reasons
}

// isRootDoc reports whether relPath is a canonical project doc sitting at
// the repository root.
func isRootDoc(relPath string) bool {
	rel := filepath.ToSlash(relPath)
	if strings.Contains(rel, "/") {
		return false
	}
	stem := strings.ToLower(stripExt(rel))
	return rootDocStems[stem]
}

// expandByImports boosts files that are imported by high-scoring seeds.
// When info is non-nil, tsconfig path aliases are resolved before matching so
// imports like `@app/user` expand to files under `src/user` the same way
// relative imports do.
func expandByImports(scored []models.ScoredFile, _ []string, info *project.Info) {
	tmp := make([]models.ScoredFile, len(scored))
	copy(tmp, scored)
	sort.Slice(tmp, func(i, j int) bool { return tmp[i].Score > tmp[j].Score })

	limit := expansionLimit
	if limit > len(tmp) {
		limit = len(tmp)
	}
	seeds := tmp[:limit]

	importedPaths := make(map[string]bool)
	for _, s := range seeds {
		for _, imp := range s.Record.Imports {
			importedPaths[strings.ToLower(imp)] = true
			if resolved := resolveAlias(imp, info); resolved != "" {
				importedPaths[strings.ToLower(resolved)] = true
			}
		}
	}

	if len(importedPaths) == 0 {
		return
	}

	for i, sf := range scored {
		base := strings.ToLower(stripExt(filepath.Base(sf.Record.RelPath)))
		for imp := range importedPaths {
			// Require a path- or extension-boundary match. Plain substring
			// (imp contains base) used to fire here too, but that matched
			// `util` against `futility` / `auth_utilities` and inflated the
			// expansion score for accidental lookalikes.
			if imp == base ||
				strings.HasSuffix(imp, "/"+base) ||
				strings.HasPrefix(imp, base+".") ||
				strings.Contains(imp, "/"+base+".") {
				scored[i].Score += weightImportExpansion
				scored[i].Reasons = append(scored[i].Reasons, models.InclusionReason{
					Signal: "import_expansion",
					Detail: imp,
					Weight: weightImportExpansion,
				})
				break
			}
		}
	}
}

// applyProjectSignals folds package.json / tsconfig.json data into the score.
// Two new signals:
//
//	entry_point       — file is declared as main/module/types/bin
//	dependency_match  — a query term names a production dependency AND the
//	                    file imports that dependency
func applyProjectSignals(scored []models.ScoredFile, terms []string, info *project.Info) {
	if info == nil {
		return
	}

	entries := info.EntryPoints()
	for i := range scored {
		addEntryPointBonus(&scored[i], info)
		applyDependencyMatch(&scored[i], terms, info)
		_ = entries // referenced via addEntryPointBonus
	}
}

// addEntryPointBonus tags a file as an entry point when its relative path
// matches one of the project's declared entries.
func addEntryPointBonus(sf *models.ScoredFile, info *project.Info) {
	if info == nil {
		return
	}
	rel := filepath.ToSlash(sf.Record.RelPath)
	for _, entry := range info.EntryPoints() {
		if pathsMatchEntry(rel, entry) {
			sf.Score += weightEntryPoint
			sf.Reasons = append(sf.Reasons, models.InclusionReason{
				Signal: "entry_point",
				Detail: entry,
				Weight: weightEntryPoint,
			})
			return
		}
	}
}

// applyDependencyMatch boosts a file when a query term names a declared
// production dependency and the file imports that dependency. The bonus is
// only applied once per file.
func applyDependencyMatch(sf *models.ScoredFile, terms []string, info *project.Info) {
	if info == nil || len(info.Dependencies) == 0 {
		return
	}
	depSet := make(map[string]bool, len(info.Dependencies))
	for _, d := range info.Dependencies {
		depSet[strings.ToLower(d)] = true
	}
	fileImports := make(map[string]bool, len(sf.Record.Imports))
	for _, imp := range sf.Record.Imports {
		fileImports[strings.ToLower(imp)] = true
	}

	for _, term := range terms {
		for dep := range depSet {
			if !strings.Contains(dep, term) {
				continue
			}
			if !fileImports[dep] {
				continue
			}
			sf.Score += weightDependencyMatch
			sf.Reasons = append(sf.Reasons, models.InclusionReason{
				Signal: "dependency_match",
				Detail: dep,
				Weight: weightDependencyMatch,
			})
			return
		}
	}
}

// pathsMatchEntry returns true when rel refers to the same file as entry
// declared in package.json, ignoring extensions and leading `./`.
func pathsMatchEntry(rel, entry string) bool {
	rel = strings.TrimPrefix(rel, "./")
	entry = strings.TrimPrefix(entry, "./")
	if rel == entry {
		return true
	}
	// package.json usually lists compiled output (e.g. dist/index.js) while
	// the indexed file is the source (src/index.ts). Compare without ext and
	// last-two-path-components to catch the common pair.
	relStem := stripExt(rel)
	entryStem := stripExt(entry)
	if relStem == entryStem {
		return true
	}
	if filepath.Base(relStem) == filepath.Base(entryStem) {
		// Match on basename alone for reasonably unique names (length >= 4).
		base := filepath.Base(relStem)
		if len(base) >= 4 && base != "index" {
			return true
		}
	}
	return false
}

// resolveAlias rewrites an import path through tsconfig path aliases, if any
// alias matches as a prefix. Returns "" when no alias applies.
func resolveAlias(imp string, info *project.Info) string {
	if info == nil || len(info.PathAliases) == 0 {
		return ""
	}
	for alias, target := range info.PathAliases {
		if alias == "" {
			continue
		}
		if imp == alias {
			return target
		}
		if strings.HasPrefix(imp, alias+"/") {
			return target + strings.TrimPrefix(imp, alias)
		}
	}
	return ""
}

// enrichWithContent adds a content-match bonus by reading files for the top
// candidates. We cap at the top 30 to avoid I/O on the whole repo.
func enrichWithContent(scored []models.ScoredFile, terms []string) {
	// Work on a snapshot sorted by current score.
	type indexed struct {
		pos int
		sf  models.ScoredFile
	}
	tmp := make([]indexed, len(scored))
	for i, s := range scored {
		tmp[i] = indexed{i, s}
	}
	sort.Slice(tmp, func(i, j int) bool { return tmp[i].sf.Score > tmp[j].sf.Score })

	limit := 30
	if limit > len(tmp) {
		limit = len(tmp)
	}

	for _, entry := range tmp[:limit] {
		content, err := os.ReadFile(entry.sf.Record.Path)
		if err != nil {
			continue
		}
		lower := strings.ToLower(string(content))
		totalTokens := tokenbudget.EstimateTokens(string(content))

		for _, term := range terms {
			count := 0
			for _, v := range termVariants(term) {
				count += strings.Count(lower, v)
			}
			if count == 0 {
				continue
			}
			// TF-style: reward frequency but dampen with log.
			bonus := weightContentMatch * math.Log1p(float64(count))
			// Normalise by file size so large files don't dominate unfairly.
			if totalTokens > 0 {
				bonus = bonus * math.Min(1.0, 1000.0/float64(totalTokens))
			}
			scored[entry.pos].Score += bonus
			scored[entry.pos].Reasons = append(scored[entry.pos].Reasons, models.InclusionReason{
				Signal: "content_match",
				Detail: term,
				Weight: bonus,
			})
		}
	}
}

// applyFocusBoost adds a strong score bump to files under any of the
// comma-separated prefixes in focus. Prefixes are compared against the
// forward-slashed relpath so behaviour is identical on Windows and Unix.
func applyFocusBoost(scored []models.ScoredFile, focus string) {
	prefixes := parseFocusPrefixes(focus)
	if len(prefixes) == 0 {
		return
	}
	for i := range scored {
		rel := filepath.ToSlash(scored[i].Record.RelPath)
		for _, p := range prefixes {
			if rel == p || strings.HasPrefix(rel, p+"/") {
				scored[i].Score += weightFocus
				scored[i].Reasons = append(scored[i].Reasons, models.InclusionReason{
					Signal: "focus",
					Detail: p,
					Weight: weightFocus,
				})
				break
			}
		}
	}
}

// applyChangedBoost bumps files whose relpath is in the changed set. The
// caller is responsible for producing the list (see cli/gitchanges.go); any
// format of slashes is normalised here so a caller on Windows is fine.
func applyChangedBoost(scored []models.ScoredFile, changed []string) {
	if len(changed) == 0 {
		return
	}
	set := make(map[string]bool, len(changed))
	for _, c := range changed {
		set[filepath.ToSlash(strings.TrimPrefix(c, "./"))] = true
	}
	for i := range scored {
		rel := filepath.ToSlash(scored[i].Record.RelPath)
		if set[rel] {
			scored[i].Score += weightChanged
			scored[i].Reasons = append(scored[i].Reasons, models.InclusionReason{
				Signal: "changed",
				Detail: rel,
				Weight: weightChanged,
			})
		}
	}
}

// parseFocusPrefixes splits a comma-separated focus argument into cleaned
// path prefixes. Empty entries are dropped so callers can pass raw user
// input without sanitising first.
func parseFocusPrefixes(focus string) []string {
	if strings.TrimSpace(focus) == "" {
		return nil
	}
	raw := strings.Split(focus, ",")
	out := make([]string, 0, len(raw))
	for _, r := range raw {
		r = strings.TrimSpace(r)
		r = strings.TrimPrefix(r, "./")
		r = strings.TrimSuffix(r, "/")
		if r == "" {
			continue
		}
		out = append(out, filepath.ToSlash(r))
	}
	return out
}

// Tokenise exposes the internal tokeniser so callers building scoring
// explanations (e.g. `ask --explain`) can display the exact terms the
// ranker consumed.
func Tokenise(query string) []string {
	return tokenise(query)
}

// SignalWeights returns the scoring weights Rank applies, keyed by signal.
// Useful for rendering an explain table alongside reason-level weights.
func SignalWeights() map[string]float64 {
	return map[string]float64{
		"filename_match":   weightFilename,
		"path_match":       weightPath,
		"symbol_match":     weightSymbol,
		"import_match":     weightImport,
		"import_expansion": weightImportExpansion,
		"content_match":    weightContentMatch,
		"lang_bonus":       weightLangBonus,
		"entry_point":      weightEntryPoint,
		"dependency_match": weightDependencyMatch,
		"focus":            weightFocus,
		"changed":          weightChanged,
		"root_doc":         weightRootDoc,
	}
}

// tokenise splits a query into meaningful terms, filtering out stop-words
// and short tokens. Identifiers in camelCase or PascalCase are also broken
// into their constituent parts so a query mentioning `UpgradeWithSlack`
// produces the tokens `upgrade` and `slack` (in addition to the raw
// `upgradewithslack`), letting the ranker match symbols whose names are
// only sub-strings of the user's term. The original lowercased token is
// kept too so existing whole-word matches keep firing.
func tokenise(query string) []string {
	stopWords := map[string]bool{
		"the": true, "a": true, "an": true, "is": true, "are": true,
		"was": true, "were": true, "in": true, "on": true, "at": true,
		"to": true, "of": true, "and": true, "or": true, "it": true,
		"for": true, "this": true, "that": true, "how": true, "what": true,
		"does": true, "do": true, "did": true, "with": true, "from": true,
		"where": true, "which": true, "can": true, "i": true, "my": true,
	}

	// Split on non-alphanumeric characters, preserving case — splitIdentifier
	// needs the original casing to detect camelCase boundaries.
	rawWords := strings.FieldsFunc(query, func(r rune) bool {
		return !isWordRune(r)
	})

	var terms []string
	seen := make(map[string]bool)
	add := func(t string) {
		t = strings.ToLower(t)
		if len(t) < 3 || stopWords[t] || seen[t] {
			return
		}
		seen[t] = true
		terms = append(terms, t)
	}
	for _, raw := range rawWords {
		add(raw)
		for _, part := range splitIdentifier(raw) {
			add(part)
		}
	}
	return terms
}

// isWordRune mirrors the FieldsFunc predicate used by tokenise: ASCII
// letters, digits, and underscore form a contiguous word.
func isWordRune(r rune) bool {
	return ('a' <= r && r <= 'z') ||
		('A' <= r && r <= 'Z') ||
		('0' <= r && r <= '9') ||
		r == '_'
}

// splitIdentifier breaks a camelCase, PascalCase, or mixed-case identifier
// into its constituent words. Returns nil for inputs that have no
// boundary (already-flat words like "auth" or "MCPSERVER" with no
// trailing camel tail) so tokenise can decide whether to keep just the
// original. The rules are:
//
//	lower→upper: split before the upper            (fooBar → foo|Bar)
//	upper→upper followed by lower: split before    (MCPServer → MCP|Server)
//	digit→letter: split before the letter          (G2Budget → G2|Budget)
//	underscore: split on either side               (foo_bar → foo|bar)
//
// Numbers stay attached to a leading letter run (G2 stays as G2, version2
// stays as version2) so short identifiers with a number suffix survive
// the min-token-length filter.
func splitIdentifier(s string) []string {
	if len(s) == 0 {
		return nil
	}
	type cls int
	const (
		cOther cls = iota
		cLower
		cUpper
		cDigit
		cUnderscore
	)
	classify := func(r rune) cls {
		switch {
		case 'a' <= r && r <= 'z':
			return cLower
		case 'A' <= r && r <= 'Z':
			return cUpper
		case '0' <= r && r <= '9':
			return cDigit
		case r == '_':
			return cUnderscore
		}
		return cOther
	}
	runes := []rune(s)
	n := len(runes)
	if n <= 1 {
		return nil
	}

	var splits []int
	for i := 1; i < n; i++ {
		prev, cur := classify(runes[i-1]), classify(runes[i])
		switch {
		case prev == cLower && cur == cUpper:
			splits = append(splits, i)
		case prev == cUpper && cur == cUpper && i+1 < n && classify(runes[i+1]) == cLower:
			splits = append(splits, i)
		case prev == cDigit && (cur == cLower || cur == cUpper):
			splits = append(splits, i)
		case prev == cUnderscore || cur == cUnderscore:
			splits = append(splits, i)
		}
	}

	if len(splits) == 0 {
		return nil
	}

	parts := make([]string, 0, len(splits)+1)
	start := 0
	for _, sp := range splits {
		seg := strings.Trim(string(runes[start:sp]), "_")
		if seg != "" {
			parts = append(parts, seg)
		}
		start = sp
	}
	if seg := strings.Trim(string(runes[start:]), "_"); seg != "" {
		parts = append(parts, seg)
	}
	return parts
}

// stripExt removes the file extension from a base name.
func stripExt(base string) string {
	ext := filepath.Ext(base)
	if ext == "" {
		return base
	}
	return base[:len(base)-len(ext)]
}
