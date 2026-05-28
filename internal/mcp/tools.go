package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/neuromfs/neuromfs/internal/config"
	"github.com/neuromfs/neuromfs/internal/fsutil"
	"github.com/neuromfs/neuromfs/internal/indexer"
	"github.com/neuromfs/neuromfs/internal/models"
	"github.com/neuromfs/neuromfs/internal/packager"
	"github.com/neuromfs/neuromfs/internal/ranking"
	"github.com/neuromfs/neuromfs/internal/retrieval"
	"github.com/neuromfs/neuromfs/internal/storage"
	"github.com/neuromfs/neuromfs/internal/taskflow"
)

const taskInputSchema = `{
  "type": "object",
  "properties": {
    "query":          { "type": "string",  "description": "What you're trying to do (a sentence)." },
    "repo":           { "type": "string",  "description": "Absolute path to repo. Default: cwd." },
    "budget":         { "type": "integer", "description": "Token budget. Default: 3000." },
    "disable_chunks": { "type": "boolean", "description": "Disable chunk-based packing and build the prompt from ranked whole files instead." }
  },
  "required": ["query"]
}`

const scanInputSchema = `{
  "type": "object",
  "properties": {
    "repo": { "type": "string", "description": "Absolute path. Default: cwd." }
  }
}`

const viewFileInputSchema = `{
  "type": "object",
  "properties": {
    "path": { "type": "string", "description": "Relative path to the file from the repository root." },
    "repo": { "type": "string", "description": "Absolute path to the repository root. Default: cwd." }
  },
  "required": ["path"]
}`

const outlineInputSchema = `{
  "type": "object",
  "properties": {
    "repo": { "type": "string", "description": "Absolute path to repo. Default: cwd." }
  }
}`

const listSignaturesInputSchema = `{
  "type": "object",
  "properties": {
    "path": { "type": "string", "description": "Relative path to the file from the repository root." },
    "repo": { "type": "string", "description": "Absolute path to repo. Default: cwd." }
  },
  "required": ["path"]
}`

const getExcerptInputSchema = `{
  "type": "object",
  "properties": {
    "path":  { "type": "string", "description": "Relative path to the file from the repository root." },
    "query": { "type": "string", "description": "Search query terms to match symbols against (space-separated)." },
    "repo":  { "type": "string", "description": "Absolute path to repo. Default: cwd." }
  },
  "required": ["path", "query"]
}`

const searchInputSchema = `{
  "type": "object",
  "properties": {
    "query": { "type": "string", "description": "Search query for relevant code chunks." },
    "repo":  { "type": "string", "description": "Absolute path to repo. Default: cwd." },
    "limit": { "type": "integer", "description": "Maximum number of chunk hits to return. Default: 8." },
    "mode":  { "type": "string", "description": "Retrieval mode hint: research, build, review, or test." }
  },
  "required": ["query"]
}`

const contextInputSchema = `{
  "type": "object",
  "properties": {
    "query":  { "type": "string", "description": "Codebase question or task. Optional only when intent is outline." },
    "repo":   { "type": "string", "description": "Absolute path to repo. Default: cwd." },
    "intent": { "type": "string", "description": "Routing hint: outline, search, excerpt, bundle, build, research, review, test, or unknown." },
    "budget": { "type": "integer", "description": "Token budget when the route needs a prompt bundle. Default: 3000." },
    "limit":  { "type": "integer", "description": "Maximum chunk hits when the route uses search. Default: 8." }
  }
}`

func toolsList() []Tool {
	return []Tool{
		{
			Name:        "neurofs_context",
			Description: "Route a codebase question to the smallest sufficient NeuroFS operation and return traceable context.",
			InputSchema: json.RawMessage(contextInputSchema),
		},
		{
			Name:        "neurofs_task",
			Description: "Pack a Claude-ready prompt for a given intention against a repo. Returns the prompt text.",
			InputSchema: json.RawMessage(taskInputSchema),
		},
		{
			Name:        "neurofs_scan",
			Description: "Index a repo and return a read-only summary (file count, total size, top extensions).",
			InputSchema: json.RawMessage(scanInputSchema),
		},
		{
			Name:        "neurofs_view_file",
			Description: "Read the full contents of a specific file inside the repository safely.",
			InputSchema: json.RawMessage(viewFileInputSchema),
		},
		{
			Name:        "neurofs_get_outline",
			Description: "List all indexed files and their sizes to outline the codebase structure.",
			InputSchema: json.RawMessage(outlineInputSchema),
		},
		{
			Name:        "neurofs_list_signatures",
			Description: "Get the signatures (functions, types, methods) of a specific file.",
			InputSchema: json.RawMessage(listSignaturesInputSchema),
		},
		{
			Name:        "neurofs_get_excerpt",
			Description: "Extract only the code block segments matching the search query terms from a file.",
			InputSchema: json.RawMessage(getExcerptInputSchema),
		},
		{
			Name:        "neurofs_search",
			Description: "Return ranked code chunks for a query using lexical, exact rg, semantic, graph, and git working-set signals.",
			InputSchema: json.RawMessage(searchInputSchema),
		},
	}
}

func callTool(ctx context.Context, p ToolCallParams) ToolCallResult {
	switch p.Name {
	case "neurofs_context":
		return runContextTool(ctx, p.Arguments)
	case "neurofs_task":
		return runTaskTool(ctx, p.Arguments)
	case "neurofs_scan":
		return runScanTool(ctx, p.Arguments)
	case "neurofs_view_file":
		return runViewFileTool(ctx, p.Arguments)
	case "neurofs_get_outline":
		return runGetOutlineTool(ctx, p.Arguments)
	case "neurofs_list_signatures":
		return runListSignaturesTool(ctx, p.Arguments)
	case "neurofs_get_excerpt":
		return runGetExcerptTool(ctx, p.Arguments)
	case "neurofs_search":
		return runSearchTool(ctx, p.Arguments)
	default:
		return errResult(fmt.Sprintf("unknown tool: %q", p.Name))
	}
}

type taskArgs struct {
	Query         string `json:"query"`
	Repo          string `json:"repo"`
	Budget        int    `json:"budget"`
	DisableChunks bool   `json:"disable_chunks"`
}

// ContextOptions configures the high-level broker used by neurofs_context.
type ContextOptions struct {
	Query  string `json:"query"`
	Repo   string `json:"repo"`
	Intent string `json:"intent"`
	Budget int    `json:"budget"`
	Limit  int    `json:"limit"`
}

// ContextTraceStep records one routed operation inside the broker response.
type ContextTraceStep struct {
	Tool   string `json:"tool"`
	Reason string `json:"reason"`
}

// ContextStructuralHint is a pre-routing match from indexed symbols/imports.
type ContextStructuralHint struct {
	Path          string          `json:"path"`
	Score         float64         `json:"score"`
	SymbolMatches []models.Symbol `json:"symbol_matches,omitempty"`
	ImportMatches []string        `json:"import_matches,omitempty"`
	Reasons       []string        `json:"reasons"`
}

// ContextResponse is the JSON payload returned by neurofs_context.
type ContextResponse struct {
	Query           string                  `json:"query,omitempty"`
	Intent          string                  `json:"intent"`
	Route           string                  `json:"route"`
	ToolTrace       []ContextTraceStep      `json:"tool_trace"`
	StructuralHints []ContextStructuralHint `json:"structural_hints,omitempty"`
	Results         []SearchResultHit       `json:"results,omitempty"`
	Text            string                  `json:"text,omitempty"`
	Prompt          string                  `json:"prompt,omitempty"`
	PromptPath      string                  `json:"prompt_path,omitempty"`
	BundlePath      string                  `json:"bundle_path,omitempty"`
	Stats           *models.BundleStats     `json:"stats,omitempty"`
}

func runContextTool(ctx context.Context, raw json.RawMessage) ToolCallResult {
	var args ContextOptions
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &args); err != nil {
			return errResult(fmt.Sprintf("invalid arguments: %v", err))
		}
	}
	response, err := Context(ctx, args)
	if err != nil {
		return errResult(err.Error())
	}
	return jsonTextResult(response)
}

// Context routes a codebase question to the smallest sufficient NeuroFS
// operation and returns a traceable, JSON-serializable response.
func Context(ctx context.Context, args ContextOptions) (ContextResponse, error) {
	args.Query = strings.TrimSpace(args.Query)
	rawIntent := strings.ToLower(strings.TrimSpace(args.Intent))
	if args.Limit <= 0 {
		args.Limit = 8
	}
	repo, err := resolveRepo(ctx, args.Repo)
	if err != nil {
		return ContextResponse{}, err
	}

	structuralHints, err := contextStructuralHints(repo, args.Query, args.Limit)
	if err != nil {
		return ContextResponse{}, err
	}
	args.Intent = normalizeContextIntent(rawIntent, args.Query)
	if shouldRouteContextToExcerpt(rawIntent, args.Intent, structuralHints) {
		args.Intent = "excerpt"
	}
	if args.Budget <= 0 {
		args.Budget = defaultContextBudget(args.Intent)
	}

	response := ContextResponse{
		Query:           args.Query,
		Intent:          args.Intent,
		StructuralHints: structuralHints,
	}
	if len(structuralHints) > 0 {
		response.ToolTrace = append(response.ToolTrace, ContextTraceStep{
			Tool:   "sqlite_symbols_imports",
			Reason: "matched query terms against indexed symbols and imports before routing",
		})
	}
	switch args.Intent {
	case "outline":
		response.Route = "outline"
		response.ToolTrace = append(response.ToolTrace, ContextTraceStep{
			Tool:   "neurofs_get_outline",
			Reason: "broad repository orientation requested",
		})
		outlineRaw, _ := json.Marshal(map[string]any{"repo": repo})
		res := runGetOutlineTool(ctx, outlineRaw)
		if res.IsError {
			return ContextResponse{}, fmt.Errorf("%s", firstText(res))
		}
		response.Text = firstText(res)
	case "excerpt":
		if args.Query == "" {
			return ContextResponse{}, fmt.Errorf("query must not be empty for excerpt intent")
		}
		searchLimit := args.Limit
		if searchLimit < 5 {
			searchLimit = 5
		}
		searchResponse, err := Search(ctx, SearchOptions{
			Query: args.Query,
			Repo:  repo,
			Limit: searchLimit,
			Mode:  "excerpt",
		})
		if err != nil {
			return ContextResponse{}, err
		}
		if len(searchResponse.Results) == 0 {
			response.Route = "search"
			response.Results = searchResponse.Results
			response.ToolTrace = append(response.ToolTrace,
				ContextTraceStep{Tool: "neurofs_search", Reason: "no matching chunk found for excerpt"},
			)
			return response, nil
		}
		excerptRaw, _ := json.Marshal(map[string]any{
			"path":  searchResponse.Results[0].Path,
			"query": args.Query,
			"repo":  repo,
		})
		res := runGetExcerptTool(ctx, excerptRaw)
		if !res.IsError {
			response.Route = "excerpt"
			response.Results = searchResponse.Results[:1]
			response.ToolTrace = append(response.ToolTrace,
				ContextTraceStep{Tool: "neurofs_search", Reason: "find the best line-ranged chunk before expanding it"},
				ContextTraceStep{Tool: "neurofs_get_excerpt", Reason: "expand only the selected file span"},
			)
			response.Text = firstText(res)
			return response, nil
		}
		// Fallback to search results if excerpt extraction fails (e.g. for Markdown or unsupported files)
		response.Route = "search"
		response.Results = searchResponse.Results
		response.ToolTrace = append(response.ToolTrace,
			ContextTraceStep{Tool: "neurofs_search", Reason: "fallback to ranked search results because excerpt extraction failed"},
		)
	case "bundle", "build":
		if args.Query == "" {
			return ContextResponse{}, fmt.Errorf("query must not be empty for bundle intent")
		}
		result, err := taskflow.Run(taskflow.Opts{
			RepoRoot:      repo,
			Query:         args.Query,
			Budget:        args.Budget,
			DisableChunks: false,
		})
		if err != nil {
			return ContextResponse{}, err
		}
		response.Route = "task_chunks"
		response.ToolTrace = append(response.ToolTrace, ContextTraceStep{
			Tool:   "neurofs_task",
			Reason: "caller requested an implementation-ready prompt bundle",
		})
		response.Prompt = result.Prompt
		response.PromptPath = result.PromptPath
		response.BundlePath = result.BundlePath
		response.Stats = &result.Stats
	case "search", "research", "review", "test":
		if args.Query == "" {
			return ContextResponse{}, fmt.Errorf("query must not be empty for %s intent", args.Intent)
		}
		searchLimit := profileSearchLimit(args.Intent, args.Limit)
		searchResponse, err := Search(ctx, SearchOptions{
			Query: args.Query,
			Repo:  repo,
			Limit: searchLimit,
			Mode:  args.Intent,
		})
		if err != nil {
			return ContextResponse{}, err
		}
		response.Route = "search"
		response.ToolTrace = append(response.ToolTrace, ContextTraceStep{
			Tool:   "neurofs_search",
			Reason: contextSearchReason(args.Intent, searchLimit),
		})
		response.Results = searchResponse.Results
	default:
		return ContextResponse{}, fmt.Errorf("unsupported intent: %s", args.Intent)
	}
	return response, nil
}

func contextStructuralHints(repo, query string, limit int) ([]ContextStructuralHint, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, nil
	}
	terms := ranking.Tokenise(query)
	if len(terms) == 0 {
		return nil, nil
	}
	cfg, err := config.New(repo)
	if err != nil {
		return nil, err
	}
	db, err := storage.Open(cfg.DBPath)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	files, err := db.AllFiles()
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		if _, err := indexer.Run(cfg, db, indexer.Options{Logf: func(string, ...any) {}}); err != nil {
			return nil, err
		}
		files, err = db.AllFiles()
		if err != nil {
			return nil, err
		}
	}

	hints := make([]ContextStructuralHint, 0)
	for _, file := range files {
		hint := ContextStructuralHint{Path: file.RelPath}
		for _, sym := range file.Symbols {
			if !contextMatchesTerms(sym.Name, terms) {
				continue
			}
			hint.SymbolMatches = append(hint.SymbolMatches, sym)
			hint.Score += contextSymbolScore(sym.Name, terms)
			addContextReason(&hint, "symbol_match")
		}
		for _, imp := range file.Imports {
			if !contextMatchesTerms(imp, terms) {
				continue
			}
			hint.ImportMatches = append(hint.ImportMatches, imp)
			hint.Score += 2.0
			addContextReason(&hint, "import_match")
		}
		if hint.Score > 0 {
			hints = append(hints, hint)
		}
	}

	sort.SliceStable(hints, func(i, j int) bool {
		if hints[i].Score != hints[j].Score {
			return hints[i].Score > hints[j].Score
		}
		return hints[i].Path < hints[j].Path
	})
	if limit <= 0 {
		limit = 8
	}
	if limit > 20 {
		limit = 20
	}
	if len(hints) > limit {
		hints = hints[:limit]
	}
	return hints, nil
}

func contextMatchesTerms(text string, terms []string) bool {
	text = strings.ToLower(strings.TrimSpace(text))
	if text == "" {
		return false
	}
	compact := compactContextText(text)
	for _, term := range terms {
		term = strings.ToLower(strings.TrimSpace(term))
		if term == "" {
			continue
		}
		if strings.Contains(text, term) || strings.Contains(compact, term) {
			return true
		}
	}
	return false
}

func compactContextText(text string) string {
	var b strings.Builder
	for _, r := range text {
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteRune(r)
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		}
	}
	return b.String()
}

func contextSymbolScore(symbol string, terms []string) float64 {
	lower := strings.ToLower(symbol)
	compact := compactContextText(lower)
	for _, term := range terms {
		term = strings.ToLower(strings.TrimSpace(term))
		if term != "" && (lower == term || compact == term) {
			return 18.0
		}
	}
	return 3.0
}

func addContextReason(hint *ContextStructuralHint, reason string) {
	if !containsString(hint.Reasons, reason) {
		hint.Reasons = append(hint.Reasons, reason)
	}
}

func shouldRouteContextToExcerpt(rawIntent, currentIntent string, hints []ContextStructuralHint) bool {
	if rawIntent != "" && rawIntent != "unknown" && rawIntent != "auto" {
		return false
	}
	if currentIntent != "search" || len(hints) == 0 {
		return false
	}
	return len(hints[0].SymbolMatches) > 0 && hints[0].Score >= 6.0
}


func normalizeContextIntent(intent, query string) string {
	intent = strings.ToLower(strings.TrimSpace(intent))
	switch intent {
	case "", "unknown", "auto":
		return inferContextIntent(query)
	case "task", "prompt":
		return "bundle"
	case "implement", "implementation", "fix", "refactor":
		return "build"
	case "tests", "test-fix":
		return "test"
	default:
		return intent
	}
}

func inferContextIntent(query string) string {
	q := strings.ToLower(strings.TrimSpace(query))
	if q == "" {
		return "outline"
	}
	words := contextQueryWords(q)
	outlineHints := []string{"outline", "overview", "map", "structure", "files", "estructura", "resumen", "mapa"}
	for _, hint := range outlineHints {
		if words[hint] {
			return "outline"
		}
	}
	bundleHints := []string{"implement", "build", "fix", "refactor", "review", "add", "arregla", "implementa", "revisa"}
	for _, hint := range bundleHints {
		if words[hint] {
			if hint == "review" || hint == "revisa" {
				return "review"
			}
			return "build"
		}
	}
	return "search"
}

func defaultContextBudget(intent string) int {
	switch intent {
	case "build", "bundle":
		return 4000
	case "review":
		return 2500
	case "test":
		return 2200
	default:
		return 3000
	}
}

func profileSearchLimit(intent string, limit int) int {
	minLimit := 0
	switch intent {
	case "research":
		minLimit = 12
	case "review":
		minLimit = 10
	case "test":
		minLimit = 8
	}
	if limit < minLimit {
		return minLimit
	}
	return limit
}

func contextSearchReason(intent string, limit int) string {
	switch intent {
	case "research":
		return fmt.Sprintf("research profile asks for a broader ranked span set (limit %d), boosted by indexed symbols/imports", limit)
	case "review":
		return fmt.Sprintf("review profile keeps working-set and graph-neighbor spans visible (limit %d), boosted by indexed symbols/imports", limit)
	case "test":
		return fmt.Sprintf("test profile retrieves implementation and nearby test-fix spans (limit %d), boosted by indexed symbols/imports", limit)
	default:
		return "smallest sufficient context is ranked code spans, boosted by indexed symbols/imports"
	}
}

func contextQueryWords(query string) map[string]bool {
	words := make(map[string]bool)
	for _, word := range strings.FieldsFunc(strings.ToLower(query), func(r rune) bool {
		return !((r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_')
	}) {
		word = strings.TrimSpace(word)
		if word != "" {
			words[word] = true
		}
	}
	return words
}

func jsonTextResult(v any) ToolCallResult {
	payload, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return errResult(fmt.Sprintf("marshal context response: %v", err))
	}
	return textResult(string(payload))
}

func firstText(res ToolCallResult) string {
	if len(res.Content) == 0 {
		return ""
	}
	return res.Content[0].Text
}

func runTaskTool(ctx context.Context, raw json.RawMessage) ToolCallResult {
	var args taskArgs
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &args); err != nil {
			return errResult(fmt.Sprintf("invalid arguments: %v", err))
		}
	}
	args.Query = strings.TrimSpace(args.Query)
	if args.Query == "" {
		return errResult("query must not be empty")
	}
	if args.Budget <= 0 {
		args.Budget = 3000
	}
	repo, err := resolveRepo(ctx, args.Repo)
	if err != nil {
		return errResult(err.Error())
	}

	result, err := taskflow.Run(taskflow.Opts{
		RepoRoot:      repo,
		Query:         args.Query,
		Budget:        args.Budget,
		DisableChunks: args.DisableChunks,
	})
	if err != nil {
		return errResult(err.Error())
	}
	return textResult(result.Prompt)
}

type scanArgs struct {
	Repo string `json:"repo"`
}

func runScanTool(ctx context.Context, raw json.RawMessage) ToolCallResult {
	var args scanArgs
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &args); err != nil {
			return errResult(fmt.Sprintf("invalid arguments: %v", err))
		}
	}
	repo, err := resolveRepo(ctx, args.Repo)
	if err != nil {
		return errResult(err.Error())
	}

	cfg, err := config.New(repo)
	if err != nil {
		return errResult(err.Error())
	}
	if err := cfg.Validate(); err != nil {
		return errResult(err.Error())
	}

	db, err := storage.Open(cfg.DBPath)
	if err != nil {
		return errResult(err.Error())
	}
	defer db.Close()

	stats, err := indexer.Run(cfg, db, indexer.Options{Logf: func(string, ...any) {}})
	if err != nil {
		return errResult(err.Error())
	}

	files, err := db.AllFiles()
	if err != nil {
		return errResult(err.Error())
	}

	var totalSize int64
	extCount := make(map[string]int)
	extBytes := make(map[string]int64)
	for _, f := range files {
		totalSize += f.Size
		ext := strings.ToLower(filepath.Ext(f.RelPath))
		if ext == "" {
			ext = "(none)"
		}
		extCount[ext]++
		extBytes[ext] += f.Size
	}

	type extRow struct {
		ext   string
		count int
		bytes int64
	}
	rows := make([]extRow, 0, len(extCount))
	for k, v := range extCount {
		rows = append(rows, extRow{ext: k, count: v, bytes: extBytes[k]})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].count != rows[j].count {
			return rows[i].count > rows[j].count
		}
		return rows[i].ext < rows[j].ext
	})

	var b strings.Builder
	fmt.Fprintf(&b, "neurofs scan — %s\n", cfg.RepoRoot)
	fmt.Fprintf(&b, "  files indexed : %d\n", stats.Indexed)
	fmt.Fprintf(&b, "  discovered    : %d\n", stats.Discovered)
	fmt.Fprintf(&b, "  skipped       : %d\n", stats.Skipped)
	fmt.Fprintf(&b, "  symbols       : %d\n", stats.Symbols)
	fmt.Fprintf(&b, "  imports       : %d\n", stats.Imports)
	fmt.Fprintf(&b, "  chunks        : %d\n", stats.Chunks)
	fmt.Fprintf(&b, "  total size    : %s\n", humanBytes(totalSize))
	fmt.Fprintf(&b, "  index db      : %s\n", cfg.DBPath)
	fmt.Fprintf(&b, "\n  top extensions:\n")
	limit := len(rows)
	if limit > 10 {
		limit = 10
	}
	for i := 0; i < limit; i++ {
		fmt.Fprintf(&b, "    %-10s %5d files  %s\n", rows[i].ext, rows[i].count, humanBytes(rows[i].bytes))
	}
	return textResult(b.String())
}

// resolveRepo returns the absolute repo root for a path-taking tool.
// When the server was started with a pinned root (Server.SetRepoRoot),
// the caller's `repo` argument is honoured only if it canonicalises to
// the pinned root — anything else is refused. Without pinning, the
// legacy behaviour applies (caller-controlled, default cwd).
func resolveRepo(ctx context.Context, requested string) (string, error) {
	pinned := repoRootFromCtx(ctx)
	requested = strings.TrimSpace(requested)
	if pinned != "" {
		if requested == "" {
			return pinned, nil
		}
		absReq, err := filepath.Abs(requested)
		if err != nil {
			return "", fmt.Errorf("resolve repo: %w", err)
		}
		if !sameDir(absReq, pinned) {
			return "", fmt.Errorf("repo arg %q refused: server is pinned to %q", requested, pinned)
		}
		return pinned, nil
	}
	repo := requested
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

// sameDir compares two paths after symlink resolution and Clean. Used
// to verify a caller's `repo` arg matches the server's pinned root even
// when one side has trailing slashes or symlink prefixes (common on
// macOS, where /var → /private/var).
func sameDir(a, b string) bool {
	aa, err1 := filepath.EvalSymlinks(a)
	bb, err2 := filepath.EvalSymlinks(b)
	if err1 == nil && err2 == nil {
		return filepath.Clean(aa) == filepath.Clean(bb)
	}
	return filepath.Clean(a) == filepath.Clean(b)
}

func textResult(text string) ToolCallResult {
	return ToolCallResult{Content: []Content{{Type: "text", Text: text}}}
}

func errResult(msg string) ToolCallResult {
	return ToolCallResult{
		Content: []Content{{Type: "text", Text: msg}},
		IsError: true,
	}
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for x := n / unit; x >= unit; x /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}

type viewFileArgs struct {
	Path string `json:"path"`
	Repo string `json:"repo"`
}

func runViewFileTool(ctx context.Context, raw json.RawMessage) ToolCallResult {
	var args viewFileArgs
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &args); err != nil {
			return errResult(fmt.Sprintf("invalid arguments: %v", err))
		}
	}
	args.Path = strings.TrimSpace(args.Path)
	if args.Path == "" {
		return errResult("path must not be empty")
	}
	repo, err := resolveRepo(ctx, args.Repo)
	if err != nil {
		return errResult(err.Error())
	}

	// Safely confine file access to the repository root to prevent path traversal
	absPath, err := fsutil.ConfineToRepoStrict(repo, args.Path)
	if err != nil {
		return errResult(err.Error())
	}

	content, err := os.ReadFile(absPath)
	if err != nil {
		return errResult(fmt.Sprintf("read file: %v", err))
	}

	return textResult(string(content))
}

type outlineArgs struct {
	Repo string `json:"repo"`
}

func runGetOutlineTool(ctx context.Context, raw json.RawMessage) ToolCallResult {
	var args outlineArgs
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &args); err != nil {
			return errResult(fmt.Sprintf("invalid arguments: %v", err))
		}
	}
	repo, err := resolveRepo(ctx, args.Repo)
	if err != nil {
		return errResult(err.Error())
	}

	cfg, err := config.New(repo)
	if err != nil {
		return errResult(err.Error())
	}
	db, err := storage.Open(cfg.DBPath)
	if err != nil {
		return errResult(err.Error())
	}
	defer db.Close()

	files, err := db.AllFiles()
	if err != nil {
		return errResult(err.Error())
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "neurofs outline — %d files indexed in %s\n\n", len(files), repo)
	for _, f := range files {
		fmt.Fprintf(&sb, "- %s (%s)\n", f.RelPath, humanBytes(f.Size))
	}
	return textResult(sb.String())
}

type listSignaturesArgs struct {
	Path string `json:"path"`
	Repo string `json:"repo"`
}

func runListSignaturesTool(ctx context.Context, raw json.RawMessage) ToolCallResult {
	var args listSignaturesArgs
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &args); err != nil {
			return errResult(fmt.Sprintf("invalid arguments: %v", err))
		}
	}
	args.Path = strings.TrimSpace(args.Path)
	if args.Path == "" {
		return errResult("path must not be empty")
	}
	repo, err := resolveRepo(ctx, args.Repo)
	if err != nil {
		return errResult(err.Error())
	}

	cfg, err := config.New(repo)
	if err != nil {
		return errResult(err.Error())
	}
	db, err := storage.Open(cfg.DBPath)
	if err != nil {
		return errResult(err.Error())
	}
	defer db.Close()

	rec, err := db.GetFileByRelPath(args.Path)
	if err != nil {
		return errResult(fmt.Sprintf("find file in index: %v", err))
	}

	absPath, err := fsutil.ConfineToRepoStrict(repo, args.Path)
	if err != nil {
		return errResult(err.Error())
	}

	contentBytes, err := os.ReadFile(absPath)
	if err != nil {
		return errResult(fmt.Sprintf("read file: %v", err))
	}

	sf := models.ScoredFile{
		Record: rec,
		Score:  1.0,
	}

	sig := packager.BuildSignature(sf, string(contentBytes))
	return textResult(sig)
}

type getExcerptArgs struct {
	Path  string `json:"path"`
	Query string `json:"query"`
	Repo  string `json:"repo"`
}

func runGetExcerptTool(ctx context.Context, raw json.RawMessage) ToolCallResult {
	var args getExcerptArgs
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &args); err != nil {
			return errResult(fmt.Sprintf("invalid arguments: %v", err))
		}
	}
	args.Path = strings.TrimSpace(args.Path)
	if args.Path == "" {
		return errResult("path must not be empty")
	}
	args.Query = strings.TrimSpace(args.Query)
	if args.Query == "" {
		return errResult("query must not be empty")
	}
	repo, err := resolveRepo(ctx, args.Repo)
	if err != nil {
		return errResult(err.Error())
	}

	cfg, err := config.New(repo)
	if err != nil {
		return errResult(err.Error())
	}
	db, err := storage.Open(cfg.DBPath)
	if err != nil {
		return errResult(err.Error())
	}
	defer db.Close()

	rec, err := db.GetFileByRelPath(args.Path)
	if err != nil {
		return errResult(fmt.Sprintf("find file in index: %v", err))
	}

	absPath, err := fsutil.ConfineToRepoStrict(repo, args.Path)
	if err != nil {
		return errResult(err.Error())
	}

	contentBytes, err := os.ReadFile(absPath)
	if err != nil {
		return errResult(fmt.Sprintf("read file: %v", err))
	}

	terms := ranking.Tokenise(args.Query)
	chunks, err := db.GetChunksForFile(rec.Path)
	if err != nil {
		return errResult(fmt.Sprintf("load chunks: %v", err))
	}
	if excerpt, ok := excerptFromPersistedChunks(rec, string(contentBytes), terms, chunks); ok {
		return textResult(excerpt)
	}

	excerpt, ok := packager.ExtractExcerpt(rec, string(contentBytes), terms)
	if !ok {
		return errResult("could not extract excerpt matching query terms")
	}

	return textResult(excerpt)
}

type searchArgs struct {
	Query string `json:"query"`
	Repo  string `json:"repo"`
	Limit int    `json:"limit"`
	Mode  string `json:"mode"`
}

// SearchOptions configures a reusable NeuroFS chunk search.
type SearchOptions struct {
	Query string
	Repo  string
	Limit int
	Mode  string
}

// SearchResponse is the JSON-serializable result returned by neurofs_search.
type SearchResponse struct {
	Query   string            `json:"query"`
	Mode    string            `json:"mode,omitempty"`
	Results []SearchResultHit `json:"results"`
}

// SearchResultHit is a ranked chunk returned by neurofs_search.
type SearchResultHit struct {
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

// Types and constants are loaded from the retrieval package directly.
type searchResponse = SearchResponse

func runSearchTool(ctx context.Context, raw json.RawMessage) ToolCallResult {
	var args searchArgs
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &args); err != nil {
			return errResult(fmt.Sprintf("invalid arguments: %v", err))
		}
	}
	args.Query = strings.TrimSpace(args.Query)
	if args.Query == "" {
		return errResult("query must not be empty")
	}
	if args.Limit <= 0 {
		args.Limit = 8
	}
	if args.Limit > 50 {
		args.Limit = 50
	}

	response, err := Search(ctx, SearchOptions{
		Query: args.Query,
		Repo:  args.Repo,
		Limit: args.Limit,
		Mode:  args.Mode,
	})
	if err != nil {
		return errResult(err.Error())
	}
	payload, err := json.MarshalIndent(response, "", "  ")
	if err != nil {
		return errResult(fmt.Sprintf("marshal search response: %v", err))
	}
	return textResult(string(payload))
}

// Search runs the same chunk retrieval path exposed by the neurofs_search MCP tool.
func Search(ctx context.Context, opts SearchOptions) (SearchResponse, error) {
	response, err := retrieval.Search(ctx, retrieval.Options{
		Query: opts.Query,
		Repo:  opts.Repo,
		Limit: opts.Limit,
		Mode:  opts.Mode,
	})
	if err != nil {
		return SearchResponse{}, err
	}
	hits := make([]SearchResultHit, 0, len(response.Results))
	for _, hit := range response.Results {
		hits = append(hits, SearchResultHit{
			Path:          hit.Path,
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
	return SearchResponse{
		Query:   response.Query,
		Mode:    response.Mode,
		Results: hits,
	}, nil
}

func excerptFromPersistedChunks(rec models.FileRecord, content string, terms []string, chunks []models.Chunk) (string, bool) {
	if len(terms) == 0 || len(chunks) == 0 {
		return "", false
	}
	var matched []models.Chunk
	for _, chunk := range chunks {
		if chunk.Kind == "file" {
			continue
		}
		snippet := snippetForRange(content, chunk.StartLine, chunk.EndLine)
		score, _ := scoreChunkHit(rec, chunk, snippet, terms)
		if score > 0 {
			matched = append(matched, chunk)
		}
	}
	if len(matched) == 0 {
		return "", false
	}
	sort.SliceStable(matched, func(i, j int) bool {
		if matched[i].StartLine != matched[j].StartLine {
			return matched[i].StartLine < matched[j].StartLine
		}
		return matched[i].ChunkID < matched[j].ChunkID
	})
	if len(matched) > 5 {
		matched = matched[:5]
	}

	lines := splitLogicalLines(content)
	var sb strings.Builder
	fmt.Fprintf(&sb, "// file: %s\n", rec.RelPath)
	fmt.Fprintf(&sb, "// lang: %s\n", rec.Lang)
	fmt.Fprintf(&sb, "// representation: excerpt\n")
	fmt.Fprintf(&sb, "// source: persisted_chunks\n\n")
	prevEnd := 0
	for i, chunk := range matched {
		if i == 0 && chunk.StartLine > 1 {
			fmt.Fprintf(&sb, "// ... %d lines omitted from start ...\n\n", chunk.StartLine-1)
		} else if i > 0 {
			gap := chunk.StartLine - prevEnd - 1
			if gap > 0 {
				fmt.Fprintf(&sb, "\n// ... %d lines omitted ...\n\n", gap)
			}
		}
		label := chunk.Symbol
		if label == "" {
			label = chunk.Kind
		}
		fmt.Fprintf(&sb, "// -- %s:%d-%d (%s) --\n", rec.RelPath, chunk.StartLine, chunk.EndLine, label)
		sb.WriteString(linesInRange(lines, chunk.StartLine, chunk.EndLine))
		prevEnd = chunk.EndLine
	}
	if prevEnd < len(lines) {
		fmt.Fprintf(&sb, "\n// ... %d lines omitted to end ...\n", len(lines)-prevEnd)
	}
	return sb.String(), true
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
	if textMatchesTerms(rec.RelPath, terms) {
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

// Dead search functions removed.

func textMatchesTerms(text string, terms []string) bool {
	for _, term := range terms {
		if termMatchesText(term, text) {
			return true
		}
	}
	return false
}

func termMatchesText(term, text string) bool {
	text = strings.ToLower(text)
	if text == "" || term == "" {
		return false
	}
	for _, variant := range ranking.TermVariants(term) {
		if len(variant) < 3 {
			continue
		}
		if strings.Contains(text, variant) {
			return true
		}
	}
	return false
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
