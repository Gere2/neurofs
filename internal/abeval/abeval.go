// Package abeval runs the Phase-0 economy experiment: an A/B comparison of how
// many context tokens it costs to ground a set of tasks two ways.
//
//	A (baseline) — native retrieval: read whole files until the answer is in hand.
//	B (NeuroFS)  — neurofs_search: targeted, citable excerpts (line ranges).
//
// The comparison is iso-recall, which is the question Phase 0 actually asks:
// "to reach equal-or-better fact recall, how many context tokens does each
// strategy deliver?". For every task:
//
//  1. Arm B runs neurofs_search and we record its snippet tokens and the fact
//     recall over those snippets (the same audit.ScoreFacts scorer the gate uses).
//  2. Arm A reads the whole files B's hits came from, in hit order, accumulating
//     until its recall reaches B's recall. Because a whole file is a superset of
//     any excerpt of it, the baseline is guaranteed to reach B's recall — so the
//     two arms are compared at the SAME recall and the only variable is tokens.
//
// The headline metric is the mean iso-recall token reduction (1 - tokensB/tokensA).
//
// Why this baseline is honest, not flattering: the native arm reads the files
// NeuroFS itself surfaced (NeuroFS-quality selection, for free) and only stops
// once it has matched NeuroFS's recall. That makes the measured savings a lower
// bound on NeuroFS's real advantage over a naive agent that opens more files.
//
// Proxy boundary: this measures single-iteration context-delivery efficiency
// for a fixed retrieval target. It does NOT measure end-to-end agent task
// success, nor the agent's re-derivation cost across loop turns. See
// docs/phase0_economy.md.
package abeval

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/neuromfs/neuromfs/internal/audit"
	"github.com/neuromfs/neuromfs/internal/models"
	"github.com/neuromfs/neuromfs/internal/tokenbudget"
)

// DefaultThreshold is the minimum mean iso-recall token reduction for a PASS.
// Matches the Phase-0 stop condition (>=25% fewer tokens at equal recall).
const DefaultThreshold = 0.25

// DefaultSearchLimit is how many neurofs_search hits arm B keeps per task.
const DefaultSearchLimit = 8

// SearchHit is one excerpt arm B delivered: the file it came from, the snippet
// text scored for facts, and the tokens that snippet costs.
type SearchHit struct {
	Path    string // repo-relative path the excerpt came from
	Snippet string // excerpt text (citable range)
	Tokens  int    // token cost of the snippet
}

// SearchFn is arm B: given a query it returns the excerpts neurofs_search would
// deliver, in rank order. Injected so the package is testable without a live
// index; the CLI wires it to retrieval.Search.
type SearchFn func(ctx context.Context, query string, limit int) ([]SearchHit, error)

// Task is one A/B unit: a question plus the facts a grounded answer must cite.
type Task struct {
	Question     string   `json:"question"`
	ExpectsFacts []string `json:"expects_facts"`
	Source       string   `json:"source,omitempty"`
}

// Arm captures one strategy's delivered cost and recall for a single task.
type Arm struct {
	Tokens   int      `json:"tokens"`
	Recall   float64  `json:"recall"`
	Files    []string `json:"files"`
	FactsHit []string `json:"facts_hit,omitempty"`
}

// TaskResult is the full A/B outcome for one task.
type TaskResult struct {
	Question string `json:"question"`
	Source   string `json:"source,omitempty"`

	Neurofs   Arm `json:"neurofs"`    // arm B: neurofs_search
	NativeIso Arm `json:"native_iso"` // arm A: whole files to match B's recall

	TokenReduction float64 `json:"token_reduction"` // 1 - B/A, iso-recall
	HasFacts       bool    `json:"has_facts"`       // task carried at least one real fact
	Scored         bool    `json:"scored"`          // false if no facts or B recovered nothing
	Note           string  `json:"note,omitempty"`
}

// Summary rolls the per-task results into headline metrics and a verdict.
type Summary struct {
	Tasks      int `json:"tasks"`
	FactTasks  int `json:"fact_tasks"`  // tasks carrying at least one real fact
	Scored     int `json:"scored"`      // fact tasks B recovered something on (iso-recall subset)
	SearchMiss int `json:"search_miss"` // fact tasks where arm B recovered no facts (recall 0)

	MeanTokensNeurofs int `json:"mean_tokens_neurofs"`
	MeanTokensNative  int `json:"mean_tokens_native"`

	// MeanRecallNeurofs is arm B's recall over the SCORED subset (the iso-recall
	// comparison set). The native arm is compared at this recall, so its recall
	// equals or exceeds it. Reported alongside, never instead of, the honest
	// OverallRecallNeurofs below.
	MeanRecallNeurofs float64 `json:"mean_recall_neurofs"`
	MeanRecallNative  float64 `json:"mean_recall_native"`

	// OverallRecallNeurofs is arm B's recall over ALL fact tasks, counting
	// search misses as 0. This is the honest "how often does it ground at all"
	// number; MeanRecallNeurofs only describes the subset it did ground.
	OverallRecallNeurofs float64 `json:"overall_recall_neurofs"`
	// MissRate is SearchMiss / FactTasks — high values mean the headline token
	// savings only apply to a minority of tasks.
	MissRate float64 `json:"miss_rate"`

	MeanTokenReduction   float64 `json:"mean_token_reduction"`
	MedianTokenReduction float64 `json:"median_token_reduction"`

	Threshold float64 `json:"threshold"`
	Verdict   string  `json:"verdict"` // PASS | FAIL | INSUFFICIENT
	Detail    string  `json:"detail"`
}

// Options tunes the run.
type Options struct {
	// SearchLimit is how many hits arm B keeps per task (default 8).
	SearchLimit int
	// Threshold is the minimum mean token reduction for PASS (default 0.25).
	Threshold float64
}

func (o Options) withDefaults() Options {
	if o.SearchLimit <= 0 {
		o.SearchLimit = DefaultSearchLimit
	}
	if o.Threshold <= 0 {
		o.Threshold = DefaultThreshold
	}
	return o
}

// Run executes the A/B experiment over every task and returns per-task results
// plus the aggregate summary with a verdict.
func Run(ctx context.Context, files []models.FileRecord, tasks []Task, search SearchFn, opts Options) ([]TaskResult, Summary, error) {
	opts = opts.withDefaults()
	absByRel := relToAbs(files)

	results := make([]TaskResult, 0, len(tasks))
	for _, t := range tasks {
		r, err := evalTask(ctx, t, search, absByRel, opts)
		if err != nil {
			return nil, Summary{}, fmt.Errorf("task %q: %w", t.Question, err)
		}
		results = append(results, r)
	}
	return results, summarise(results, opts), nil
}

// evalTask runs both arms for one task.
func evalTask(ctx context.Context, t Task, search SearchFn, absByRel map[string]string, opts Options) (TaskResult, error) {
	res := TaskResult{Question: t.Question, Source: t.Source}

	hits, err := search(ctx, t.Question, opts.SearchLimit)
	if err != nil {
		return res, err
	}

	// Arm B — neurofs_search excerpts.
	var snippetBuf strings.Builder
	seenB := make(map[string]bool)
	for _, h := range hits {
		snippetBuf.WriteString(h.Snippet)
		snippetBuf.WriteByte('\n')
		res.Neurofs.Tokens += h.Tokens
		rel := normPath(h.Path)
		if rel != "" && !seenB[rel] {
			seenB[rel] = true
			res.Neurofs.Files = append(res.Neurofs.Files, rel)
		}
	}
	res.Neurofs.FactsHit, res.Neurofs.Recall = audit.ScoreFacts(snippetBuf.String(), t.ExpectsFacts)

	// Arm A — native whole-file, accumulated in hit order until it matches B's
	// recall (iso-recall). Files are de-duplicated; we stop as soon as recall
	// reaches B's, so the baseline pays only for what it needs to tie.
	target := res.Neurofs.Recall
	var wholeBuf strings.Builder
	seenA := make(map[string]bool)
	for _, h := range hits {
		if res.NativeIso.Recall >= target-1e-9 && len(res.NativeIso.Files) > 0 {
			break
		}
		rel := normPath(h.Path)
		if rel == "" || seenA[rel] {
			continue
		}
		abs, ok := absByRel[rel]
		if !ok {
			continue
		}
		data, readErr := os.ReadFile(abs)
		if readErr != nil {
			continue
		}
		seenA[rel] = true
		res.NativeIso.Files = append(res.NativeIso.Files, rel)
		res.NativeIso.Tokens += tokenbudget.EstimateTokens(string(data))
		wholeBuf.Write(data)
		wholeBuf.WriteByte('\n')
		res.NativeIso.FactsHit, res.NativeIso.Recall = audit.ScoreFacts(wholeBuf.String(), t.ExpectsFacts)
	}

	res.HasFacts = len(validFacts(t.ExpectsFacts)) > 0

	switch {
	case !res.HasFacts:
		res.Note = "no facts to score"
	case res.Neurofs.Recall <= 1e-9:
		res.Note = "neurofs_search recovered no facts (search miss)"
	case res.NativeIso.Tokens == 0:
		res.Note = "no readable hit files for the native baseline"
	default:
		res.Scored = true
		res.TokenReduction = 1 - float64(res.Neurofs.Tokens)/float64(res.NativeIso.Tokens)
	}
	return res, nil
}

func summarise(results []TaskResult, opts Options) Summary {
	s := Summary{Tasks: len(results), Threshold: opts.Threshold}

	var (
		sumTokB, sumTokA int
		sumRecB, sumRecA float64
		reductions       []float64
		overallRecallSum float64 // arm B recall over ALL fact tasks (misses = 0)
	)
	for _, r := range results {
		if r.HasFacts {
			s.FactTasks++
			overallRecallSum += r.Neurofs.Recall
		}
		if !r.Scored {
			if strings.Contains(r.Note, "search miss") {
				s.SearchMiss++
			}
			continue
		}
		s.Scored++
		sumTokB += r.Neurofs.Tokens
		sumTokA += r.NativeIso.Tokens
		sumRecB += r.Neurofs.Recall
		sumRecA += r.NativeIso.Recall
		reductions = append(reductions, r.TokenReduction)
	}

	if s.FactTasks > 0 {
		s.OverallRecallNeurofs = overallRecallSum / float64(s.FactTasks)
		s.MissRate = float64(s.SearchMiss) / float64(s.FactTasks)
	}

	if s.Scored == 0 {
		s.Verdict = "INSUFFICIENT"
		s.Detail = "no scorable tasks (no facts, or neurofs_search recovered nothing)"
		return s
	}

	n := float64(s.Scored)
	s.MeanTokensNeurofs = sumTokB / s.Scored
	s.MeanTokensNative = sumTokA / s.Scored
	s.MeanRecallNeurofs = sumRecB / n
	s.MeanRecallNative = sumRecA / n
	s.MeanTokenReduction = mean(reductions)
	s.MedianTokenReduction = median(reductions)

	// A high miss rate means the headline savings cover only a minority of
	// tasks — the token economy is real but retrieval is leaving facts on the
	// floor. Flag it rather than letting a flattering scored-subset number stand
	// alone. One third is the line: below it, misses are noise; at or above it,
	// they dominate the story.
	missHeavy := s.MissRate >= 1.0/3.0

	switch {
	case s.MeanTokenReduction >= opts.Threshold && !missHeavy:
		s.Verdict = "PASS"
		s.Detail = fmt.Sprintf("mean iso-recall token reduction %.1f%% >= %.0f%% (B %d tok vs native %d tok); overall recall %.0f%% over %d fact tasks, %d miss",
			s.MeanTokenReduction*100, opts.Threshold*100, s.MeanTokensNeurofs, s.MeanTokensNative,
			s.OverallRecallNeurofs*100, s.FactTasks, s.SearchMiss)
	case s.MeanTokenReduction >= opts.Threshold && missHeavy:
		s.Verdict = "WARN"
		s.Detail = fmt.Sprintf("savings hold on the answerable subset (%.1f%% reduction) but %.0f%% of fact tasks are search misses — retrieval recall, not the economy, is the gap",
			s.MeanTokenReduction*100, s.MissRate*100)
	default:
		s.Verdict = "FAIL"
		s.Detail = fmt.Sprintf("mean iso-recall token reduction %.1f%% below threshold %.0f%%",
			s.MeanTokenReduction*100, opts.Threshold*100)
	}
	return s
}

func relToAbs(files []models.FileRecord) map[string]string {
	m := make(map[string]string, len(files))
	for _, f := range files {
		m[normPath(f.RelPath)] = f.Path
	}
	return m
}

func normPath(p string) string {
	return strings.TrimPrefix(filepath.ToSlash(strings.TrimSpace(p)), "./")
}

func validFacts(facts []string) []string {
	out := make([]string, 0, len(facts))
	for _, f := range facts {
		if strings.TrimSpace(f) != "" {
			out = append(out, f)
		}
	}
	return out
}

func mean(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	var sum float64
	for _, x := range xs {
		sum += x
	}
	return sum / float64(len(xs))
}

func median(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	cp := make([]float64, len(xs))
	copy(cp, xs)
	sort.Float64s(cp)
	mid := len(cp) / 2
	if len(cp)%2 == 1 {
		return cp[mid]
	}
	return (cp[mid-1] + cp[mid]) / 2
}
