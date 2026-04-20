// Package benchmark measures ranking precision against a set of curated
// (question → expected-file) pairs. It's meant for regression detection:
// flag changes that silently hurt retrieval quality before they land.
package benchmark

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/neuromfs/neuromfs/internal/models"
	"github.com/neuromfs/neuromfs/internal/packager"
	"github.com/neuromfs/neuromfs/internal/project"
	"github.com/neuromfs/neuromfs/internal/ranking"
)

// Question pairs a natural-language query with the paths the ranker is
// expected to surface near the top. Any one of `Expects` being in the top-k
// counts as a hit — use this when several files are legitimate answers.
//
// ExpectsFacts is an optional set of short substrings that a well-grounded
// answer should mention (e.g. "jwt.sign", "decrement stock"). It is consumed
// by the governance audit, not the retrieval bench — retrieval metrics stay
// unchanged whether or not facts are filled in.
type Question struct {
	Question     string   `json:"question"`
	Expects      []string `json:"expects"`
	ExpectsFacts []string `json:"expects_facts,omitempty"`
	Note         string   `json:"note,omitempty"` // optional human hint
}

// LoadQuestions reads a JSON file containing a list of Questions. Empty
// files or missing paths produce a descriptive error so users know what
// they forgot to configure.
func LoadQuestions(path string) ([]Question, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("benchmark: read %s: %w", path, err)
	}
	var qs []Question
	if err := json.Unmarshal(data, &qs); err != nil {
		return nil, fmt.Errorf("benchmark: parse %s: %w", path, err)
	}
	if len(qs) == 0 {
		return nil, fmt.Errorf("benchmark: %s contains no questions", path)
	}
	return qs, nil
}

// Result captures how a single question fared against the ranker.
//
// BundleTokens is only populated when RunOptions.ComputeBundle is true —
// it costs one full packager.Pack per question, which reads every selected
// file from disk, so callers opt in.
type Result struct {
	Question     string
	Expects      []string
	Top          []Ranked // top N files from ranker, N = RunOptions.KeepTop
	HitRank      int      // 1-based rank of the first expected file, or 0 for miss
	MatchedAt    string   // the expected file that hit, if any
	BundleTokens int      // tokens the packager would actually spend on this question
	BundleFiles  int      // files the packager would include
}

// Ranked is a projection of models.ScoredFile for benchmark output.
type Ranked struct {
	Path  string
	Score float64
}

// RunOptions tunes benchmark behaviour.
type RunOptions struct {
	// TopK is the cut-off for counting a hit (default 3).
	TopK int
	// KeepTop controls how many top-N rows each Result carries for display
	// (default 5). Does not affect correctness.
	KeepTop int
	// Project is forwarded to the ranker to exercise project signals.
	Project *project.Info
	// ComputeBundle, when true, packages each question into a bundle so the
	// summary includes mean/p50/p95 token counts. Off by default because it
	// hits the filesystem once per question.
	ComputeBundle bool
	// PackBudget controls the token budget used when ComputeBundle is on.
	// Zero falls back to 8000 (the config default) to mirror real usage.
	PackBudget int
	// PreferSignatures mirrors `pack --for claude` — the knob lets CI bench
	// the same compression policy users actually run with.
	PreferSignatures bool
}

// Summary rolls up the per-question Results into headline metrics.
// Bundle* fields are zero unless RunOptions.ComputeBundle was set.
type Summary struct {
	Questions int
	Hits      int     // questions where an expected file was in TopK
	Top1      float64 // fraction of questions with a hit at rank 1
	Top3      float64
	Top5      float64
	MeanRank  float64 // average 1-based rank of hits; misses excluded

	BundleMeanTokens int // average tokens per bundle across questions
	BundleP50Tokens  int // 50th percentile
	BundleP95Tokens  int // 95th percentile
	BundleMeanFiles  int // average files included per bundle
}

// Run ranks files for each question and returns per-question Results plus
// aggregated metrics. Missing expected files (typos in the benchmark) are
// reported via Result.HitRank == 0.
func Run(files []models.FileRecord, questions []Question, opts RunOptions) ([]Result, Summary) {
	if opts.TopK <= 0 {
		opts.TopK = 3
	}
	if opts.KeepTop <= 0 {
		opts.KeepTop = 5
	}

	results := make([]Result, 0, len(questions))
	var (
		hits, top1, top3, top5 int
		rankSum                int
	)

	packBudget := opts.PackBudget
	if packBudget == 0 {
		packBudget = 8000
	}

	for _, q := range questions {
		ranked := ranking.RankWithOptions(files, q.Question, ranking.Options{Project: opts.Project})

		keep := opts.KeepTop
		if keep > len(ranked) {
			keep = len(ranked)
		}
		top := make([]Ranked, keep)
		for i := 0; i < keep; i++ {
			top[i] = Ranked{Path: ranked[i].Record.RelPath, Score: ranked[i].Score}
		}

		hitRank := 0
		matched := ""
		for i, r := range ranked {
			if matchesAny(r.Record.RelPath, q.Expects) {
				hitRank = i + 1
				matched = r.Record.RelPath
				break
			}
		}

		if hitRank > 0 && hitRank <= opts.TopK {
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

		res := Result{
			Question:  q.Question,
			Expects:   q.Expects,
			Top:       top,
			HitRank:   hitRank,
			MatchedAt: matched,
		}

		if opts.ComputeBundle {
			bundle, err := packager.Pack(ranked, q.Question, packager.Options{
				Budget:           packBudget,
				PreferSignatures: opts.PreferSignatures,
			})
			if err == nil {
				res.BundleTokens = bundle.Stats.TokensUsed
				res.BundleFiles = bundle.Stats.FilesIncluded
			}
		}

		results = append(results, res)
	}

	n := float64(len(questions))
	summary := Summary{
		Questions: len(questions),
		Hits:      hits,
		Top1:      percentage(top1, n),
		Top3:      percentage(top3, n),
		Top5:      percentage(top5, n),
	}
	if hits > 0 {
		summary.MeanRank = float64(rankSum) / float64(hits)
	}
	if opts.ComputeBundle {
		applyBundleMetrics(&summary, results)
	}

	return results, summary
}

// applyBundleMetrics fills Summary.Bundle* from the per-question sizes.
// We compute p50/p95 with a simple linear-interpolation-free method: sort
// the observed values, then index at floor(N * q). That's accurate enough
// for the small Ns we see in practice (single-digit to ~100 questions).
func applyBundleMetrics(summary *Summary, results []Result) {
	if len(results) == 0 {
		return
	}
	tokens := make([]int, 0, len(results))
	filesSum := 0
	tokensSum := 0
	for _, r := range results {
		tokens = append(tokens, r.BundleTokens)
		tokensSum += r.BundleTokens
		filesSum += r.BundleFiles
	}
	sort.Ints(tokens)
	n := len(tokens)
	summary.BundleMeanTokens = tokensSum / n
	summary.BundleMeanFiles = filesSum / n
	summary.BundleP50Tokens = tokens[pctileIdx(n, 0.50)]
	summary.BundleP95Tokens = tokens[pctileIdx(n, 0.95)]
}

// pctileIdx returns the index into a sorted slice of length n for the q-th
// quantile. Clamps to [0, n-1] so callers never index out of bounds.
func pctileIdx(n int, q float64) int {
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

// FormatResults prints a human-readable report to buf.
func FormatResults(w *strings.Builder, results []Result, summary Summary, topK int) {
	fmt.Fprintf(w, "  questions : %d\n", summary.Questions)
	fmt.Fprintf(w, "  top-k     : %d\n\n", topK)

	for _, r := range results {
		mark := "✗"
		if r.HitRank >= 1 && r.HitRank <= topK {
			mark = "✓"
		}
		fmt.Fprintf(w, "  [%s] %q\n", mark, r.Question)
		fmt.Fprintf(w, "       expects : %s\n", strings.Join(r.Expects, ", "))
		if r.HitRank > 0 {
			fmt.Fprintf(w, "       matched : %s (rank %d)\n", r.MatchedAt, r.HitRank)
		} else {
			fmt.Fprintf(w, "       matched : (none)\n")
		}
		parts := make([]string, 0, len(r.Top))
		for _, t := range r.Top {
			parts = append(parts, fmt.Sprintf("%s(%.2f)", t.Path, t.Score))
		}
		fmt.Fprintf(w, "       top %-2d  : %s\n\n", len(r.Top), strings.Join(parts, ", "))
	}

	fmt.Fprintf(w, "  summary:\n")
	fmt.Fprintf(w, "    hits      : %d / %d\n", summary.Hits, summary.Questions)
	fmt.Fprintf(w, "    top-1     : %.1f%%\n", summary.Top1)
	fmt.Fprintf(w, "    top-3     : %.1f%%\n", summary.Top3)
	fmt.Fprintf(w, "    top-5     : %.1f%%\n", summary.Top5)
	if summary.MeanRank > 0 {
		fmt.Fprintf(w, "    mean rank : %.2f (hits only)\n", summary.MeanRank)
	}
	if summary.BundleMeanTokens > 0 {
		fmt.Fprintf(w, "    bundle mean tokens : %d  (p50 %d, p95 %d)\n",
			summary.BundleMeanTokens, summary.BundleP50Tokens, summary.BundleP95Tokens)
		fmt.Fprintf(w, "    bundle mean files  : %d\n", summary.BundleMeanFiles)
	}
}

// matchesAny returns true when rel equals any expected path (exact match
// after normalising leading "./" and OS separators).
func matchesAny(rel string, expected []string) bool {
	norm := strings.TrimPrefix(rel, "./")
	for _, e := range expected {
		if norm == strings.TrimPrefix(e, "./") {
			return true
		}
	}
	return false
}

func percentage(n int, total float64) float64 {
	if total == 0 {
		return 0
	}
	return float64(n) / total * 100
}

// SortedByScore is a small utility for callers that want ranked output
// independent of the ranker's own sort.
func SortedByScore(files []Ranked) []Ranked {
	out := make([]Ranked, len(files))
	copy(out, files)
	sort.Slice(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	return out
}
