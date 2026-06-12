// Package retrieval contains reusable chunk-level search used by MCP, CLI,
// benchmarks, and taskflow.
package retrieval

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/neuromfs/neuromfs/internal/config"
	"github.com/neuromfs/neuromfs/internal/embeddings"
	"github.com/neuromfs/neuromfs/internal/fsutil"
	"github.com/neuromfs/neuromfs/internal/indexer"
	"github.com/neuromfs/neuromfs/internal/models"
	"github.com/neuromfs/neuromfs/internal/ranking"
	"github.com/neuromfs/neuromfs/internal/storage"
)

// Options configures a reusable NeuroFS chunk search.
type Options struct {
	Query string
	Repo  string
	Limit int
	Mode  string
}

// Response is the JSON-serializable result returned by chunk search.
type Response struct {
	Query   string `json:"query"`
	Mode    string `json:"mode,omitempty"`
	Results []Hit  `json:"results"`
}

// Hit is a ranked chunk returned by chunk search.
type Hit struct {
	Path          string   `json:"path"`
	StartLine     int      `json:"start_line"`
	EndLine       int      `json:"end_line"`
	Kind          string   `json:"kind"`
	Symbol        string   `json:"symbol,omitempty"`
	Score         float64  `json:"score"`
	Reasons       []string `json:"reasons"`
	TokenEstimate int      `json:"token_estimate"`
	ContentHash   string   `json:"content_hash"`
	Snippet       string   `json:"snippet"`
}

type candidate struct {
	hit      Hit
	filePath string
}

type exactSignal struct {
	filename bool
	lines    map[int]bool
}

const semanticSimilarityThreshold = 0.18
const workingSetBoost = 2.25
const exactContentBoost = 3.75
const exactFilenameBoost = 4.25
const longChunkTokenThreshold = 500
const longChunkRelativeFactor = 2
const maxLongChunkPenalty = 4.0

// Search runs chunk-level retrieval against a repo index.
func Search(ctx context.Context, opts Options) (Response, error) {
	query := strings.TrimSpace(opts.Query)
	if query == "" {
		return Response{}, fmt.Errorf("query must not be empty")
	}
	if opts.Limit <= 0 {
		opts.Limit = 8
	}
	if opts.Limit > 50 {
		opts.Limit = 50
	}

	repo, err := resolveRepo(opts.Repo)
	if err != nil {
		return Response{}, err
	}
	cfg, err := config.New(repo)
	if err != nil {
		return Response{}, err
	}
	db, err := storage.Open(cfg.DBPath)
	if err != nil {
		return Response{}, err
	}
	defer db.Close()

	files, err := db.AllFiles()
	if err != nil {
		return Response{}, err
	}
	chunks, err := db.AllChunks()
	if err != nil {
		return Response{}, err
	}
	if len(files) == 0 || len(chunks) == 0 {
		if _, err := indexer.Run(cfg, db, indexer.Options{Logf: func(string, ...any) {}}); err != nil {
			return Response{}, err
		}
		files, err = db.AllFiles()
		if err != nil {
			return Response{}, err
		}
		chunks, err = db.AllChunks()
		if err != nil {
			return Response{}, err
		}
	}

	chunkEmbeddings, err := db.AllChunkEmbeddings()
	if err != nil {
		return Response{}, fmt.Errorf("load chunk embeddings: %w", err)
	}
	relations, err := db.AllRelations()
	if err != nil {
		return Response{}, fmt.Errorf("load dependency graph: %w", err)
	}

	var queryEmbedding []float32
	if len(chunkEmbeddings) > 0 {
		embClient := embeddings.NewClient(cfg.HybridMode)
		queryEmbedding, err = embClient.GetEmbedding(ctx, query)
		if err != nil {
			queryEmbedding = nil
		}
	}

	terms := ranking.Tokenise(query)
	exactSignals := exactSearchSignals(ctx, repo, terms, files)
	changedPaths := changedPathSet(fsutil.GitChangedFiles(repo))

	type structMatch struct {
		symbolMatches []string
		importMatches []string
	}
	structuralMatches := make(map[string]structMatch)
	for _, file := range files {
		var matches structMatch
		for _, sym := range file.Symbols {
			if textMatchesTerms(sym.Name, terms) {
				matches.symbolMatches = append(matches.symbolMatches, sym.Name)
			}
		}
		for _, imp := range file.Imports {
			if textMatchesTerms(imp, terms) {
				matches.importMatches = append(matches.importMatches, imp)
			}
		}
		if len(matches.symbolMatches) > 0 || len(matches.importMatches) > 0 {
			structuralMatches[file.Path] = matches
		}
	}

	fileByPath := make(map[string]models.FileRecord, len(files))
	for _, f := range files {
		fileByPath[f.Path] = f
	}

	contentCache := make(map[string]string)
	candidates := make([]candidate, 0, len(chunks))
	for _, chunk := range chunks {
		rec, ok := fileByPath[chunk.FilePath]
		if !ok {
			continue
		}
		content, ok := contentCache[rec.Path]
		if !ok {
			absPath, err := fsutil.ConfineToRepoStrict(repo, rec.RelPath)
			if err != nil {
				continue
			}
			b, err := os.ReadFile(absPath)
			if err != nil {
				continue
			}
			content = string(b)
			contentCache[rec.Path] = content
		}
		snippet := snippetForRange(content, chunk.StartLine, chunk.EndLine)
		score, reasons := scoreChunkHit(rec, chunk, snippet, terms)
		hit := Hit{
			Path:          rec.RelPath,
			StartLine:     chunk.StartLine,
			EndLine:       chunk.EndLine,
			Kind:          chunk.Kind,
			Symbol:        chunk.Symbol,
			Score:         score,
			Reasons:       reasons,
			TokenEstimate: chunk.TokenEstimate,
			ContentHash:   chunk.ContentHash,
			Snippet:       snippet,
		}
		if matches, ok := structuralMatches[rec.Path]; ok {
			if len(matches.symbolMatches) > 0 {
				var symBoost float64
				for _, name := range matches.symbolMatches {
					symBoost += symbolScore(name, terms)
				}
				if symBoost > 18.0 {
					symBoost = 18.0
				}
				addReason(&hit, "structural_symbol", symBoost)
			}
			if len(matches.importMatches) > 0 {
				impBoost := float64(len(matches.importMatches)) * 2.0
				if impBoost > 10.0 {
					impBoost = 10.0
				}
				addReason(&hit, "structural_import", impBoost)
			}
		}
		if len(queryEmbedding) > 0 {
			if chunkEmbedding, ok := chunkEmbeddings[chunk.ContentHash]; ok {
				if sim := embeddings.CosineSimilarity(queryEmbedding, chunkEmbedding); sim >= semanticSimilarityThreshold {
					addReason(&hit, "semantic_match", semanticBoost(sim))
				}
			}
		}
		candidates = append(candidates, candidate{
			hit:      hit,
			filePath: rec.Path,
		})
	}

	applyExactBoost(candidates, exactSignals)
	applyWorkingSetBoost(candidates, changedPaths)
	applyGraphBoost(candidates, relations)
	applyLongChunkPenalty(candidates)
	applyTestPenalty(candidates, query)

	hits := make([]Hit, 0, len(candidates))
	for _, candidate := range candidates {
		if candidate.hit.Score <= 0 {
			continue
		}
		hits = append(hits, candidate.hit)
	}

	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].Score != hits[j].Score {
			return hits[i].Score > hits[j].Score
		}
		if hits[i].Path != hits[j].Path {
			return hits[i].Path < hits[j].Path
		}
		if hits[i].StartLine != hits[j].StartLine {
			return hits[i].StartLine < hits[j].StartLine
		}
		return hits[i].Symbol < hits[j].Symbol
	})

	hits = dedupeSameSymbol(hits)

	// Enforce diversity: allow at most 3 chunks per file in the final search results
	const maxChunksPerFile = 3
	filteredHits := make([]Hit, 0, len(hits))
	fileCounts := make(map[string]int)
	for _, hit := range hits {
		if fileCounts[hit.Path] < maxChunksPerFile {
			filteredHits = append(filteredHits, hit)
			fileCounts[hit.Path]++
		}
	}
	hits = filteredHits

	if len(hits) > opts.Limit {
		hits = hits[:opts.Limit]
	}

	return Response{
		Query:   query,
		Mode:    strings.TrimSpace(opts.Mode),
		Results: hits,
	}, nil
}

// dedupeSameSymbol keeps one hit per (path, symbol) for named chunks. Python
// @t.overload and TS .d.ts overloads index the same symbol several times as
// near-identical stubs; left alone they fill the per-file diversity cap with
// copies of one declaration and squeeze every other symbol in that file out
// of the results. Among duplicates the largest chunk wins (the implementation
// body, not a stub) with ties going to the earlier, higher-scored occurrence.
// Hits must already be sorted by score; the kept hit stays at the position of
// the first occurrence so ordering is preserved.
func dedupeSameSymbol(hits []Hit) []Hit {
	type symKey struct{ path, symbol string }
	keptAt := make(map[symKey]int)
	out := make([]Hit, 0, len(hits))
	for _, h := range hits {
		if h.Symbol == "" || h.Kind == "file" {
			out = append(out, h)
			continue
		}
		k := symKey{h.Path, h.Symbol}
		if i, seen := keptAt[k]; seen {
			if h.TokenEstimate > out[i].TokenEstimate {
				// Same declaration, bigger body — replace in place, but keep
				// the first occurrence's (higher or equal) score so the swap
				// never promotes a duplicate above where its symbol ranked.
				score, reasons := out[i].Score, out[i].Reasons
				out[i] = h
				out[i].Score, out[i].Reasons = score, reasons
			}
			continue
		}
		keptAt[k] = len(out)
		out = append(out, h)
	}
	return out
}

func resolveRepo(path string) (string, error) {
	repo := strings.TrimSpace(path)
	if repo == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return "", fmt.Errorf("resolve cwd: %w", err)
		}
		repo = cwd
	}
	abs, err := filepath.Abs(repo)
	if err != nil {
		return "", fmt.Errorf("resolve repo: %w", err)
	}
	return abs, nil
}

func scoreChunkHit(rec models.FileRecord, chunk models.Chunk, snippet string, terms []string) (float64, []string) {
	var score float64
	var reasons []string
	add := func(reason string, weight float64) {
		score += weight
		if !containsString(reasons, reason) {
			reasons = append(reasons, reason)
		}
	}

	if textMatchesTerms(chunk.Symbol, terms) {
		add("symbol_match", 8.0)
	}
	// A query term that *equals* the symbol name (or its last dotted
	// component) is much stronger evidence than the substring matching
	// above — the question literally names this identifier. This is the
	// discriminator inside one file, where the file-level structural boosts
	// are identical for every chunk and substring symbol matches tie.
	if symbolExactlyNamed(chunk.Symbol, terms) {
		add("symbol_exact", 6.0)
	}
	baseStem := stripExt(filepath.Base(rec.RelPath))
	if textMatchesTerms(rec.RelPath, terms) || textMatchesTerms(baseStem, terms) {
		add("path_match", 3.0)
	}
	if textMatchesTerms(chunk.Kind, terms) {
		add("kind_match", 1.0)
	}

	contentHits := 0
	for _, term := range terms {
		if termMatchesText(term, snippet) {
			contentHits++
		}
	}
	if contentHits > 0 {
		if contentHits > 3 {
			contentHits = 3
		}
		add("content_match", float64(contentHits)*2.0)
	}
	if chunk.Kind != "file" && score > 0 {
		add("chunk_scope", 0.5)
	}
	return score, reasons
}

func semanticBoost(similarity float64) float64 {
	if similarity > 1 {
		similarity = 1
	}
	boost := 1.0 + ((similarity - semanticSimilarityThreshold) * 8.0)
	if boost < 1.0 {
		return 1.0
	}
	if boost > 8.0 {
		return 8.0
	}
	return boost
}

func applyExactBoost(candidates []candidate, signals map[string]exactSignal) {
	if len(candidates) == 0 || len(signals) == 0 {
		return
	}
	for i := range candidates {
		signal, ok := signals[candidates[i].hit.Path]
		if !ok {
			continue
		}
		if signal.filename {
			addReason(&candidates[i].hit, "exact_filename", exactFilenameBoost)
		}
		if linesOverlap(signal.lines, candidates[i].hit.StartLine, candidates[i].hit.EndLine) {
			addReason(&candidates[i].hit, "exact_content", exactContentBoost)
		}
	}
}

func exactSearchSignals(ctx context.Context, repo string, terms []string, files []models.FileRecord) map[string]exactSignal {
	signals := exactFilenameSignals(terms, files)
	patterns := exactSearchTerms(terms)
	if len(patterns) == 0 {
		return signals
	}
	if _, err := exec.LookPath("rg"); err != nil {
		return signals
	}

	rgCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()

	args := []string{
		"--json",
		"--ignore-case",
		"--word-regexp",
		"--glob", "!.git/**",
		"--glob", "!.neurofs/**",
	}
	for _, pattern := range patterns {
		args = append(args, "-e", pattern)
	}
	args = append(args, repo)

	var out bytes.Buffer
	cmd := exec.CommandContext(rgCtx, "rg", args...)
	cmd.Stdout = &out
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
			return signals
		}
		return signals
	}

	dec := json.NewDecoder(bytes.NewReader(out.Bytes()))
	for {
		var event struct {
			Type string `json:"type"`
			Data struct {
				Path struct {
					Text string `json:"text"`
				} `json:"path"`
				LineNumber int `json:"line_number"`
			} `json:"data"`
		}
		if err := dec.Decode(&event); err != nil {
			if err == io.EOF {
				break
			}
			return signals
		}
		if event.Type != "match" || event.Data.LineNumber <= 0 {
			continue
		}
		relPath := normalizeRGPath(repo, event.Data.Path.Text)
		if relPath == "" {
			continue
		}
		signal := signals[relPath]
		if signal.lines == nil {
			signal.lines = make(map[int]bool)
		}
		signal.lines[event.Data.LineNumber] = true
		signals[relPath] = signal
	}
	return signals
}

func exactFilenameSignals(terms []string, files []models.FileRecord) map[string]exactSignal {
	signals := make(map[string]exactSignal)
	if len(terms) == 0 {
		return signals
	}
	termSet := make(map[string]bool, len(terms))
	for _, term := range terms {
		term = strings.ToLower(strings.TrimSpace(term))
		if term != "" {
			termSet[term] = true
		}
	}
	for _, file := range files {
		if filenameMatchesExactTerm(file.RelPath, termSet) {
			signal := signals[file.RelPath]
			signal.filename = true
			signals[file.RelPath] = signal
		}
	}
	return signals
}

func filenameMatchesExactTerm(relPath string, termSet map[string]bool) bool {
	base := strings.ToLower(filepath.Base(relPath))
	stem := base
	if ext := filepath.Ext(base); ext != "" {
		stem = strings.TrimSuffix(base, ext)
	}
	candidates := []string{base, stem}
	for _, part := range splitIdentifierForSearch(stem) {
		candidates = append(candidates, part)
	}
	for _, candidate := range candidates {
		if termSet[candidate] {
			return true
		}
	}
	return false
}

func exactSearchTerms(terms []string) []string {
	const maxPatterns = 12
	out := make([]string, 0, len(terms))
	seen := make(map[string]bool, len(terms))
	for _, term := range terms {
		term = strings.ToLower(strings.TrimSpace(term))
		if len(term) < 3 || seen[term] {
			continue
		}
		seen[term] = true
		out = append(out, term)
		if len(out) >= maxPatterns {
			break
		}
	}
	return out
}

func normalizeRGPath(repo, path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	if filepath.IsAbs(path) {
		rel, err := filepath.Rel(repo, path)
		if err == nil {
			path = rel
		}
	}
	path = filepath.ToSlash(path)
	path = strings.TrimPrefix(path, "./")
	return path
}

func applyWorkingSetBoost(candidates []candidate, changedPaths map[string]bool) {
	if len(candidates) == 0 || len(changedPaths) == 0 {
		return
	}

	bridge := selectWorkingSetBridgeCandidates(candidates, changedPaths)
	for i := range candidates {
		if !changedPaths[candidates[i].hit.Path] {
			continue
		}
		if candidates[i].hit.Score <= 0 && !bridge[candidateKey(candidates[i])] {
			continue
		}
		addReason(&candidates[i].hit, "working_set", workingSetBoost)
	}
}

func selectWorkingSetBridgeCandidates(candidates []candidate, changedPaths map[string]bool) map[string]bool {
	selected := make(map[string]bool)
	seenPath := make(map[string]bool)
	for _, candidate := range candidates {
		if candidate.hit.Score > 0 || seenPath[candidate.hit.Path] {
			continue
		}
		if !changedPaths[candidate.hit.Path] {
			continue
		}
		selected[candidateKey(candidate)] = true
		seenPath[candidate.hit.Path] = true
	}
	return selected
}

func applyGraphBoost(candidates []candidate, relations []models.FileRelation) {
	if len(candidates) == 0 || len(relations) == 0 {
		return
	}

	seeds := seedPathsForCandidates(candidates, 8)
	if len(seeds) == 0 {
		return
	}

	related := make(map[string]string)
	for _, rel := range relations {
		if seeds[rel.SourcePath] {
			related[rel.TargetPath] = "graph_dependency"
		}
		if seeds[rel.TargetPath] {
			related[rel.SourcePath] = "graph_dependent"
		}
	}
	if len(related) == 0 {
		return
	}

	graphBridge := selectGraphBridgeCandidates(candidates, related)
	for i := range candidates {
		reason, ok := related[candidates[i].filePath]
		if !ok {
			continue
		}
		if candidates[i].hit.Score <= 0 && !graphBridge[candidateKey(candidates[i])] {
			continue
		}
		addReason(&candidates[i].hit, reason, 1.25)
	}
}

func applyLongChunkPenalty(candidates []candidate) {
	if len(candidates) == 0 {
		return
	}

	smallest := 0
	for _, candidate := range candidates {
		if candidate.hit.Score <= 0 || candidate.hit.TokenEstimate <= 0 {
			continue
		}
		if smallest == 0 || candidate.hit.TokenEstimate < smallest {
			smallest = candidate.hit.TokenEstimate
		}
	}
	if smallest == 0 || smallest >= longChunkTokenThreshold {
		return
	}

	for i := range candidates {
		tokens := candidates[i].hit.TokenEstimate
		if candidates[i].hit.Score <= 0 || tokens < longChunkTokenThreshold {
			continue
		}
		if tokens < smallest*longChunkRelativeFactor {
			continue
		}
		penalty := float64(tokens-smallest) / 250.0
		if penalty < 1.0 {
			penalty = 1.0
		}
		if penalty > maxLongChunkPenalty {
			penalty = maxLongChunkPenalty
		}
		addPenalty(&candidates[i].hit, "long_chunk_penalty", penalty)
	}
}

func applyTestPenalty(candidates []candidate, query string) {
	wantsTests := ranking.QueryWantsTests(query)
	for i := range candidates {
		if !ranking.IsTestLikePath(candidates[i].hit.Path) {
			continue
		}
		if wantsTests {
			addReason(&candidates[i].hit, "query_test_intent_detected", 0)
			continue
		}
		if candidates[i].hit.Score <= 0 {
			continue
		}
		before := candidates[i].hit.Score
		candidates[i].hit.Score = before * 0.72
		addPenalty(&candidates[i].hit, "test_like_downrank", before-candidates[i].hit.Score)
	}
}

func seedPathsForCandidates(candidates []candidate, limit int) map[string]bool {
	eligible := make([]candidate, 0, len(candidates))
	for _, candidate := range candidates {
		if candidate.hit.Score > 0 {
			eligible = append(eligible, candidate)
		}
	}
	sort.SliceStable(eligible, func(i, j int) bool {
		if eligible[i].hit.Score != eligible[j].hit.Score {
			return eligible[i].hit.Score > eligible[j].hit.Score
		}
		if eligible[i].hit.Path != eligible[j].hit.Path {
			return eligible[i].hit.Path < eligible[j].hit.Path
		}
		return eligible[i].hit.StartLine < eligible[j].hit.StartLine
	})

	seeds := make(map[string]bool)
	for _, candidate := range eligible {
		seeds[candidate.filePath] = true
		if limit > 0 && len(seeds) >= limit {
			break
		}
	}
	return seeds
}

func selectGraphBridgeCandidates(candidates []candidate, related map[string]string) map[string]bool {
	selected := make(map[string]bool)
	seenPath := make(map[string]bool)
	for _, candidate := range candidates {
		if candidate.hit.Score > 0 || seenPath[candidate.filePath] {
			continue
		}
		if _, ok := related[candidate.filePath]; !ok {
			continue
		}
		selected[candidateKey(candidate)] = true
		seenPath[candidate.filePath] = true
	}
	return selected
}

func candidateKey(candidate candidate) string {
	return fmt.Sprintf("%s|%s|%d", candidate.filePath, candidate.hit.ContentHash, candidate.hit.StartLine)
}

func addReason(hit *Hit, reason string, weight float64) {
	hit.Score += weight
	if !containsString(hit.Reasons, reason) {
		hit.Reasons = append(hit.Reasons, reason)
	}
}

func addPenalty(hit *Hit, reason string, weight float64) {
	hit.Score -= weight
	if hit.Score < 0 {
		hit.Score = 0
	}
	if !containsString(hit.Reasons, reason) {
		hit.Reasons = append(hit.Reasons, reason)
	}
}

func changedPathSet(paths []string) map[string]bool {
	if len(paths) == 0 {
		return nil
	}
	set := make(map[string]bool, len(paths))
	for _, path := range paths {
		path = filepath.ToSlash(strings.TrimSpace(path))
		if path == "" {
			continue
		}
		set[path] = true
	}
	return set
}

func textMatchesTerms(text string, terms []string) bool {
	for _, term := range terms {
		if termMatchesText(term, text) {
			return true
		}
	}
	return false
}

func termMatchesText(term, text string) bool {
	term = strings.ToLower(term)
	text = strings.ToLower(text)
	if text == "" || term == "" {
		return false
	}
	tvars := ranking.TermVariants(term)
	pvars := ranking.TermVariants(text)
	for _, t := range tvars {
		if len(t) < 3 {
			continue
		}
		for _, p := range pvars {
			if len(p) < 3 {
				continue
			}
			if strings.Contains(p, t) || strings.Contains(t, p) {
				return true
			}
		}
	}
	return false
}

// symbolExactlyNamed reports whether any query term is equal (case-insensitive)
// to the chunk's symbol or to its last dotted component, e.g. the term
// "upgradewithslack" against symbol "UpgradeWithSlack", or "invoke" against
// "CliRunner.invoke". Tokenise keeps the raw lowercased token alongside its
// camelCase splits, so multi-word identifiers written verbatim in the query
// still compare equal here.
func symbolExactlyNamed(symbol string, terms []string) bool {
	sym := strings.ToLower(strings.TrimSpace(symbol))
	if sym == "" {
		return false
	}
	last := sym
	if dot := strings.LastIndex(sym, "."); dot >= 0 && dot+1 < len(sym) {
		last = sym[dot+1:]
	}
	for _, term := range terms {
		t := strings.ToLower(strings.TrimSpace(term))
		if t == "" {
			continue
		}
		if t == sym || t == last {
			return true
		}
	}
	return false
}

func symbolScore(symbol string, terms []string) float64 {
	lower := strings.ToLower(symbol)
	for _, term := range terms {
		term = strings.ToLower(strings.TrimSpace(term))
		if term == "" {
			continue
		}
		tvars := ranking.TermVariants(term)
		pvars := ranking.TermVariants(lower)
		for _, tv := range tvars {
			for _, pv := range pvars {
				if tv == pv {
					return 18.0
				}
			}
		}
	}
	return 3.0
}

func stripExt(base string) string {
	ext := filepath.Ext(base)
	if ext == "" {
		return base
	}
	return base[:len(base)-len(ext)]
}

func linesOverlap(lines map[int]bool, startLine, endLine int) bool {
	if len(lines) == 0 {
		return false
	}
	for line := range lines {
		if line >= startLine && line <= endLine {
			return true
		}
	}
	return false
}

func splitIdentifierForSearch(s string) []string {
	var parts []string
	for _, part := range strings.FieldsFunc(s, func(r rune) bool {
		return !(('a' <= r && r <= 'z') ||
			('A' <= r && r <= 'Z') ||
			('0' <= r && r <= '9'))
	}) {
		part = strings.ToLower(part)
		if len(part) >= 3 {
			parts = append(parts, part)
		}
	}
	return parts
}

func containsString(items []string, needle string) bool {
	for _, item := range items {
		if item == needle {
			return true
		}
	}
	return false
}

func snippetForRange(content string, startLine, endLine int) string {
	return linesInRange(splitLogicalLines(content), startLine, endLine)
}

func splitLogicalLines(content string) []string {
	if content == "" {
		return []string{""}
	}
	lines := strings.Split(content, "\n")
	if strings.HasSuffix(content, "\n") {
		lines = lines[:len(lines)-1]
	}
	if len(lines) == 0 {
		return []string{""}
	}
	return lines
}

func linesInRange(lines []string, startLine, endLine int) string {
	if len(lines) == 0 {
		return ""
	}
	if startLine < 1 {
		startLine = 1
	}
	if endLine > len(lines) {
		endLine = len(lines)
	}
	if endLine < startLine {
		return ""
	}
	return strings.Join(lines[startLine-1:endLine], "\n")
}
