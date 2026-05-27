package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/neuromfs/neuromfs/internal/audit"
	"github.com/neuromfs/neuromfs/internal/benchmark"
	"github.com/neuromfs/neuromfs/internal/config"
	"github.com/neuromfs/neuromfs/internal/embeddings"
	"github.com/neuromfs/neuromfs/internal/mcp"
	"github.com/neuromfs/neuromfs/internal/retrieval"
	"github.com/neuromfs/neuromfs/internal/storage"
	"github.com/neuromfs/neuromfs/internal/tokenbudget"
	"github.com/spf13/cobra"
)

func newBenchCmd() *cobra.Command {
	var (
		repoPath             string
		benchArg             string
		topK                 int
		minTop3              float64
		bundle               bool
		packBudget           int
		preferSignatures     bool
		maxMeanBundleTokens  int
		search               bool
		searchLimit          int
		searchStability      bool
		maxMeanSearchTokens  int
		contextBench         bool
		contextLimit         int
		maxMeanContextTokens int
	)

	cmd := &cobra.Command{
		Use:   "bench [benchmark.json]",
		Short: "Measure top-k precision against a curated question set",
		Long: `Bench reads a list of (question, expected-files) pairs and reports
how often the ranker surfaces an expected file in the top-k results.

The default location of the benchmark file is <repo>/.neurofs-bench.json.

Use this to spot-check ranking changes before they regress real queries.
With --min-top3 you can fail the command (non-zero exit) when precision
drops below a threshold — wire this into CI as a ranking regression gate.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.New(repoPath)
			if err != nil {
				return fmt.Errorf("bench: %w", err)
			}
			if err := cfg.Validate(); err != nil {
				return fmt.Errorf("bench: config: %w", err)
			}

			benchPath := benchArg
			if len(args) > 0 {
				benchPath = args[0]
			}
			if benchPath == "" {
				benchPath = filepath.Join(cfg.RepoRoot, ".neurofs-bench.json")
			}

			questions, err := benchmark.LoadQuestions(benchPath)
			if err != nil {
				return fmt.Errorf("bench: %w", err)
			}

			db, err := storage.Open(cfg.DBPath)
			if err != nil {
				return fmt.Errorf("bench: open index (did you run 'neurofs scan'?): %w", err)
			}
			defer db.Close()

			count, err := db.FileCount()
			if err != nil {
				return fmt.Errorf("bench: %w", err)
			}
			if count == 0 {
				return fmt.Errorf("bench: index is empty — run 'neurofs scan' first")
			}

			files, err := db.AllFiles()
			if err != nil {
				return fmt.Errorf("bench: %w", err)
			}

			embClient := embeddings.NewClient(cfg.HybridMode)
			fileEmbs, _ := db.AllEmbeddings()
			rels, _ := db.AllRelations()

			results, summary := benchmark.Run(files, questions, benchmark.RunOptions{
				TopK:             topK,
				Project:          loadProjectInfo(db),
				ComputeBundle:    bundle || maxMeanBundleTokens > 0,
				PackBudget:       packBudget,
				PreferSignatures: preferSignatures,
				Embeddings:       fileEmbs,
				EmbClient:        embClient,
				Relations:        rels,
			})

			fmt.Fprintf(os.Stderr, "NeuroFS — benchmark on %s\n", cfg.RepoRoot)
			fmt.Fprintf(os.Stderr, "  questions : %s\n\n", benchPath)

			var sb strings.Builder
			benchmark.FormatResults(&sb, results, summary, topK)
			fmt.Fprint(os.Stderr, sb.String())

			var searchSummary searchBenchSummary
			if search || maxMeanSearchTokens > 0 {
				searchResults, s, err := runSearchBenchmark(cmd.Context(), cfg.RepoRoot, questions, searchLimit, topK, searchStability)
				if err != nil {
					return fmt.Errorf("bench search: %w", err)
				}
				searchSummary = s
				var searchOut strings.Builder
				formatSearchBenchmark(&searchOut, searchResults, searchSummary, topK, summary.BundleMeanTokens)
				fmt.Fprint(os.Stderr, searchOut.String())
			}

			var contextSummary contextBenchSummary
			if contextBench || maxMeanContextTokens > 0 {
				contextResults, s, err := runContextBenchmark(cmd.Context(), cfg.RepoRoot, questions, contextLimit, topK)
				if err != nil {
					return fmt.Errorf("bench context: %w", err)
				}
				contextSummary = s
				var contextOut strings.Builder
				formatContextBenchmark(&contextOut, contextResults, contextSummary, topK)
				fmt.Fprint(os.Stderr, contextOut.String())
			}

			if minTop3 > 0 && summary.Top3 < minTop3 {
				return fmt.Errorf("bench: top-3 precision %.1f%% below threshold %.1f%%",
					summary.Top3, minTop3)
			}
			if maxMeanBundleTokens > 0 && summary.BundleMeanTokens > maxMeanBundleTokens {
				return fmt.Errorf("bench: mean bundle tokens %d exceeds ceiling %d",
					summary.BundleMeanTokens, maxMeanBundleTokens)
			}
			if maxMeanSearchTokens > 0 && searchSummary.SearchMeanTokens > maxMeanSearchTokens {
				return fmt.Errorf("bench: mean search tokens %d exceeds ceiling %d",
					searchSummary.SearchMeanTokens, maxMeanSearchTokens)
			}
			if maxMeanContextTokens > 0 && contextSummary.ContextMeanTokens > maxMeanContextTokens {
				return fmt.Errorf("bench: mean context tokens %d exceeds ceiling %d",
					contextSummary.ContextMeanTokens, maxMeanContextTokens)
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&repoPath, "repo", "", "Repository root (defaults to current directory)")
	cmd.Flags().StringVar(&benchArg, "file", "", "Path to benchmark JSON (defaults to <repo>/.neurofs-bench.json)")
	cmd.Flags().IntVar(&topK, "top-k", 3, "Rank cut-off for counting a question as a hit")
	cmd.Flags().Float64Var(&minTop3, "min-top3", 0, "Fail with non-zero exit when top-3 precision drops below this %")
	cmd.Flags().BoolVar(&bundle, "bundle", false, "Also pack a bundle per question and report mean/p50/p95 tokens")
	cmd.Flags().IntVar(&packBudget, "pack-budget", 0, "Token budget used with --bundle (default 8000)")
	cmd.Flags().BoolVar(&preferSignatures, "prefer-signatures", false, "Mirror --for claude compression when measuring bundles")
	cmd.Flags().IntVar(&maxMeanBundleTokens, "max-mean-bundle-tokens", 0, "Fail when the mean bundle token count exceeds this ceiling")
	cmd.Flags().BoolVar(&search, "search", false, "Also run neurofs_search per question and report top-k, token, and stability metrics")
	cmd.Flags().IntVar(&searchLimit, "search-limit", 5, "Number of neurofs_search chunk hits to keep per benchmark question")
	cmd.Flags().BoolVar(&searchStability, "search-stability", false, "Run neurofs_search twice per question and compare stable JSON prefixes")
	cmd.Flags().IntVar(&maxMeanSearchTokens, "max-mean-search-tokens", 0, "Fail when mean neurofs_search token count exceeds this ceiling")
	cmd.Flags().BoolVar(&contextBench, "context", false, "Also run neurofs_context per question and report routing, token, and top-k metrics")
	cmd.Flags().IntVar(&contextLimit, "context-limit", 5, "Number of neurofs_context hits to keep per benchmark question")
	cmd.Flags().IntVar(&maxMeanContextTokens, "max-mean-context-tokens", 0, "Fail when mean neurofs_context output token count exceeds this ceiling")
	return cmd
}

type searchBenchResult struct {
	Question     string
	Expects      []string
	Top          []benchmark.Ranked
	HitRank      int
	MatchedAt    string
	SearchTokens int
	Latency      time.Duration
	FactRecall   float64
	FactsHit     []string
	StablePrefix bool
}

type searchBenchSummary struct {
	Questions        int
	Hits             int
	Top1             float64
	Top3             float64
	Top5             float64
	MeanRank         float64
	SearchMeanTokens int
	SearchP50Tokens  int
	SearchP95Tokens  int
	LatencyP50       time.Duration
	LatencyP95       time.Duration
	FactRecall       float64
	FactQuestions    int
	StablePrefixes   int
	Stability        float64
	StabilityChecked bool
}

type contextBenchResult struct {
	Question        string
	Expects         []string
	Route           string
	Top             []benchmark.Ranked
	HitRank         int
	MatchedAt       string
	ContextTokens   int
	Latency         time.Duration
	StructuralHints int
}

type contextBenchSummary struct {
	Questions         int
	Hits              int
	Top1              float64
	Top3              float64
	Top5              float64
	MeanRank          float64
	ContextMeanTokens int
	ContextP50Tokens  int
	ContextP95Tokens  int
	LatencyP50        time.Duration
	LatencyP95        time.Duration
	Routes            map[string]int
	StructuralHints   int
}

func runSearchBenchmark(ctx context.Context, repo string, questions []benchmark.Question, limit, topK int, checkStability bool) ([]searchBenchResult, searchBenchSummary, error) {
	if limit <= 0 {
		limit = 5
	}
	if topK <= 0 {
		topK = 3
	}
	if limit < topK {
		limit = topK
	}

	results := make([]searchBenchResult, 0, len(questions))
	var hits, top1, top3, top5, rankSum, stable int
	var factRecallSum float64
	var factQuestions int
	for _, q := range questions {
		start := time.Now()
		resp, err := retrieval.Search(ctx, retrieval.Options{
			Query: q.Question,
			Repo:  repo,
			Limit: limit,
		})
		latency := time.Since(start)
		if err != nil {
			return nil, searchBenchSummary{}, err
		}

		top := make([]benchmark.Ranked, 0, len(resp.Results))
		var snippets strings.Builder
		tokenSum := 0
		hitRank := 0
		matched := ""
		for i, hit := range resp.Results {
			top = append(top, benchmark.Ranked{Path: hit.Path, Score: hit.Score})
			tokenSum += hit.TokenEstimate
			snippets.WriteString(hit.Snippet)
			snippets.WriteString("\n")
			if hitRank == 0 && searchMatchesAny(hit.Path, q.Expects) {
				hitRank = i + 1
				matched = hit.Path
			}
		}
		var factsHit []string
		var factRecall float64
		if len(q.ExpectsFacts) > 0 {
			factsHit, factRecall = audit.ScoreFacts(snippets.String(), q.ExpectsFacts)
			factRecallSum += factRecall
			factQuestions++
		}

		stablePrefix := false
		if checkStability {
			again, err := retrieval.Search(ctx, retrieval.Options{
				Query: q.Question,
				Repo:  repo,
				Limit: limit,
			})
			if err != nil {
				return nil, searchBenchSummary{}, err
			}
			stablePrefix = stableSearchPrefix(resp, again, 2048)
			if stablePrefix {
				stable++
			}
		}

		if hitRank > 0 && hitRank <= topK {
			hits++
			rankSum += hitRank
		}
		if hitRank == 1 {
			top1++
		}
		if hitRank >= 1 && hitRank <= 3 {
			top3++
		}
		if hitRank >= 1 && hitRank <= 5 {
			top5++
		}

		results = append(results, searchBenchResult{
			Question:     q.Question,
			Expects:      q.Expects,
			Top:          top,
			HitRank:      hitRank,
			MatchedAt:    matched,
			SearchTokens: tokenSum,
			Latency:      latency,
			FactRecall:   factRecall,
			FactsHit:     factsHit,
			StablePrefix: stablePrefix,
		})
	}

	n := float64(len(questions))
	summary := searchBenchSummary{
		Questions:        len(questions),
		Hits:             hits,
		Top1:             pct(top1, n),
		Top3:             pct(top3, n),
		Top5:             pct(top5, n),
		StablePrefixes:   stable,
		Stability:        pct(stable, n),
		StabilityChecked: checkStability,
		FactQuestions:    factQuestions,
	}
	if hits > 0 {
		summary.MeanRank = float64(rankSum) / float64(hits)
	}
	if factQuestions > 0 {
		summary.FactRecall = factRecallSum / float64(factQuestions) * 100
	}
	applySearchTokenMetrics(&summary, results)
	applySearchLatencyMetrics(&summary, results)
	return results, summary, nil
}

func runContextBenchmark(ctx context.Context, repo string, questions []benchmark.Question, limit, topK int) ([]contextBenchResult, contextBenchSummary, error) {
	if limit <= 0 {
		limit = 5
	}
	if topK <= 0 {
		topK = 3
	}
	if limit < topK {
		limit = topK
	}

	results := make([]contextBenchResult, 0, len(questions))
	var hits, top1, top3, top5, rankSum, structuralHints int
	routes := make(map[string]int)
	for _, q := range questions {
		start := time.Now()
		resp, err := mcp.Context(ctx, mcp.ContextOptions{
			Query: q.Question,
			Repo:  repo,
			Limit: limit,
		})
		latency := time.Since(start)
		if err != nil {
			return nil, contextBenchSummary{}, err
		}

		top := rankedFromContextResponse(resp, limit)
		tokenPayload, _ := json.Marshal(resp)
		tokenCount := tokenbudget.EstimateTokens(string(tokenPayload))
		route := resp.Route
		if route == "" {
			route = "(none)"
		}
		routes[route]++
		structuralHints += len(resp.StructuralHints)

		hitRank := 0
		matched := ""
		for i, ranked := range top {
			if searchMatchesAny(ranked.Path, q.Expects) {
				hitRank = i + 1
				matched = ranked.Path
				break
			}
		}
		if hitRank > 0 && hitRank <= topK {
			hits++
			rankSum += hitRank
		}
		if hitRank == 1 {
			top1++
		}
		if hitRank >= 1 && hitRank <= 3 {
			top3++
		}
		if hitRank >= 1 && hitRank <= 5 {
			top5++
		}

		results = append(results, contextBenchResult{
			Question:        q.Question,
			Expects:         q.Expects,
			Route:           route,
			Top:             top,
			HitRank:         hitRank,
			MatchedAt:       matched,
			ContextTokens:   tokenCount,
			Latency:         latency,
			StructuralHints: len(resp.StructuralHints),
		})
	}

	n := float64(len(questions))
	summary := contextBenchSummary{
		Questions:       len(questions),
		Hits:            hits,
		Top1:            pct(top1, n),
		Top3:            pct(top3, n),
		Top5:            pct(top5, n),
		Routes:          routes,
		StructuralHints: structuralHints,
	}
	if hits > 0 {
		summary.MeanRank = float64(rankSum) / float64(hits)
	}
	applyContextTokenMetrics(&summary, results)
	applyContextLatencyMetrics(&summary, results)
	return results, summary, nil
}

func rankedFromContextResponse(resp mcp.ContextResponse, limit int) []benchmark.Ranked {
	if limit <= 0 {
		limit = 5
	}
	top := make([]benchmark.Ranked, 0, limit)
	seen := make(map[string]bool, limit)
	add := func(path string, score float64) {
		path = strings.TrimPrefix(filepath.ToSlash(strings.TrimSpace(path)), "./")
		if path == "" || seen[path] || len(top) >= limit {
			return
		}
		seen[path] = true
		top = append(top, benchmark.Ranked{Path: path, Score: score})
	}
	for _, hit := range resp.Results {
		add(hit.Path, hit.Score)
	}
	for _, path := range contextPathsFromPrompt(resp.Prompt) {
		add(path, 1.0/float64(len(top)+1))
	}
	for _, path := range contextPathsFromExcerpt(resp.Text) {
		add(path, 1.0/float64(len(top)+1))
	}
	return top
}

func contextPathsFromPrompt(prompt string) []string {
	var paths []string
	rest := prompt
	for {
		idx := strings.Index(rest, `<file path="`)
		if idx < 0 {
			return paths
		}
		rest = rest[idx+len(`<file path="`):]
		end := strings.Index(rest, `"`)
		if end < 0 {
			return paths
		}
		paths = append(paths, rest[:end])
		rest = rest[end+1:]
	}
}

func contextPathsFromExcerpt(text string) []string {
	var paths []string
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "// file:") {
			paths = append(paths, strings.TrimSpace(strings.TrimPrefix(line, "// file:")))
		}
	}
	return paths
}

func formatSearchBenchmark(w *strings.Builder, results []searchBenchResult, summary searchBenchSummary, topK int, bundleMeanTokens int) {
	fmt.Fprintf(w, "\n  neurofs_search:\n")
	fmt.Fprintf(w, "    questions : %d\n", summary.Questions)
	fmt.Fprintf(w, "    top-k     : %d\n\n", topK)

	for _, r := range results {
		mark := "✗"
		if r.HitRank >= 1 && r.HitRank <= topK {
			mark = "✓"
		}
		fmt.Fprintf(w, "    [%s] %q\n", mark, r.Question)
		fmt.Fprintf(w, "         expects : %s\n", strings.Join(r.Expects, ", "))
		if r.HitRank > 0 {
			fmt.Fprintf(w, "         matched : %s (rank %d)\n", r.MatchedAt, r.HitRank)
		} else {
			fmt.Fprintf(w, "         matched : (none)\n")
		}
		parts := make([]string, 0, len(r.Top))
		for _, t := range r.Top {
			parts = append(parts, fmt.Sprintf("%s(%.2f)", t.Path, t.Score))
		}
		fmt.Fprintf(w, "         top %-2d  : %s\n", len(r.Top), strings.Join(parts, ", "))
		fmt.Fprintf(w, "         tokens   : %d\n", r.SearchTokens)
		fmt.Fprintf(w, "         latency  : %s\n", r.Latency.Round(time.Millisecond))
		if len(r.FactsHit) > 0 || r.FactRecall > 0 {
			fmt.Fprintf(w, "         facts    : %.1f%% (%d hit)\n", r.FactRecall*100, len(r.FactsHit))
		}
		if summary.StabilityChecked {
			fmt.Fprintf(w, "         stable   : %t\n", r.StablePrefix)
		}
		fmt.Fprintf(w, "\n")
	}

	fmt.Fprintf(w, "    summary:\n")
	fmt.Fprintf(w, "      hits      : %d / %d\n", summary.Hits, summary.Questions)
	fmt.Fprintf(w, "      top-1     : %.1f%%\n", summary.Top1)
	fmt.Fprintf(w, "      top-3     : %.1f%%\n", summary.Top3)
	fmt.Fprintf(w, "      top-5     : %.1f%%\n", summary.Top5)
	if summary.MeanRank > 0 {
		fmt.Fprintf(w, "      mean rank : %.2f (hits only)\n", summary.MeanRank)
	}
	fmt.Fprintf(w, "      search mean tokens : %d  (p50 %d, p95 %d)\n",
		summary.SearchMeanTokens, summary.SearchP50Tokens, summary.SearchP95Tokens)
	fmt.Fprintf(w, "      search latency     : p50 %s, p95 %s\n",
		summary.LatencyP50.Round(time.Millisecond), summary.LatencyP95.Round(time.Millisecond))
	if summary.FactQuestions > 0 {
		fmt.Fprintf(w, "      search fact recall : %.1f%% over %d question(s)\n",
			summary.FactRecall, summary.FactQuestions)
	}
	if bundleMeanTokens > 0 && summary.SearchMeanTokens > 0 {
		ratio := float64(summary.SearchMeanTokens) / float64(bundleMeanTokens)
		fmt.Fprintf(w, "      search/bundle tokens : %.2fx\n", ratio)
	}
	if summary.StabilityChecked {
		fmt.Fprintf(w, "      stable prefixes    : %d / %d (%.1f%%)\n",
			summary.StablePrefixes, summary.Questions, summary.Stability)
	}
}

func formatContextBenchmark(w *strings.Builder, results []contextBenchResult, summary contextBenchSummary, topK int) {
	fmt.Fprintf(w, "\n  neurofs_context:\n")
	fmt.Fprintf(w, "    questions : %d\n", summary.Questions)
	fmt.Fprintf(w, "    top-k     : %d\n\n", topK)

	for _, r := range results {
		mark := "✗"
		if r.HitRank >= 1 && r.HitRank <= topK {
			mark = "✓"
		}
		fmt.Fprintf(w, "    [%s] %q\n", mark, r.Question)
		fmt.Fprintf(w, "         route   : %s\n", r.Route)
		fmt.Fprintf(w, "         expects : %s\n", strings.Join(r.Expects, ", "))
		if r.HitRank > 0 {
			fmt.Fprintf(w, "         matched : %s (rank %d)\n", r.MatchedAt, r.HitRank)
		} else {
			fmt.Fprintf(w, "         matched : (none)\n")
		}
		parts := make([]string, 0, len(r.Top))
		for _, t := range r.Top {
			parts = append(parts, fmt.Sprintf("%s(%.2f)", t.Path, t.Score))
		}
		fmt.Fprintf(w, "         top %-2d  : %s\n", len(r.Top), strings.Join(parts, ", "))
		fmt.Fprintf(w, "         tokens  : %d\n", r.ContextTokens)
		fmt.Fprintf(w, "         latency : %s\n", r.Latency.Round(time.Millisecond))
		fmt.Fprintf(w, "         hints   : %d\n\n", r.StructuralHints)
	}

	fmt.Fprintf(w, "    summary:\n")
	fmt.Fprintf(w, "      hits      : %d / %d\n", summary.Hits, summary.Questions)
	fmt.Fprintf(w, "      top-1     : %.1f%%\n", summary.Top1)
	fmt.Fprintf(w, "      top-3     : %.1f%%\n", summary.Top3)
	fmt.Fprintf(w, "      top-5     : %.1f%%\n", summary.Top5)
	if summary.MeanRank > 0 {
		fmt.Fprintf(w, "      mean rank : %.2f (hits only)\n", summary.MeanRank)
	}
	fmt.Fprintf(w, "      context mean tokens : %d  (p50 %d, p95 %d)\n",
		summary.ContextMeanTokens, summary.ContextP50Tokens, summary.ContextP95Tokens)
	fmt.Fprintf(w, "      context latency     : p50 %s, p95 %s\n",
		summary.LatencyP50.Round(time.Millisecond), summary.LatencyP95.Round(time.Millisecond))
	fmt.Fprintf(w, "      structural hints    : %d total\n", summary.StructuralHints)
	if len(summary.Routes) > 0 {
		fmt.Fprintf(w, "      routes              : %s\n", formatRouteCounts(summary.Routes))
	}
}

func applySearchTokenMetrics(summary *searchBenchSummary, results []searchBenchResult) {
	if len(results) == 0 {
		return
	}
	tokens := make([]int, 0, len(results))
	sum := 0
	for _, result := range results {
		tokens = append(tokens, result.SearchTokens)
		sum += result.SearchTokens
	}
	sort.Ints(tokens)
	summary.SearchMeanTokens = sum / len(results)
	summary.SearchP50Tokens = tokens[searchPctileIdx(len(tokens), 0.50)]
	summary.SearchP95Tokens = tokens[searchPctileIdx(len(tokens), 0.95)]
}

func applyContextTokenMetrics(summary *contextBenchSummary, results []contextBenchResult) {
	if len(results) == 0 {
		return
	}
	tokens := make([]int, 0, len(results))
	sum := 0
	for _, result := range results {
		tokens = append(tokens, result.ContextTokens)
		sum += result.ContextTokens
	}
	sort.Ints(tokens)
	summary.ContextMeanTokens = sum / len(results)
	summary.ContextP50Tokens = tokens[searchPctileIdx(len(tokens), 0.50)]
	summary.ContextP95Tokens = tokens[searchPctileIdx(len(tokens), 0.95)]
}

func applyContextLatencyMetrics(summary *contextBenchSummary, results []contextBenchResult) {
	if len(results) == 0 {
		return
	}
	latencies := make([]time.Duration, 0, len(results))
	for _, result := range results {
		latencies = append(latencies, result.Latency)
	}
	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	summary.LatencyP50 = latencies[searchPctileIdx(len(latencies), 0.50)]
	summary.LatencyP95 = latencies[searchPctileIdx(len(latencies), 0.95)]
}

func formatRouteCounts(routes map[string]int) string {
	names := make([]string, 0, len(routes))
	for name := range routes {
		names = append(names, name)
	}
	sort.Strings(names)
	parts := make([]string, 0, len(names))
	for _, name := range names {
		parts = append(parts, fmt.Sprintf("%s:%d", name, routes[name]))
	}
	return strings.Join(parts, ", ")
}

func applySearchLatencyMetrics(summary *searchBenchSummary, results []searchBenchResult) {
	if len(results) == 0 {
		return
	}
	latencies := make([]time.Duration, 0, len(results))
	for _, result := range results {
		latencies = append(latencies, result.Latency)
	}
	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	summary.LatencyP50 = latencies[searchPctileIdx(len(latencies), 0.50)]
	summary.LatencyP95 = latencies[searchPctileIdx(len(latencies), 0.95)]
}

func searchPctileIdx(n int, q float64) int {
	if n <= 1 {
		return 0
	}
	i := int(float64(n-1) * q)
	if i < 0 {
		return 0
	}
	if i >= n {
		return n - 1
	}
	return i
}

func searchMatchesAny(rel string, expected []string) bool {
	norm := strings.TrimPrefix(filepath.ToSlash(rel), "./")
	for _, e := range expected {
		if norm == strings.TrimPrefix(filepath.ToSlash(e), "./") {
			return true
		}
	}
	return false
}

func stableSearchPrefix(a, b retrieval.Response, n int) bool {
	ba, errA := json.Marshal(a)
	bb, errB := json.Marshal(b)
	if errA != nil || errB != nil {
		return false
	}
	if n <= 0 || n > len(ba) {
		n = len(ba)
	}
	if n > len(bb) {
		return false
	}
	return string(ba[:n]) == string(bb[:n])
}

func pct(n int, total float64) float64 {
	if total == 0 {
		return 0
	}
	return float64(n) / total * 100
}
