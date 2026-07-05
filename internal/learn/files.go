package learn

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/neuromfs/neuromfs/internal/benchmark"
	"github.com/neuromfs/neuromfs/internal/config"
	"github.com/neuromfs/neuromfs/internal/models"
	"github.com/neuromfs/neuromfs/internal/ranking"
	"github.com/neuromfs/neuromfs/internal/storage"
	"github.com/neuromfs/neuromfs/internal/taskflow"
)

// The file-level ranker (internal/ranking) shapes the bundle path — a
// different surface than chunk search, with its own weights. TuneFiles
// mirrors Tune for that surface: coordinate descent over ranking.Weights,
// scored by top-3 precision on committed (question → expected files)
// benchmarks, macro-averaged across corpora so one repo's shape cannot
// overfit the rest.

// BenchCorpus pairs a repository with a benchmark question file.
type BenchCorpus struct {
	Repo      string `json:"repo"`
	BenchPath string `json:"bench_path"`
}

// TuneFilesOptions configures a file-ranker tuning run.
type TuneFilesOptions struct {
	Passes      int
	Multipliers []float64
	// BenchPath overrides the primary repo's benchmark file
	// (default <repo>/.neurofs-bench.json).
	BenchPath    string
	ExtraCorpora []BenchCorpus
	Apply        bool
	Logf         func(format string, args ...any)
}

// FilesEvalSummary aggregates one evaluation across bench corpora.
type FilesEvalSummary struct {
	Top3      float64            `json:"top3"`      // macro-average, percent
	HitRate   float64            `json:"hit_rate"`  // macro-average, fraction
	MeanRank  float64            `json:"mean_rank"` // macro-average over hits
	PerCorpus []FilesCorpusScore `json:"per_corpus,omitempty"`
}

// FilesCorpusScore is one corpus's slice of a file-ranker evaluation.
type FilesCorpusScore struct {
	Repo      string  `json:"repo"`
	Questions int     `json:"questions"`
	Top3      float64 `json:"top3"`
	HitRate   float64 `json:"hit_rate"`
	MeanRank  float64 `json:"mean_rank"`
}

// TuneFilesResult reports a full file-ranker tuning run.
type TuneFilesResult struct {
	Questions int                 `json:"questions"`
	Baseline  FilesEvalSummary    `json:"baseline"`
	Tuned     FilesEvalSummary    `json:"tuned"`
	Weights   ranking.Weights     `json:"weights"`
	Changed   []FileWeightChange  `json:"changed"`
	Applied   bool                `json:"applied"`
	Warning   string              `json:"warning,omitempty"`
}

// FileWeightChange records one tuned file-ranker weight.
type FileWeightChange struct {
	Name string  `json:"name"`
	From float64 `json:"from"`
	To   float64 `json:"to"`
}

type fileWeightParam struct {
	name string
	get  func(*ranking.Weights) *float64
}

func fileWeightParams() []fileWeightParam {
	return []fileWeightParam{
		{"filename", func(w *ranking.Weights) *float64 { return &w.Filename }},
		{"path", func(w *ranking.Weights) *float64 { return &w.Path }},
		{"symbol", func(w *ranking.Weights) *float64 { return &w.Symbol }},
		{"import", func(w *ranking.Weights) *float64 { return &w.Import }},
		{"import_expansion", func(w *ranking.Weights) *float64 { return &w.ImportExpansion }},
		{"lang_bonus", func(w *ranking.Weights) *float64 { return &w.LangBonus }},
		{"content_match", func(w *ranking.Weights) *float64 { return &w.ContentMatch }},
		{"entry_point", func(w *ranking.Weights) *float64 { return &w.EntryPoint }},
		{"dependency_match", func(w *ranking.Weights) *float64 { return &w.DependencyMatch }},
		{"focus", func(w *ranking.Weights) *float64 { return &w.Focus }},
		{"changed", func(w *ranking.Weights) *float64 { return &w.Changed }},
		{"semantic", func(w *ranking.Weights) *float64 { return &w.Semantic }},
		{"root_doc", func(w *ranking.Weights) *float64 { return &w.RootDoc }},
	}
}

// loadedBenchCorpus holds one corpus's loaded state, reused across every
// candidate evaluation (the file ranker is pure in-memory scoring, so a
// full multi-corpus evaluation runs in milliseconds once loaded).
type loadedBenchCorpus struct {
	repo      string
	questions []benchmark.Question
	files     []models.FileRecord
	opts      benchmark.RunOptions
}

func loadBenchCorpora(repoRoot, primaryBench string, extra []BenchCorpus) ([]*loadedBenchCorpus, int, error) {
	if strings.TrimSpace(primaryBench) == "" {
		primaryBench = repoRoot + "/.neurofs-bench.json"
	}
	all := append([]BenchCorpus{{Repo: repoRoot, BenchPath: primaryBench}}, extra...)

	var corpora []*loadedBenchCorpus
	total := 0
	for _, c := range all {
		questions, err := benchmark.LoadQuestions(c.BenchPath)
		if err != nil {
			return nil, 0, fmt.Errorf("learn: bench corpus %s: %w", c.Repo, err)
		}
		if len(questions) == 0 {
			return nil, 0, fmt.Errorf("learn: bench corpus %s: no questions in %s", c.Repo, c.BenchPath)
		}
		cfg, err := config.New(c.Repo)
		if err != nil {
			return nil, 0, err
		}
		db, err := storage.Open(cfg.DBPath)
		if err != nil {
			return nil, 0, fmt.Errorf("learn: open index for %s (run 'neurofs scan' there first?): %w", c.Repo, err)
		}
		files, err := db.AllFiles()
		if err != nil {
			db.Close()
			return nil, 0, err
		}
		relations, _ := db.AllRelations()
		info := taskflow.LoadProjectInfo(db)
		db.Close()
		if len(files) == 0 {
			return nil, 0, fmt.Errorf("learn: empty index for %s — run 'neurofs scan' there first", c.Repo)
		}
		corpora = append(corpora, &loadedBenchCorpus{
			repo:      c.Repo,
			questions: questions,
			files:     files,
			opts: benchmark.RunOptions{
				TopK:      3,
				Project:   info,
				Relations: relations,
			},
		})
		total += len(questions)
	}
	return corpora, total, nil
}

func evaluateFiles(corpora []*loadedBenchCorpus, w ranking.Weights) FilesEvalSummary {
	var macro FilesEvalSummary
	for _, c := range corpora {
		opts := c.opts
		opts.Weights = &w
		_, summary := benchmark.Run(c.files, c.questions, opts)
		hitRate := 0.0
		if summary.Questions > 0 {
			hitRate = float64(summary.Hits) / float64(summary.Questions)
		}
		macro.PerCorpus = append(macro.PerCorpus, FilesCorpusScore{
			Repo:      c.repo,
			Questions: summary.Questions,
			Top3:      summary.Top3,
			HitRate:   hitRate,
			MeanRank:  summary.MeanRank,
		})
		macro.Top3 += summary.Top3
		macro.HitRate += hitRate
		macro.MeanRank += summary.MeanRank
	}
	n := float64(len(corpora))
	macro.Top3 /= n
	macro.HitRate /= n
	macro.MeanRank /= n
	return macro
}

// filesBetterThan: top-3 precision first, then overall hit rate, then mean
// rank (lower is better).
func filesBetterThan(a, b FilesEvalSummary) bool {
	const eps = 1e-9
	if a.Top3 > b.Top3+eps {
		return true
	}
	if a.Top3 < b.Top3-eps {
		return false
	}
	if a.HitRate > b.HitRate+eps {
		return true
	}
	if a.HitRate < b.HitRate-eps {
		return false
	}
	return a.MeanRank < b.MeanRank-eps
}

// TuneFiles runs coordinate descent over the file-ranker weights against
// every bench corpus and returns the best weight set found.
func TuneFiles(ctx context.Context, repoRoot string, opts TuneFilesOptions) (TuneFilesResult, error) {
	if opts.Passes <= 0 {
		opts.Passes = 2
	}
	if len(opts.Multipliers) == 0 {
		opts.Multipliers = []float64{0.6, 0.8, 1.25, 1.5}
	}
	logf := opts.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}

	corpora, totalQuestions, err := loadBenchCorpora(repoRoot, opts.BenchPath, opts.ExtraCorpora)
	if err != nil {
		return TuneFilesResult{}, err
	}

	current, _, _ := ranking.LoadWeights(repoRoot)
	start := current
	baseline := evaluateFiles(corpora, current)
	logf("baseline: top-3 %.1f%%, hit rate %.0f%%, mean rank %.2f over %d questions in %d corpus(es)",
		baseline.Top3, baseline.HitRate*100, baseline.MeanRank, totalQuestions, len(corpora))

	best := baseline
	params := fileWeightParams()
	for pass := 1; pass <= opts.Passes; pass++ {
		improved := false
		for _, param := range params {
			base := *param.get(&current)
			bestVal := base
			for _, value := range candidateValues(base, opts.Multipliers) {
				candidate := current
				*param.get(&candidate) = value
				candidate.Clamp()
				if candidate == current {
					continue
				}
				if ctx.Err() != nil {
					return TuneFilesResult{}, ctx.Err()
				}
				summary := evaluateFiles(corpora, candidate)
				if filesBetterThan(summary, best) {
					best = summary
					bestVal = *param.get(&candidate)
				}
			}
			if bestVal != base {
				*param.get(&current) = bestVal
				current.Clamp()
				improved = true
				logf("pass %d: %s %.3g -> %.3g (top-3 %.1f%%, hit rate %.0f%%)",
					pass, param.name, base, bestVal, best.Top3, best.HitRate*100)
			}
		}
		if !improved {
			break
		}
	}

	result := TuneFilesResult{
		Questions: totalQuestions,
		Baseline:  baseline,
		Tuned:     best,
		Weights:   current,
		Changed:   diffFileWeights(start, current),
	}
	if totalQuestions < overfitWarnMin {
		result.Warning = fmt.Sprintf(
			"only %d bench questions — tuned weights are likely overfit; grow the bench sets first", totalQuestions)
	} else if len(corpora) == 1 {
		result.Warning = "single-corpus tune — add --bench <repo>:<bench.json> pairs so the objective spans repository shapes"
	}
	if opts.Apply {
		if err := ranking.SaveWeights(repoRoot, current); err != nil {
			return result, err
		}
		result.Applied = true
	}
	return result, nil
}

func diffFileWeights(from, to ranking.Weights) []FileWeightChange {
	var changes []FileWeightChange
	for _, param := range fileWeightParams() {
		f := *param.get(&from)
		t := *param.get(&to)
		if f != t {
			changes = append(changes, FileWeightChange{Name: param.name, From: f, To: t})
		}
	}
	sort.Slice(changes, func(i, j int) bool { return changes[i].Name < changes[j].Name })
	return changes
}
