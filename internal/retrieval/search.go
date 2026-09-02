// Package retrieval contains reusable chunk-level search used by MCP, CLI,
// benchmarks, and taskflow.
package retrieval

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Gere2/neurofs/internal/config"
	"github.com/Gere2/neurofs/internal/embeddings"
	"github.com/Gere2/neurofs/internal/fsutil"
	"github.com/Gere2/neurofs/internal/indexer"
	"github.com/Gere2/neurofs/internal/models"
	"github.com/Gere2/neurofs/internal/ranking"
	"github.com/Gere2/neurofs/internal/storage"
)

// Options configures a reusable NeuroFS chunk search.
type Options struct {
	Query string
	Repo  string
	Limit int
	Mode  string
	// DisableIndexRefresh makes one-shot search consume the existing index
	// exactly as stored. It disables version, embedding, source-generation,
	// and empty-index rebuilds. The index is opened read-only and must already
	// exist. Measurement callers use this to avoid changing what they measure.
	DisableIndexRefresh bool
	// ExpandStructuralContext appends a bounded number of matching class
	// headers and explicitly named members after the ranked limit. Task
	// bundles enable it so unused token budget can carry cheap structural
	// context without displacing primary search hits.
	ExpandStructuralContext bool
	// Weights overrides the scoring weights for this search. When nil the
	// repo's tuned weights (.neurofs/weights.json) or defaults are used.
	// The learn tuner injects candidates here to evaluate them.
	Weights *Weights
	// NeutralizeGitState drops the working-set boost for this query.
	// Benchmarks and the tuner set it: the boost is deliberately
	// situational (recently edited files rank higher), which makes
	// measurements depend on whatever happens to be dirty — measured
	// drift: the context bench read 9/12 on a dirty tree and 8/12 on the
	// same code clean. Regression gates must measure the tree-independent
	// engine; production search keeps the boost.
	NeutralizeGitState bool
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
	ChunkID       string   `json:"-"`
	ParentID      string   `json:"-"`
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
const longChunkTokenThreshold = 500
const longChunkRelativeFactor = 2

// Session holds a loaded repo index for repeated searches. Opening the
// database, loading files/chunks/embeddings/relations, and reading file
// contents dominate a one-shot Search; callers that run many queries
// against the same index — the learn tuner above all, at hundreds of
// evaluations per tune — pay that cost once here instead of per query.
// A Session snapshots the index at creation: reindexing or file edits
// after NewSession are not visible to it.
type Session struct {
	repo            string
	hybridMode      bool
	files           []models.FileRecord
	chunks          []models.Chunk
	chunkEmbeddings map[string][]float32
	relations       []models.FileRelation
	fileByPath      map[string]models.FileRecord
	contentCache    map[string]string
	changedPaths    map[string]bool
	// exactCache memoizes checksum-validated exact signals per term set: the
	// tuner replays the same fixture questions hundreds of times per run.
	exactCache map[string]map[string]exactSignal
}

// NewSession loads the repo index once for repeated searches, refreshing it
// first when its version or indexed source generations are stale.
func NewSession(ctx context.Context, repoPath string) (*Session, error) {
	return newSession(ctx, repoPath, false)
}

// SnapshotFiles returns the file records captured with this session's index
// generation. Measurement callers use the copy as their whole-file baseline
// so snippets and native reads cannot accidentally straddle an auto-reindex.
func (s *Session) SnapshotFiles() []models.FileRecord {
	if s == nil {
		return nil
	}
	return append([]models.FileRecord(nil), s.files...)
}

func newSession(ctx context.Context, repoPath string, disableIndexRefresh bool) (*Session, error) {
	repo, err := resolveRepo(repoPath)
	if err != nil {
		return nil, err
	}
	cfg, err := config.New(repo)
	if err != nil {
		return nil, err
	}
	var db *storage.DB
	if disableIndexRefresh {
		db, err = storage.OpenReadOnly(cfg.DBPath)
	} else {
		db, err = storage.Open(cfg.DBPath)
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = db.Close() }()

	embeddingClient := embeddings.NewClient(cfg.HybridMode)
	if err := embeddingClient.Validate(); err != nil {
		return nil, fmt.Errorf("validate embedding configuration: %w", err)
	}

	requiresReindex := false
	if !disableIndexRefresh {
		requiresReindex, err = indexer.RequiresReindex(db)
		if err != nil {
			return nil, fmt.Errorf("check indexer version: %w", err)
		}
		if !requiresReindex {
			requiresReindex, err = indexer.RequiresEmbeddingReindex(
				db,
				embeddingClient.ProviderName(),
				embeddingClient.ModelName(),
			)
			if err != nil {
				return nil, fmt.Errorf("check embedding index freshness: %w", err)
			}
		}
		if !requiresReindex {
			requiresReindex, err = indexer.RequiresSourceReindex(cfg, db)
			if err != nil {
				return nil, fmt.Errorf("check source index freshness: %w", err)
			}
		}
		if requiresReindex {
			if _, err := indexer.Run(cfg, db, indexer.Options{Logf: func(string, ...any) {}}); err != nil {
				return nil, fmt.Errorf("rebuild stale index: %w", err)
			}
		}
	}

	files, err := db.AllFiles()
	if err != nil {
		return nil, err
	}
	chunks, err := db.AllChunks()
	if err != nil {
		return nil, err
	}
	if disableIndexRefresh && (len(files) == 0 || len(chunks) == 0) {
		return nil, fmt.Errorf("index is empty; automatic refresh is disabled")
	}
	if !disableIndexRefresh && !requiresReindex && (len(files) == 0 || len(chunks) == 0) {
		if _, err := indexer.Run(cfg, db, indexer.Options{Logf: func(string, ...any) {}}); err != nil {
			return nil, err
		}
		files, err = db.AllFiles()
		if err != nil {
			return nil, err
		}
		chunks, err = db.AllChunks()
		if err != nil {
			return nil, err
		}
	}

	// Mock embeddings (keyless installs) are deterministic pseudo-random
	// vectors: cosine similarity between them sits ~7 standard deviations
	// below the 0.18 semantic threshold, so semantic_match never fires —
	// measured identical recall/tokens with the signal on and off across
	// three corpora (2026-07-04). Loading megabytes of them per session
	// and running the cosine loop per query is pure cost; skip the whole
	// semantic path unless a real provider is configured.
	// NEUROFS_MOCK_SEMANTIC=1 keeps the path alive under the mock provider
	// so tests can exercise the semantic plumbing with planted vectors.
	var chunkEmbeddings map[string][]float32
	if embeddingClient.ProviderName() != "mock" || os.Getenv("NEUROFS_MOCK_SEMANTIC") == "1" {
		chunkEmbeddings, err = db.AllChunkEmbeddings()
		if err != nil {
			return nil, fmt.Errorf("load chunk embeddings: %w", err)
		}
	}
	relations, err := db.AllRelations()
	if err != nil {
		return nil, fmt.Errorf("load dependency graph: %w", err)
	}

	fileByPath := make(map[string]models.FileRecord, len(files))
	for _, f := range files {
		fileByPath[f.Path] = f
	}

	return &Session{
		repo:            repo,
		hybridMode:      cfg.HybridMode,
		files:           files,
		chunks:          chunks,
		chunkEmbeddings: chunkEmbeddings,
		relations:       relations,
		fileByPath:      fileByPath,
		contentCache:    make(map[string]string),
		changedPaths:    changedPathSet(fsutil.GitChangedFiles(repo)),
		exactCache:      make(map[string]map[string]exactSignal),
	}, nil
}

// Search runs chunk-level retrieval against a repo index, loading the
// index for this single query. For repeated queries use NewSession.
func Search(ctx context.Context, opts Options) (Response, error) {
	if strings.TrimSpace(opts.Query) == "" {
		return Response{}, fmt.Errorf("query must not be empty")
	}
	session, err := newSession(ctx, opts.Repo, opts.DisableIndexRefresh)
	if err != nil {
		return Response{}, err
	}
	return session.Search(ctx, opts)
}

// Search runs one query against the session's loaded index. Options.Repo
// is ignored — the session is already bound to a repo.
func (s *Session) Search(ctx context.Context, opts Options) (Response, error) {
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

	repo := s.repo
	weights := opts.Weights
	if weights == nil {
		// Malformed weights.json falls back to defaults on purpose: an
		// optional tuning file must never take retrieval down. `neurofs
		// learn status` surfaces the parse error.
		loaded, _, _ := LoadWeights(repo)
		weights = &loaded
	}

	files := s.files
	chunks := s.chunks
	chunkEmbeddings := s.chunkEmbeddings
	relations := s.relations

	var queryEmbedding []float32
	if len(chunkEmbeddings) > 0 {
		queryEmbedding = cachedQueryEmbedding(ctx, s.hybridMode, query)
	}

	terms := ranking.Tokenise(query)
	exactKey := strings.Join(terms, "\x00")
	changedPaths := s.changedPaths
	if opts.NeutralizeGitState {
		changedPaths = nil
	}

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

	candidates := make([]candidate, 0, len(chunks))
	unreadablePaths := make(map[string]struct{})
	for _, chunk := range chunks {
		rec, ok := s.fileByPath[chunk.FilePath]
		if !ok {
			continue
		}
		if _, unreadable := unreadablePaths[rec.Path]; unreadable {
			continue
		}
		content, ok := s.contentCache[rec.Path]
		if !ok {
			absPath, err := fsutil.ConfineToRepoStrict(repo, rec.RelPath)
			if err != nil {
				unreadablePaths[rec.Path] = struct{}{}
				continue
			}
			readRecord := rec
			readRecord.Path = absPath
			b, _, err := fsutil.ReadIndexedFileBounded(readRecord, config.MaxFileSize)
			if err != nil {
				unreadablePaths[rec.Path] = struct{}{}
				continue
			}
			content = string(b)
			s.contentCache[rec.Path] = content
		}
		snippet := snippetForRange(content, chunk.StartLine, chunk.EndLine)
		score, reasons := scoreChunkHit(rec, chunk, snippet, terms, weights)
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
			ChunkID:       chunk.ChunkID,
			ParentID:      chunk.ParentID,
		}
		if matches, ok := structuralMatches[rec.Path]; ok {
			if len(matches.symbolMatches) > 0 {
				var symBoost float64
				for _, name := range matches.symbolMatches {
					symBoost += symbolScore(name, terms, weights)
				}
				if symBoost > weights.StructuralSymbol {
					symBoost = weights.StructuralSymbol
				}
				addReason(&hit, "structural_symbol", symBoost)
			}
			if len(matches.importMatches) > 0 {
				impBoost := float64(len(matches.importMatches)) * weights.StructuralImport
				if impBoost > 10.0 {
					impBoost = 10.0
				}
				addReason(&hit, "structural_import", impBoost)
			}
		}
		if len(queryEmbedding) > 0 {
			if chunkEmbedding, ok := chunkEmbeddings[chunk.ContentHash]; ok {
				if sim := embeddings.CosineSimilarity(queryEmbedding, chunkEmbedding); sim >= semanticSimilarityThreshold {
					addReason(&hit, "semantic_match", semanticBoost(sim, weights))
				}
			}
		}
		candidates = append(candidates, candidate{
			hit:      hit,
			filePath: rec.Path,
		})
	}

	exactSignals, cached := s.exactCache[exactKey]
	if !cached {
		exactSignals = exactSearchSignals(terms, files, s.contentCache)
		s.cacheExactSignals(exactKey, exactSignals)
	}
	applyExactBoost(candidates, exactSignals, weights)
	applyWorkingSetBoost(candidates, changedPaths, weights)
	applyGraphBoost(candidates, relations, weights)
	applyLongChunkPenalty(candidates, weights)
	applyTinyChunkPenalty(candidates, weights)
	applyTestPenalty(candidates, query, weights)
	applyLegacyPathPenalty(candidates, query, weights)

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
	rankedHits := hits

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
	if opts.ExpandStructuralContext {
		hits = appendStructuralContext(hits, rankedHits, terms)
	}

	return Response{
		Query:   query,
		Mode:    strings.TrimSpace(opts.Mode),
		Results: hits,
	}, nil
}

func (s *Session) cacheExactSignals(key string, signals map[string]exactSignal) {
	if len(s.exactCache) >= maxExactCacheLen {
		s.exactCache = make(map[string]map[string]exactSignal)
	}
	s.exactCache[key] = signals
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

const (
	maxExpandedClassHeaders        = 2
	maxExpandedClassMembers        = 2
	maxExpandedCompanions          = 3
	maxExpandedFileAnchors         = 1
	maxExpandedCompoundFileHelpers = 2
	maxExpandedChunkTokens         = 2000
)

// appendStructuralContext uses only additive slack: it never reorders or
// removes primary hits. Large files often need more than the global top-N to
// explain a result — for example a matched CliRunner.invoke method benefits
// from its CliRunner header, while a plural query for "arguments" should be
// able to carry the compact Argument class even when other chunks from the
// same core file fill the diversity cap.
func appendStructuralContext(selected, ranked []Hit, terms []string) []Hit {
	if len(selected) == 0 || len(ranked) == 0 {
		return selected
	}
	out := append([]Hit(nil), selected...)
	selectedPaths := make(map[string]bool)
	seen := make(map[string]bool)
	for _, hit := range out {
		selectedPaths[hit.Path] = true
		seen[hitIdentity(hit)] = true
	}

	// If an exactly named method already survived primary ranking, its own
	// class is the most useful header. Prefer that parent before generic
	// class names that merely share a query term (for example _FDCapture
	// before CliRunner in a query about CliRunner.invoke).
	wantedParents := make(map[string]bool)
	for _, hit := range out {
		if hit.ParentID != "" && symbolExactlyNamed(hit.Symbol, terms) {
			wantedParents[parentIdentity(hit.Path, hit.ParentID)] = true
		}
	}

	matchingParents := make(map[string]bool)
	for _, hit := range ranked {
		if !selectedPaths[hit.Path] || !isClassHeaderKind(hit.Kind) ||
			!classSymbolMatchesQuery(hit.Symbol, terms) {
			continue
		}
		matchingParents[parentIdentity(hit.Path, hit.ChunkID)] = true
	}

	addedHeaders := 0
	appendHeaders := func(wantedOnly bool) {
		for _, hit := range ranked {
			if addedHeaders >= maxExpandedClassHeaders {
				return
			}
			parentKey := parentIdentity(hit.Path, hit.ChunkID)
			if !selectedPaths[hit.Path] || !isClassHeaderKind(hit.Kind) ||
				!matchingParents[parentKey] ||
				(wantedOnly && !wantedParents[parentKey]) ||
				(!wantedOnly && wantedParents[parentKey]) ||
				seen[hitIdentity(hit)] ||
				hit.TokenEstimate > maxExpandedChunkTokens {
				continue
			}
			out = append(out, hit)
			seen[hitIdentity(hit)] = true
			addedHeaders++
		}
	}
	appendHeaders(true)
	appendHeaders(false)

	addedMembers := 0
	for _, hit := range ranked {
		if addedMembers >= maxExpandedClassMembers {
			break
		}
		if !selectedPaths[hit.Path] || hit.ParentID == "" ||
			!matchingParents[parentIdentity(hit.Path, hit.ParentID)] ||
			!symbolExactlyNamed(hit.Symbol, terms) ||
			seen[hitIdentity(hit)] ||
			hit.TokenEstimate > maxExpandedChunkTokens {
			continue
		}
		out = append(out, hit)
		seen[hitIdentity(hit)] = true
		addedMembers++
	}

	// Compound implementation names encode an action and its subject. When a
	// selected file has spare result/bundle budget, append these companions
	// instead of changing primary scores and displacing existing evidence.
	// This recovers APIs hidden inside large factories (mountComponent,
	// patchChildren) and explicit factory entry points (createRenderer).
	addedCompanions := 0
	for _, hit := range ranked {
		if addedCompanions >= maxExpandedCompanions {
			break
		}
		if !selectedPaths[hit.Path] ||
			!isImplementationKind(hit.Kind) ||
			seen[hitIdentity(hit)] ||
			hit.TokenEstimate > maxExpandedChunkTokens ||
			(symbolQueryCoverage(hit.Symbol, terms) < 2 &&
				!factorySymbolMatchesQuery(hit.Symbol, terms)) {
			continue
		}
		out = append(out, hit)
		seen[hitIdentity(hit)] = true
		addedCompanions++
	}

	// When the query names a compound filename concern ("session pool" →
	// sessionpool.go), carry one matching helper and one exported entry point
	// from that already-selected file. This is deliberately narrower than a
	// generic same-file expansion: compound stems are specific, and the two
	// extra slots recover the API + implementation bridge without disturbing
	// the primary ranking or flat names such as ledger.go.
	addedCompoundHelpers := 0
	appendCompoundHelper := func(exportedOnly bool) {
		if addedCompoundHelpers >= maxExpandedCompoundFileHelpers {
			return
		}
		for _, hit := range ranked {
			if !selectedPaths[hit.Path] ||
				!fileStemCompoundMatchesQuery(hit.Path, terms) ||
				!isImplementationKind(hit.Kind) ||
				seen[hitIdentity(hit)] ||
				hit.TokenEstimate > maxExpandedChunkTokens {
				continue
			}
			if exportedOnly {
				if !isExportedImplementation(hit) {
					continue
				}
			} else if !symbolSharesQueryComponent(hit.Symbol, terms) {
				continue
			}
			out = append(out, hit)
			seen[hitIdentity(hit)] = true
			addedCompoundHelpers++
			return
		}
	}
	appendCompoundHelper(false)
	appendCompoundHelper(true)

	// A query can name a file's concern without naming the declaration that
	// contains the answer ("what tools does the server expose?" →
	// tools.go:toolsList, or "cross shape ..." →
	// phase_g5_cross_shape.md). If primary ranking selected no chunk from
	// that file, use one final additive slot for a filename anchor. Prefer a
	// compact implementation whose symbol begins with the filename term;
	// otherwise the highest-ranked chunk from the file is still enough to
	// expose its path and local context.
	if maxExpandedFileAnchors > 0 {
		var fallback *Hit
		for i := range ranked {
			hit := &ranked[i]
			if selectedPaths[hit.Path] ||
				seen[hitIdentity(*hit)] ||
				hit.TokenEstimate > maxExpandedChunkTokens ||
				!fileStemMatchesQuery(hit.Path, terms) {
				continue
			}
			// A multi-part filename phrase is specific enough to use its
			// highest-ranked chunk as a fallback. A flat generic filename
			// such as ledger.go is not: require a matching implementation
			// name there, otherwise it can add a cheap but irrelevant type
			// and distort both precision and iso-recall economics.
			if fallback == nil && fileStemCompoundMatchesQuery(hit.Path, terms) {
				fallback = hit
			}
			if isImplementationKind(hit.Kind) && symbolBeginsWithFileStem(hit.Symbol, hit.Path, terms) {
				fallback = hit
				break
			}
		}
		if fallback != nil {
			out = append(out, *fallback)
		}
	}
	return out
}

func symbolSharesQueryComponent(symbol string, terms []string) bool {
	raw := strings.ToLower(strings.TrimSpace(symbol))
	for _, symbolPart := range ranking.Tokenise(symbol) {
		if symbolPart == raw || len(symbolPart) < 4 {
			continue
		}
		for _, term := range terms {
			if morphologicallyEqual(symbolPart, term) {
				return true
			}
		}
	}
	return false
}

func isExportedImplementation(hit Hit) bool {
	if hit.Kind == "export_func" {
		return true
	}
	symbol := hit.Symbol
	if dot := strings.LastIndex(symbol, "."); dot >= 0 {
		symbol = symbol[dot+1:]
	}
	if symbol == "" {
		return false
	}
	first := rune(symbol[0])
	return first >= 'A' && first <= 'Z'
}

func isClassHeaderKind(kind string) bool {
	return kind == "class" || kind == "export_class"
}

func classSymbolMatchesQuery(symbol string, terms []string) bool {
	if symbolExactlyNamed(symbol, terms) {
		return true
	}
	raw := strings.ToLower(strings.TrimSpace(symbol))
	for _, part := range ranking.Tokenise(symbol) {
		if part == raw {
			continue
		}
		for _, term := range terms {
			if morphologicallyEqual(part, term) {
				return true
			}
		}
	}
	return false
}

func hitIdentity(hit Hit) string {
	return fmt.Sprintf("%s|%s|%d", hit.Path, hit.ContentHash, hit.StartLine)
}

func parentIdentity(path, chunkID string) string {
	return path + "\x00" + chunkID
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

func scoreChunkHit(rec models.FileRecord, chunk models.Chunk, snippet string, terms []string, w *Weights) (float64, []string) {
	var score float64
	var reasons []string
	add := func(reason string, weight float64) {
		score += weight
		if !containsString(reasons, reason) {
			reasons = append(reasons, reason)
		}
	}

	if textMatchesTerms(chunk.Symbol, terms) {
		add("symbol_match", w.SymbolMatch)
	}
	// A query term that *equals* the symbol name (or its last dotted
	// component) is much stronger evidence than the substring matching
	// above — the question literally names this identifier. This is the
	// discriminator inside one file, where the file-level structural boosts
	// are identical for every chunk and substring symbol matches tie.
	if symbolExactlyNamed(chunk.Symbol, terms) {
		add("symbol_exact", w.SymbolExact)
	}
	// Prefer an unqualified declaration when the query names it directly.
	// A query for "trigger" can match both Dep.trigger and the exported
	// trigger function; the latter is the exact symbol the user named.
	if symbolFullyExactlyNamed(chunk.Symbol, terms) {
		add("symbol_full_exact", w.SymbolExact*0.5)
	}
	baseStem := stripExt(filepath.Base(rec.RelPath))
	if textMatchesTerms(rec.RelPath, terms) || textMatchesTerms(baseStem, terms) {
		add("path_match", w.PathMatch)
	}
	if textMatchesTerms(chunk.Kind, terms) {
		add("kind_match", w.KindMatch)
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
		add("content_match", float64(contentHits)*w.ContentMatch)
	}
	if chunk.Kind != "file" && score > 0 {
		add("chunk_scope", w.ChunkScope)
	}
	if w.ImplKind > 0 && score > 0 && isImplementationKind(chunk.Kind) {
		add("impl_kind", w.ImplKind)
	}
	return score, reasons
}

// isImplementationKind marks chunks with a function body — where facts
// live — as opposed to declarations (types, aliases, re-export stubs).
// Classes are deliberately neither: click's fixtures need class-header
// chunks to compete on equal footing.
func isImplementationKind(kind string) bool {
	switch kind {
	case "func", "export_func", "method", "nested_func", "get", "set":
		return true
	}
	return false
}

func semanticBoost(similarity float64, w *Weights) float64 {
	if similarity > 1 {
		similarity = 1
	}
	boost := 1.0 + ((similarity - semanticSimilarityThreshold) * w.Semantic)
	if boost < 1.0 {
		return 1.0
	}
	if boost > w.Semantic {
		return w.Semantic
	}
	return boost
}

func applyExactBoost(candidates []candidate, signals map[string]exactSignal, w *Weights) {
	if len(candidates) == 0 || len(signals) == 0 {
		return
	}
	for i := range candidates {
		signal, ok := signals[candidates[i].hit.Path]
		if !ok {
			continue
		}
		if signal.filename {
			addReason(&candidates[i].hit, "exact_filename", w.ExactFilename)
		}
		if linesOverlap(signal.lines, candidates[i].hit.StartLine, candidates[i].hit.EndLine) {
			addReason(&candidates[i].hit, "exact_content", w.ExactContent)
		}
	}
}

// exactSearchSignals derives content signals from the same checksum-validated
// bytes used to build result snippets. Reading the live working tree here
// would let a post-session edit boost an old chunk at the edited line number,
// mixing two generations inside one result.
func exactSearchSignals(
	terms []string,
	files []models.FileRecord,
	contentByPath map[string]string,
) map[string]exactSignal {
	signals := exactFilenameSignals(terms, files)
	patterns := exactSearchTerms(terms)
	if len(patterns) == 0 {
		return signals
	}
	for _, file := range files {
		content, ok := contentByPath[file.Path]
		if !ok {
			continue
		}
		for lineIndex, line := range splitLogicalLines(content) {
			if !lineContainsExactSearchTerm(line, patterns) {
				continue
			}
			signal := signals[file.RelPath]
			if signal.lines == nil {
				signal.lines = make(map[int]bool)
			}
			signal.lines[lineIndex+1] = true
			signals[file.RelPath] = signal
		}
	}
	return signals
}

func lineContainsExactSearchTerm(line string, terms []string) bool {
	line = strings.ToLower(line)
	for _, term := range terms {
		offset := 0
		for offset <= len(line)-len(term) {
			match := strings.Index(line[offset:], term)
			if match < 0 {
				break
			}
			start := offset + match
			end := start + len(term)
			leftBoundary := start == 0 || !isExactWordByte(line[start-1])
			rightBoundary := end == len(line) || !isExactWordByte(line[end])
			if leftBoundary && rightBoundary {
				return true
			}
			offset = start + 1
		}
	}
	return false
}

func isExactWordByte(value byte) bool {
	return value >= 'a' && value <= 'z' ||
		value >= '0' && value <= '9' ||
		value == '_'
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
	candidates = append(candidates, splitIdentifierForSearch(stem)...)
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

func applyWorkingSetBoost(candidates []candidate, changedPaths map[string]bool, w *Weights) {
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
		addReason(&candidates[i].hit, "working_set", w.WorkingSet)
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

func applyGraphBoost(candidates []candidate, relations []models.FileRelation, w *Weights) {
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
		addReason(&candidates[i].hit, reason, w.Graph)
	}
}

func applyLongChunkPenalty(candidates []candidate, w *Weights) {
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
		if penalty > w.LongChunkPenaltyMax {
			penalty = w.LongChunkPenaltyMax
		}
		addPenalty(&candidates[i].hit, "long_chunk_penalty", penalty)
	}
}

// applyLegacyPathPenalty downranks chunks under compat/legacy directories
// unless the query names that surface. Mirrors the test-path penalty and
// is neutral until the tuner moves LegacyPathKeep below 1.0.
func applyLegacyPathPenalty(candidates []candidate, query string, w *Weights) {
	if w.LegacyPathKeep >= 1.0 {
		return
	}
	wantsLegacy := ranking.QueryWantsLegacy(query)
	for i := range candidates {
		if !ranking.IsLegacyLikePath(candidates[i].hit.Path) {
			continue
		}
		if wantsLegacy {
			addReason(&candidates[i].hit, "query_legacy_intent_detected", 0)
			continue
		}
		if candidates[i].hit.Score <= 0 {
			continue
		}
		addPenalty(&candidates[i].hit, "legacy_path_downrank", candidates[i].hit.Score*(1-w.LegacyPathKeep))
	}
}

const tinyChunkTokenThreshold = 40

// applyTinyChunkPenalty downranks near-empty declaration chunks — one-line
// re-export stubs, short type aliases — whose names collide with ordinary
// query words. Measured on vuejs/core: `export const Vue` stubs (~14
// tokens) and the 4-line `Renderer` type alias outranked the actual
// renderer implementation on symbol_exact alone, crowding the per-file
// diversity cap. Multiplicative like the test downrank, so a stub that is
// genuinely the only match still surfaces.
func applyTinyChunkPenalty(candidates []candidate, w *Weights) {
	for i := range candidates {
		hit := &candidates[i].hit
		if hit.Score <= 0 || hit.Kind == "file" {
			continue
		}
		if hit.TokenEstimate <= 0 || hit.TokenEstimate >= tinyChunkTokenThreshold {
			continue
		}
		addPenalty(hit, "tiny_chunk_downrank", hit.Score*(1-w.TinyChunkKeep))
	}
}

func applyTestPenalty(candidates []candidate, query string, w *Weights) {
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
		addPenalty(&candidates[i].hit, "test_like_downrank", candidates[i].hit.Score*(1-w.TestDownrank))
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
		if morphologicallyEqual(t, sym) || morphologicallyEqual(t, last) {
			return true
		}
	}
	return false
}

func symbolFullyExactlyNamed(symbol string, terms []string) bool {
	sym := strings.ToLower(strings.TrimSpace(symbol))
	if sym == "" || strings.Contains(sym, ".") {
		return false
	}
	for _, term := range terms {
		if morphologicallyEqual(term, sym) {
			return true
		}
	}
	return false
}

func symbolQueryCoverage(symbol string, terms []string) int {
	symbol = strings.TrimSpace(symbol)
	if symbol == "" {
		return 0
	}
	if dot := strings.LastIndex(symbol, "."); dot >= 0 {
		symbol = symbol[dot+1:]
	}
	raw := strings.ToLower(symbol)
	var parts []string
	for _, part := range ranking.Tokenise(symbol) {
		if part == raw {
			continue
		}
		parts = append(parts, part)
	}
	if len(parts) < 2 || len(terms) < 2 {
		return 0
	}

	// Require the same contiguous phrase in both the identifier and query.
	// Unordered component coverage made test_other_command_invoke look like
	// the best answer to "invoke ... commands", and partial coverage made
	// mountChildren compete with "mount components ... patch children".
	best := 0
	for partStart := range parts {
		for termStart := range terms {
			n := 0
			for partStart+n < len(parts) && termStart+n < len(terms) &&
				morphologicallyEqual(parts[partStart+n], terms[termStart+n]) {
				n++
			}
			if n > best {
				best = n
			}
		}
	}
	if best < 2 {
		return 0
	}
	return best
}

func factorySymbolMatchesQuery(symbol string, terms []string) bool {
	symbol = strings.TrimSpace(symbol)
	if dot := strings.LastIndex(symbol, "."); dot >= 0 {
		symbol = symbol[dot+1:]
	}
	raw := strings.ToLower(symbol)
	var parts []string
	for _, part := range ranking.Tokenise(symbol) {
		if part != raw {
			parts = append(parts, part)
		}
	}
	if len(parts) != 2 {
		return false
	}
	switch parts[0] {
	case "create", "make", "build", "open", "new":
	default:
		return false
	}
	for _, term := range terms {
		if morphologicallyEqual(parts[1], term) {
			return true
		}
	}
	return false
}

func fileStemMatchesQuery(path string, terms []string) bool {
	stem := stripExt(filepath.Base(path))
	for _, term := range terms {
		if morphologicallyEqual(stem, term) {
			return true
		}
	}
	return fileStemCompoundMatchesQuery(path, terms)
}

func fileStemCompoundMatchesQuery(path string, terms []string) bool {
	stem := strings.ToLower(stripExt(filepath.Base(path)))
	if symbolQueryCoverage(stem, terms) >= 2 {
		return true
	}
	// Lowercase compound filenames such as sessionpool.go carry no camel or
	// separator boundary for Tokenise to split. Match only an exact contiguous
	// concatenation of two or more query terms.
	for start := range terms {
		var joined strings.Builder
		for end := start; end < len(terms); end++ {
			joined.WriteString(strings.ToLower(terms[end]))
			if end > start && joined.String() == stem {
				return true
			}
			if joined.Len() >= len(stem) {
				break
			}
		}
	}
	return false
}

func symbolBeginsWithFileStem(symbol, path string, terms []string) bool {
	if dot := strings.LastIndex(symbol, "."); dot >= 0 {
		symbol = symbol[dot+1:]
	}
	rawSymbol := strings.ToLower(strings.TrimSpace(symbol))
	var symbolParts []string
	for _, part := range ranking.Tokenise(symbol) {
		if part != rawSymbol {
			symbolParts = append(symbolParts, part)
		}
	}
	if len(symbolParts) == 0 {
		symbolParts = []string{rawSymbol}
	}

	stem := stripExt(filepath.Base(path))
	rawStem := strings.ToLower(stem)
	stemParts := []string{rawStem}
	for _, part := range ranking.Tokenise(stem) {
		if part != rawStem {
			stemParts = append(stemParts, part)
		}
	}
	for _, stemPart := range stemParts {
		matchesQuery := false
		for _, term := range terms {
			if morphologicallyEqual(stemPart, term) {
				matchesQuery = true
				break
			}
		}
		if matchesQuery && morphologicallyEqual(symbolParts[0], stemPart) {
			return true
		}
	}
	return false
}

func morphologicallyEqual(a, b string) bool {
	a = strings.ToLower(strings.TrimSpace(a))
	b = strings.ToLower(strings.TrimSpace(b))
	if a == "" || b == "" {
		return false
	}
	for _, av := range ranking.TermVariants(a) {
		if len(av) < 3 {
			continue
		}
		for _, bv := range ranking.TermVariants(b) {
			if len(bv) >= 3 && av == bv {
				return true
			}
		}
	}
	return false
}

func symbolScore(symbol string, terms []string, w *Weights) float64 {
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
					return w.StructuralSymbol
				}
			}
		}
	}
	return w.StructuralSymbolPartial
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
		return !isSearchIdentifierRune(r)
	}) {
		part = strings.ToLower(part)
		if len(part) >= 3 {
			parts = append(parts, part)
		}
	}
	return parts
}

func isSearchIdentifierRune(r rune) bool {
	return ('a' <= r && r <= 'z') ||
		('A' <= r && r <= 'Z') ||
		('0' <= r && r <= '9')
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
