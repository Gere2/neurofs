// Package learn closes the improve-through-use loop. It consumes the two
// ledgers the MCP surface writes during real work (.neurofs/usage.jsonl,
// .neurofs/feedback.jsonl) and turns them into engine improvements in two
// steps:
//
//   - Promote: feedback entries become G3-style fact fixtures under
//     audit/facts/learned-*.json. A real query that retrieval failed (or
//     partially served) becomes tomorrow's regression oracle — the same
//     files the pivot gate's G3 and the tuner evaluate against.
//
//   - Tune: coordinate descent over the retrieval scoring weights,
//     maximizing mean fact recall on the search surface (ties broken by
//     fewer delivered tokens) across ALL fixtures, hand-written and
//     learned. Winning weights are persisted to .neurofs/weights.json,
//     which every subsequent search loads.
//
// The gate stays the independent guardrail: it scores the bundle surface,
// so a tune that overfits the search surface still has to survive
// `neurofs gate` and `neurofs bench` before it deserves trust.
package learn

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/neuromfs/neuromfs/internal/audit"
	"github.com/neuromfs/neuromfs/internal/ranking"
	"github.com/neuromfs/neuromfs/internal/retrieval"
	"github.com/neuromfs/neuromfs/internal/usage"
)

// Fixture mirrors the gate's G3 fixture shape; the extra fields record
// provenance for learned fixtures and are ignored by the gate's loader.
type Fixture struct {
	Question     string   `json:"question"`
	ExpectsFacts []string `json:"expects_facts"`
	Source       string   `json:"source,omitempty"`
	UsageID      string   `json:"usage_id,omitempty"`
	CreatedAt    string   `json:"created_at,omitempty"`
}

const (
	learnedPrefix  = "learned-"
	maxFactCount   = 6 // G3 guidance: 3-6 facts; more lets one miss tank recall
	minFactLength  = 3
	overfitWarnMin = 10
)

// FactsDir returns where fixtures live for repoRoot.
func FactsDir(repoRoot string) string {
	return filepath.Join(repoRoot, "audit", "facts")
}

// Corpus pairs a repository with the fixture set that evaluates it. Tuning
// against several corpora at once (e.g. this repo plus the G5 cross-shape
// repos) is the structural defence against overfitting: a weight move only
// survives if it helps the macro-average across shapes, not one repo's
// fixture set.
type Corpus struct {
	Repo        string `json:"repo"`
	FixturesDir string `json:"fixtures_dir"`
}

// LoadFixtures reads every fixture under repoRoot's audit/facts/*.json.
func LoadFixtures(repoRoot string) ([]Fixture, error) {
	return LoadFixturesFrom(FactsDir(repoRoot))
}

// LoadFixturesFrom reads every fixture under dir/*.json, sorted by filename
// for reproducible evaluation order.
func LoadFixturesFrom(dir string) ([]Fixture, error) {
	paths, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return nil, err
	}
	sort.Strings(paths)
	var fixtures []Fixture
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			return nil, fmt.Errorf("learn: read fixture %s: %w", p, err)
		}
		var f Fixture
		if err := json.Unmarshal(data, &f); err != nil {
			return nil, fmt.Errorf("learn: parse fixture %s: %w", p, err)
		}
		if strings.TrimSpace(f.Question) == "" || len(f.ExpectsFacts) == 0 {
			continue
		}
		fixtures = append(fixtures, f)
	}
	return fixtures, nil
}

// PromoteResult reports what Promote did.
type PromoteResult struct {
	Created  []string // fixture files written
	Skipped  int      // feedback entries with nothing promotable
	Existing int      // fixtures already on disk (never overwritten)
}

// Promote converts feedback into fixtures. Useful symbols and missing
// identifiers become expected facts — for a "no" rating the missing facts
// are exactly what retrieval failed to deliver, which makes the strictest
// kind of fixture. One fixture per distinct query; existing files are
// never overwritten so hand-tweaked fixtures survive re-promotion.
func Promote(repoRoot string) (PromoteResult, error) {
	feedbacks, err := usage.LoadFeedback(repoRoot)
	if err != nil {
		return PromoteResult{}, err
	}
	var res PromoteResult
	if len(feedbacks) == 0 {
		return res, nil
	}
	if err := os.MkdirAll(FactsDir(repoRoot), 0o755); err != nil {
		return res, fmt.Errorf("learn: mkdir facts: %w", err)
	}

	// The LATEST feedback per query wins, promotable or not: a later entry
	// with no facts is a retraction ("that judgement was wrong / belongs
	// elsewhere") and must suppress the earlier promotable one, not lose to
	// it. Only after picking the survivor do we ask whether it carries
	// promotable, repo-present facts.
	latest := make(map[string]usage.Feedback)
	for _, fb := range feedbacks {
		query := strings.TrimSpace(fb.Query)
		if query == "" {
			res.Skipped++
			continue
		}
		latest[strings.ToLower(query)] = fb
	}

	byID := make(map[string]Fixture, len(latest))
	var ids []string
	for _, fb := range latest {
		query := strings.TrimSpace(fb.Query)
		facts := factsPresentInRepo(repoRoot, collectFacts(fb))
		if len(facts) == 0 {
			res.Skipped++
			continue
		}
		id := queryID(query)
		ids = append(ids, id)
		byID[id] = Fixture{
			Question:     query,
			ExpectsFacts: facts,
			Source:       "feedback",
			UsageID:      fb.UsageID,
			CreatedAt:    time.Now().UTC().Format(time.RFC3339),
		}
	}
	sort.Strings(ids)

	for _, id := range ids {
		path := filepath.Join(FactsDir(repoRoot), learnedPrefix+id+".json")
		if _, err := os.Stat(path); err == nil {
			res.Existing++
			continue
		}
		data, err := json.MarshalIndent(byID[id], "", "  ")
		if err != nil {
			return res, fmt.Errorf("learn: marshal fixture: %w", err)
		}
		if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
			return res, fmt.Errorf("learn: write fixture: %w", err)
		}
		res.Created = append(res.Created, path)
	}
	return res, nil
}

// collectFacts builds the expected-fact list for one feedback entry:
// useful symbols and missing identifiers first (identifier-shaped facts
// survive every representation), path stems only as filler when the
// identifier signal is thin.
func collectFacts(fb usage.Feedback) []string {
	var facts []string
	seen := make(map[string]bool)
	add := func(items []string) {
		for _, item := range items {
			item = strings.TrimSpace(item)
			key := strings.ToLower(item)
			if len(item) < minFactLength || seen[key] || len(facts) >= maxFactCount {
				continue
			}
			seen[key] = true
			facts = append(facts, item)
		}
	}
	add(fb.UsefulSymbols)
	add(fb.MissingFacts)
	if len(facts) < 3 {
		var stems []string
		for _, p := range fb.UsefulPaths {
			base := filepath.Base(strings.TrimSpace(p))
			if ext := filepath.Ext(base); ext != "" {
				base = strings.TrimSuffix(base, ext)
			}
			stems = append(stems, base)
		}
		add(stems)
	}
	return facts
}

func queryID(query string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(query))))
	return hex.EncodeToString(sum[:])[:12]
}

// factsPresentInRepo drops facts that do not occur anywhere in the repo.
// Feedback can name identifiers from a different codebase (an agent working
// across repos files it against the wrong ledger) or misremember a symbol;
// promoting such a fact would mint a fixture with permanent zero recall
// that silently drags every future tune. The fixtures directory itself is
// excluded — a fact must exist outside its own oracle, or every rotten
// fixture would self-certify. Validation uses ripgrep, the same
// dependency retrieval's exact-signal path already relies on; if rg is not
// installed the facts pass through unvalidated rather than blocking
// promotion.
func factsPresentInRepo(repoRoot string, facts []string) []string {
	if len(facts) == 0 {
		return facts
	}
	if _, err := exec.LookPath("rg"); err != nil {
		return facts
	}
	var present []string
	for _, fact := range facts {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		cmd := exec.CommandContext(ctx, "rg", "--fixed-strings", "--ignore-case", "--quiet",
			"--glob", "!.git/**", "--glob", "!.neurofs/**", "--glob", "!audit/facts/**", fact, repoRoot)
		err := cmd.Run()
		cancel()
		if err == nil {
			present = append(present, fact)
		}
	}
	return present
}

// ---------- tuning ----------

// TuneOptions configures a tuning run.
type TuneOptions struct {
	Limit       int // search hits per fixture evaluation (default 8)
	Passes      int // coordinate-descent sweeps (default 2)
	Multipliers []float64
	// FixturesDir overrides where the primary repo's fixtures load from
	// (default <repo>/audit/facts) — the same escape hatch economy has, so
	// external corpora without an audit/ tree can still be tuned against.
	FixturesDir string
	// ExtraCorpora adds more repo+fixture pairs to the objective alongside
	// the primary repo. The objective becomes the macro-average across
	// corpora, so a small extra corpus is not drowned by a large primary.
	ExtraCorpora []Corpus
	Apply        bool
	Logf         func(format string, args ...any)
}

// FixtureScore is one fixture's evaluation under a weight set.
type FixtureScore struct {
	Corpus   string  `json:"corpus,omitempty"`
	Question string  `json:"question"`
	Recall   float64 `json:"recall"`
	Tokens   int     `json:"tokens"`
}

// EvalSummary aggregates an evaluation. For multi-corpus runs MeanRecall
// and MeanTokens are macro-averages (mean of per-corpus means) and
// PerCorpus carries the per-corpus breakdown.
type EvalSummary struct {
	MeanRecall float64         `json:"mean_recall"`
	MeanTokens float64         `json:"mean_tokens"`
	PerFixture []FixtureScore  `json:"per_fixture"`
	PerCorpus  []CorpusSummary `json:"per_corpus,omitempty"`
}

// CorpusSummary is one corpus's slice of a multi-corpus evaluation.
type CorpusSummary struct {
	Repo       string  `json:"repo"`
	Fixtures   int     `json:"fixtures"`
	MeanRecall float64 `json:"mean_recall"`
	MeanTokens float64 `json:"mean_tokens"`
}

// WeightChange records one tuned weight.
type WeightChange struct {
	Name string  `json:"name"`
	From float64 `json:"from"`
	To   float64 `json:"to"`
}

// TuneResult reports a full tuning run.
type TuneResult struct {
	Fixtures int               `json:"fixtures"`
	Baseline EvalSummary       `json:"baseline"`
	Tuned    EvalSummary       `json:"tuned"`
	Weights  retrieval.Weights `json:"weights"`
	Changed  []WeightChange    `json:"changed"`
	Applied  bool              `json:"applied"`
	Warning  string            `json:"warning,omitempty"`
}

type weightParam struct {
	name string
	get  func(*retrieval.Weights) *float64
}

func weightParams() []weightParam {
	return []weightParam{
		{"symbol_match", func(w *retrieval.Weights) *float64 { return &w.SymbolMatch }},
		{"symbol_exact", func(w *retrieval.Weights) *float64 { return &w.SymbolExact }},
		{"path_match", func(w *retrieval.Weights) *float64 { return &w.PathMatch }},
		{"kind_match", func(w *retrieval.Weights) *float64 { return &w.KindMatch }},
		{"content_match", func(w *retrieval.Weights) *float64 { return &w.ContentMatch }},
		{"chunk_scope", func(w *retrieval.Weights) *float64 { return &w.ChunkScope }},
		{"structural_symbol", func(w *retrieval.Weights) *float64 { return &w.StructuralSymbol }},
		{"structural_symbol_partial", func(w *retrieval.Weights) *float64 { return &w.StructuralSymbolPartial }},
		{"structural_import", func(w *retrieval.Weights) *float64 { return &w.StructuralImport }},
		{"semantic", func(w *retrieval.Weights) *float64 { return &w.Semantic }},
		{"working_set", func(w *retrieval.Weights) *float64 { return &w.WorkingSet }},
		{"exact_content", func(w *retrieval.Weights) *float64 { return &w.ExactContent }},
		{"exact_filename", func(w *retrieval.Weights) *float64 { return &w.ExactFilename }},
		{"graph", func(w *retrieval.Weights) *float64 { return &w.Graph }},
		{"long_chunk_penalty_max", func(w *retrieval.Weights) *float64 { return &w.LongChunkPenaltyMax }},
		{"test_downrank", func(w *retrieval.Weights) *float64 { return &w.TestDownrank }},
		{"tiny_chunk_keep", func(w *retrieval.Weights) *float64 { return &w.TinyChunkKeep }},
		{"impl_kind", func(w *retrieval.Weights) *float64 { return &w.ImplKind }},
		{"legacy_path_keep", func(w *retrieval.Weights) *float64 { return &w.LegacyPathKeep }},
	}
}

// zeroWeightProbes are the absolute candidate values tried for a weight
// currently at zero. Multiplicative steps can never move a zero (0×m = 0),
// which would make inert-by-default signals like impl_kind permanently
// invisible to tuning; fixed probes let evidence switch them on.
var zeroWeightProbes = []float64{0.5, 1.5, 4.0}

// candidateValues returns the values coordinate descent tries for a
// parameter: multiples of the current value, or the fixed probes when the
// current value is zero.
func candidateValues(base float64, multipliers []float64) []float64 {
	if base == 0 {
		return zeroWeightProbes
	}
	out := make([]float64, 0, len(multipliers))
	for _, m := range multipliers {
		out = append(out, base*m)
	}
	return out
}

// Tune runs coordinate descent over the retrieval weights against every
// fixture and returns the best weight set found. Objective: higher mean
// recall; at equal recall, fewer delivered tokens (the economy tiebreak).
func Tune(ctx context.Context, repoRoot string, opts TuneOptions) (TuneResult, error) {
	if opts.Limit <= 0 {
		opts.Limit = 8
	}
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

	corpora, totalFixtures, err := loadCorpora(repoRoot, opts.FixturesDir, opts.ExtraCorpora)
	if err != nil {
		return TuneResult{}, err
	}

	current, _, _ := retrieval.LoadWeights(repoRoot)
	start := current
	baseline, err := evaluateCorpora(ctx, corpora, current, opts.Limit)
	if err != nil {
		return TuneResult{}, err
	}
	logf("baseline: recall %.1f%%, %.0f tokens/fixture over %d fixtures in %d corpus(es)",
		baseline.MeanRecall*100, baseline.MeanTokens, totalFixtures, len(corpora))

	best := baseline
	params := weightParams()
	for pass := 1; pass <= opts.Passes; pass++ {
		improvedThisPass := false
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
				summary, err := evaluateCorpora(ctx, corpora, candidate, opts.Limit)
				if err != nil {
					return TuneResult{}, err
				}
				if betterThan(summary, best) {
					best = summary
					bestVal = *param.get(&candidate)
				}
			}
			if bestVal != base {
				*param.get(&current) = bestVal
				current.Clamp()
				improvedThisPass = true
				logf("pass %d: %s %.3g -> %.3g (recall %.1f%%, %.0f tokens)",
					pass, param.name, base, bestVal, best.MeanRecall*100, best.MeanTokens)
			}
		}
		if !improvedThisPass {
			break
		}
	}

	result := TuneResult{
		Fixtures: totalFixtures,
		Baseline: baseline,
		Tuned:    best,
		Weights:  current,
		Changed:  diffWeights(start, current),
	}
	if totalFixtures < overfitWarnMin {
		result.Warning = fmt.Sprintf(
			"only %d fixtures — tuned weights are likely overfit; keep using the tool and re-run after `neurofs learn promote` grows the set past %d",
			totalFixtures, overfitWarnMin)
	} else if len(corpora) == 1 {
		result.Warning = "single-corpus tune — weights may overfit this repo's shape; add --corpus <repo>:<fixtures-dir> pairs (e.g. the G5 repos) to tune for generalization"
	}
	if opts.Apply {
		if err := retrieval.SaveWeights(repoRoot, current); err != nil {
			return result, err
		}
		result.Applied = true
	}
	return result, nil
}

// Evaluate scores a weight set against the current fixtures without
// changing anything — the read-only entry point for `learn eval` and CI.
// fixturesDir overrides the primary repo's fixture location; empty means
// <repo>/audit/facts.
func Evaluate(ctx context.Context, repoRoot, fixturesDir string, w retrieval.Weights, limit int, extra []Corpus) (EvalSummary, error) {
	if limit <= 0 {
		limit = 8
	}
	corpora, _, err := loadCorpora(repoRoot, fixturesDir, extra)
	if err != nil {
		return EvalSummary{}, err
	}
	return evaluateCorpora(ctx, corpora, w, limit)
}

type corpusFixtures struct {
	repo     string
	fixtures []Fixture
	// session is lazily created on first evaluation and reused across every
	// candidate weight set — loading the index once per corpus instead of
	// once per search is what makes a ~1700-evaluation tune tractable.
	session *retrieval.Session
}

func (c *corpusFixtures) ensureSession(ctx context.Context) (*retrieval.Session, error) {
	if c.session == nil {
		session, err := retrieval.NewSession(ctx, c.repo)
		if err != nil {
			return nil, err
		}
		c.session = session
	}
	return c.session, nil
}

// loadCorpora resolves the primary repo plus any extra corpora into loaded
// fixture sets, failing on an empty corpus — an empty fixture set would
// silently drop out of the macro-average and skew the objective.
func loadCorpora(repoRoot, primaryFixturesDir string, extra []Corpus) ([]*corpusFixtures, int, error) {
	if strings.TrimSpace(primaryFixturesDir) == "" {
		primaryFixturesDir = FactsDir(repoRoot)
	}
	all := append([]Corpus{{Repo: repoRoot, FixturesDir: primaryFixturesDir}}, extra...)
	var corpora []*corpusFixtures
	total := 0
	for _, c := range all {
		dir := c.FixturesDir
		if strings.TrimSpace(dir) == "" {
			dir = FactsDir(c.Repo)
		}
		fixtures, err := LoadFixturesFrom(dir)
		if err != nil {
			return nil, 0, err
		}
		if len(fixtures) == 0 {
			return nil, 0, fmt.Errorf("learn: no fixtures under %s — promote feedback or add hand-written ones first", dir)
		}
		corpora = append(corpora, &corpusFixtures{repo: c.Repo, fixtures: fixtures})
		total += len(fixtures)
	}
	return corpora, total, nil
}

// evaluateCorpora scores a weight set as the macro-average of per-corpus
// evaluations, so each repository shape carries equal weight in the
// objective regardless of how many fixtures it contributes.
func evaluateCorpora(ctx context.Context, corpora []*corpusFixtures, w retrieval.Weights, limit int) (EvalSummary, error) {
	if len(corpora) == 1 {
		return evaluate(ctx, corpora[0], w, limit)
	}
	var macro EvalSummary
	for _, c := range corpora {
		summary, err := evaluate(ctx, c, w, limit)
		if err != nil {
			return EvalSummary{}, err
		}
		for i := range summary.PerFixture {
			summary.PerFixture[i].Corpus = c.repo
		}
		macro.PerFixture = append(macro.PerFixture, summary.PerFixture...)
		macro.PerCorpus = append(macro.PerCorpus, CorpusSummary{
			Repo:       c.repo,
			Fixtures:   len(c.fixtures),
			MeanRecall: summary.MeanRecall,
			MeanTokens: summary.MeanTokens,
		})
		macro.MeanRecall += summary.MeanRecall
		macro.MeanTokens += summary.MeanTokens
	}
	n := float64(len(corpora))
	macro.MeanRecall /= n
	macro.MeanTokens /= n
	return macro, nil
}

// evaluate runs the search surface for each fixture question and scores
// fact recall over the delivered snippets — the same scorer (audit.
// ScoreFacts) and surface the Phase-0 economy harness measured, so tuning
// optimizes exactly the number the pivot decision was made on.
func evaluate(ctx context.Context, c *corpusFixtures, w retrieval.Weights, limit int) (EvalSummary, error) {
	session, err := c.ensureSession(ctx)
	if err != nil {
		return EvalSummary{}, err
	}
	var summary EvalSummary
	for _, fixture := range c.fixtures {
		response, err := session.Search(ctx, retrieval.Options{
			Query:              fixture.Question,
			Limit:              limit,
			Weights:            &w,
			NeutralizeGitState: true,
		})
		if err != nil {
			return EvalSummary{}, fmt.Errorf("learn: search %q: %w", fixture.Question, err)
		}
		var sb strings.Builder
		tokens := 0
		for _, hit := range response.Results {
			fmt.Fprintf(&sb, "%s:%d-%d %s\n%s\n", hit.Path, hit.StartLine, hit.EndLine, hit.Symbol, hit.Snippet)
			tokens += hit.TokenEstimate
		}
		_, recall := audit.ScoreFacts(sb.String(), fixture.ExpectsFacts)
		summary.PerFixture = append(summary.PerFixture, FixtureScore{
			Question: fixture.Question,
			Recall:   recall,
			Tokens:   tokens,
		})
		summary.MeanRecall += recall
		summary.MeanTokens += float64(tokens)
	}
	n := float64(len(c.fixtures))
	summary.MeanRecall /= n
	summary.MeanTokens /= n
	return summary, nil
}

// betterThan implements the tuning objective: recall first, then economy.
func betterThan(a, b EvalSummary) bool {
	const eps = 1e-9
	if a.MeanRecall > b.MeanRecall+eps {
		return true
	}
	if a.MeanRecall < b.MeanRecall-eps {
		return false
	}
	return a.MeanTokens < b.MeanTokens-eps
}

func diffWeights(from, to retrieval.Weights) []WeightChange {
	var changes []WeightChange
	for _, param := range weightParams() {
		f := *param.get(&from)
		t := *param.get(&to)
		if f != t {
			changes = append(changes, WeightChange{Name: param.name, From: f, To: t})
		}
	}
	return changes
}

// ---------- status ----------

// StatusResult summarizes the learn loop's accumulated signal.
type StatusResult struct {
	UsageCount      int            `json:"usage_count"`
	FeedbackCount   int            `json:"feedback_count"`
	HandFixtures    int            `json:"hand_fixtures"`
	LearnedFixtures int            `json:"learned_fixtures"`
	WeightsPath     string         `json:"weights_path"`
	WeightsCustom   bool           `json:"weights_custom"`
	WeightsError    string         `json:"weights_error,omitempty"`
	Changed         []WeightChange `json:"changed,omitempty"`
	// File-ranker weights (the bundle surface) — the second tunable set.
	RankingWeightsPath   string             `json:"ranking_weights_path"`
	RankingWeightsCustom bool               `json:"ranking_weights_custom"`
	RankingWeightsError  string             `json:"ranking_weights_error,omitempty"`
	RankingChanged       []FileWeightChange `json:"ranking_changed,omitempty"`
}

// Status reports ledger sizes, fixture counts, and how the active weights
// differ from defaults — including a weights.json parse error that Search
// silently falls back from.
func Status(repoRoot string) (StatusResult, error) {
	entries, err := usage.Load(repoRoot)
	if err != nil {
		return StatusResult{}, err
	}
	feedbacks, err := usage.LoadFeedback(repoRoot)
	if err != nil {
		return StatusResult{}, err
	}
	res := StatusResult{
		UsageCount:    len(entries),
		FeedbackCount: len(feedbacks),
		WeightsPath:   retrieval.WeightsPath(repoRoot),
	}

	paths, err := filepath.Glob(filepath.Join(FactsDir(repoRoot), "*.json"))
	if err != nil {
		return res, err
	}
	for _, p := range paths {
		if strings.HasPrefix(filepath.Base(p), learnedPrefix) {
			res.LearnedFixtures++
		} else {
			res.HandFixtures++
		}
	}

	w, existed, werr := retrieval.LoadWeights(repoRoot)
	res.WeightsCustom = existed
	if werr != nil {
		res.WeightsError = werr.Error()
	}
	if existed && werr == nil {
		res.Changed = diffWeights(retrieval.DefaultWeights(), w)
	}

	rw, rexisted, rerr := ranking.LoadWeights(repoRoot)
	res.RankingWeightsPath = ranking.WeightsPath(repoRoot)
	res.RankingWeightsCustom = rexisted
	if rerr != nil {
		res.RankingWeightsError = rerr.Error()
	}
	if rexisted && rerr == nil {
		res.RankingChanged = diffFileWeights(ranking.DefaultWeights(), rw)
	}
	return res, nil
}
