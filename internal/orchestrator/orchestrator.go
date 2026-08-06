package orchestrator

import (
	"context"
	"fmt"
	"time"

	"github.com/Gere2/neurofs/internal/audit"
	"github.com/Gere2/neurofs/internal/retrieval"
)

// OrchestrationOptions configures a full orchestration run.
type OrchestrationOptions struct {
	Question string
	RepoRoot string
	Callback ProgressCallback
}

// Orchestrator coordinates decomposition, context retrieval, routing, dispatching, and grounding.
type Orchestrator struct {
	Config     ModelsConfig
	Router     *Router
	Decomposer *Decomposer
	Dispatcher *Dispatcher
}

// NewOrchestrator initializes an orchestrator instance using models.json or defaults.
func NewOrchestrator(repoRoot string, client LLMClient) (*Orchestrator, error) {
	cfg, err := LoadModelsConfig(repoRoot)
	if err != nil {
		cfg = DefaultModelsConfig()
	}

	router := NewRouter(cfg)
	decomposer := NewDecomposer(router, client)
	dispatcher := NewDispatcher(router, client)
	dispatcher.RepoRoot = repoRoot

	if cache, err := NewSemanticCache("", 24*time.Hour); err == nil {
		dispatcher.Cache = cache
	}

	return &Orchestrator{
		Config:     cfg,
		Router:     router,
		Decomposer: decomposer,
		Dispatcher: dispatcher,
	}, nil
}

// Run executes the complete orchestration flow:
// 1. Initial repository context retrieval
// 2. Task decomposition
// 3. Per-task context retrieval via NeuroFS chunk search
// 4. Model routing & dispatched completion
// 5. Grounding verification
func (o *Orchestrator) Run(ctx context.Context, opts OrchestrationOptions) (Result, error) {
	start := time.Now()

	if opts.RepoRoot == "" {
		opts.RepoRoot = "."
	}
	// Run's root wins over the one NewOrchestrator was built with, so
	// grounding inside the dispatcher verifies against the same tree as the
	// re-scoring below.
	o.Dispatcher.RepoRoot = opts.RepoRoot

	// Step 1: Initial context query to orient decomposition
	initialSearch, _ := retrieval.Search(ctx, retrieval.Options{
		Query: opts.Question,
		Repo:  opts.RepoRoot,
		Limit: 5,
	})

	repoCtxSummary := fmt.Sprintf("Initial search returned %d hits for query: %s", len(initialSearch.Results), opts.Question)

	// Step 2: Decompose question into plan
	plan, err := o.Decomposer.Decompose(ctx, opts.Question, opts.RepoRoot, repoCtxSummary)
	if err != nil {
		return Result{}, fmt.Errorf("decomposition failed: %w", err)
	}

	// Step 3: Populate per-task retrieval context
	for i := range plan.Tasks {
		t := &plan.Tasks[i]
		searchResp, searchErr := retrieval.Search(ctx, retrieval.Options{
			Query: t.Description,
			Repo:  opts.RepoRoot,
			Limit: 4,
		})
		if searchErr == nil && len(searchResp.Results) > 0 {
			var snippets []string
			for _, hit := range searchResp.Results {
				snippets = append(snippets, fmt.Sprintf("File %s (%d-%d):\n%s", hit.Path, hit.StartLine, hit.EndLine, hit.Snippet))
			}
			t.Context = fmt.Sprintf("Relevant codebase snippets:\n\n%s", truncate(joinSnippets(snippets), 3000))
		}
	}

	// Step 4: Dispatch plan with real-time callback
	if err := o.Dispatcher.Dispatch(ctx, &plan, func(ev StatusEvent) {
		// Calculate grounding on completion if response present
		if ev.Status == StatusDone && ev.Response != "" {
			var taskCtx string
			for _, t := range plan.Tasks {
				if t.ID == ev.TaskID {
					taskCtx = t.Context
					break
				}
			}
			if taskCtx != "" {
				ev.Grounding = calculateGroundingScoreIn(ev.Response, taskCtx, opts.RepoRoot)
			}
		}
		if opts.Callback != nil {
			opts.Callback(ev)
		}
	}); err != nil {
		return Result{}, fmt.Errorf("dispatch failed: %w", err)
	}

	// Update task grounding scores in plan
	for i := range plan.Tasks {
		t := &plan.Tasks[i]
		if t.Status == StatusDone && t.Response != "" && t.Context != "" {
			t.Grounding = calculateGroundingScoreIn(t.Response, t.Context, opts.RepoRoot)
		}
	}

	duration := time.Since(start)
	result := Result{
		Plan:          plan,
		TotalCostUSD:  plan.TotalCost(),
		MeanGrounding: plan.MeanGrounding(),
		DurationMs:    duration.Milliseconds(),
	}

	// Aggregate cascade and cache metrics
	for _, t := range plan.Tasks {
		if len(t.CascadeAttempts) > 1 {
			result.CascadeEscalations += len(t.CascadeAttempts) - 1
		}
		result.CascadeSavedUSD += t.CascadeSaved
		if t.Cached {
			result.CacheHits++
		}
	}

	return result, nil

}

// calculateGroundingScoreIn verifies the response against contextStr with
// citations resolved inside repoRoot.
func calculateGroundingScoreIn(response, contextStr, repoRoot string) float64 {
	if response == "" || contextStr == "" {
		return 0.0
	}
	if repoRoot == "" {
		repoRoot = "."
	}
	report := audit.VerifyResponse(context.Background(), response, contextStr, repoRoot, false)
	return report.Score
}

func joinSnippets(snippets []string) string {
	res := ""
	for i, s := range snippets {
		if i > 0 {
			res += "\n---\n"
		}
		res += s
	}
	return res
}
