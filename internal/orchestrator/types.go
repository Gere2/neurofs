// Package orchestrator decomposes a high-level question into sub-tasks,
// routes each to the most cost-effective model, dispatches them respecting
// dependency order, and verifies grounding of every result.
//
// The package is the core of NeuroFS's orchestration layer. It sits above
// the retrieval engine (internal/retrieval) and the context broker
// (internal/mcp) and below the UI and CLI surfaces.
package orchestrator

import "time"

// TaskKind classifies a sub-task so the router can pick a model.
type TaskKind string

const (
	KindFrontend TaskKind = "frontend"
	KindBackend  TaskKind = "backend"
	KindDatabase TaskKind = "database"
	KindTest     TaskKind = "test"
	KindDesign   TaskKind = "design"
	KindConfig   TaskKind = "config"
	KindDocs     TaskKind = "docs"
	KindPlanning TaskKind = "planning"
	KindGeneral  TaskKind = "general"
)

// Complexity guides model selection: simple tasks use cheap models,
// complex tasks use powerful ones.
type Complexity string

const (
	Simple  Complexity = "simple"
	Medium  Complexity = "medium"
	Complex Complexity = "complex"
)

// TaskStatus tracks the lifecycle of a sub-task through orchestration.
type TaskStatus string

const (
	StatusPending  TaskStatus = "pending"
	StatusRunning  TaskStatus = "running"
	StatusDone     TaskStatus = "done"
	StatusFailed   TaskStatus = "failed"
	StatusSkipped  TaskStatus = "skipped"
)

// Task is one decomposed sub-task within a Plan.
type Task struct {
	ID          string     `json:"id"`
	Description string     `json:"description"`
	Kind        TaskKind   `json:"kind"`
	Complexity  Complexity `json:"complexity"`
	DependsOn   []string   `json:"depends_on,omitempty"`

	// Set by the router
	Model       string     `json:"model,omitempty"`
	Provider    string     `json:"provider,omitempty"`

	// Set by the dispatcher
	Status      TaskStatus `json:"status"`
	Context     string     `json:"context,omitempty"`      // NeuroFS retrieval context
	Prompt      string     `json:"prompt,omitempty"`       // full prompt sent to the model
	Response    string     `json:"response,omitempty"`     // raw model response
	Error       string     `json:"error,omitempty"`

	// Metrics
	StartedAt   *time.Time `json:"started_at,omitempty"`
	FinishedAt  *time.Time `json:"finished_at,omitempty"`
	InputTokens  int       `json:"input_tokens,omitempty"`
	OutputTokens int       `json:"output_tokens,omitempty"`
	CostUSD      float64   `json:"cost_usd,omitempty"`
	Grounding    float64   `json:"grounding,omitempty"`
}

// Duration returns how long the task took, or zero if not finished.
func (t Task) Duration() time.Duration {
	if t.StartedAt == nil || t.FinishedAt == nil {
		return 0
	}
	return t.FinishedAt.Sub(*t.StartedAt)
}

// Plan is the decomposed execution plan for a user question.
type Plan struct {
	ID        string    `json:"id"`
	Question  string    `json:"question"`
	Repo      string    `json:"repo"`
	Tasks     []Task    `json:"tasks"`
	CreatedAt time.Time `json:"created_at"`

	// Set after execution
	Status    TaskStatus `json:"status"`
}

// TotalCost sums the cost of all tasks.
func (p Plan) TotalCost() float64 {
	var total float64
	for _, t := range p.Tasks {
		total += t.CostUSD
	}
	return total
}

// TotalDuration returns the wall-clock time from first start to last finish.
func (p Plan) TotalDuration() time.Duration {
	var earliest, latest time.Time
	for _, t := range p.Tasks {
		if t.StartedAt != nil && (earliest.IsZero() || t.StartedAt.Before(earliest)) {
			earliest = *t.StartedAt
		}
		if t.FinishedAt != nil && (latest.IsZero() || t.FinishedAt.After(latest)) {
			latest = *t.FinishedAt
		}
	}
	if earliest.IsZero() || latest.IsZero() {
		return 0
	}
	return latest.Sub(earliest)
}

// MeanGrounding returns the average grounding score across completed tasks.
func (p Plan) MeanGrounding() float64 {
	var sum float64
	var n int
	for _, t := range p.Tasks {
		if t.Status == StatusDone && t.Grounding > 0 {
			sum += t.Grounding
			n++
		}
	}
	if n == 0 {
		return 0
	}
	return sum / float64(n)
}

// Result is the final output of an orchestration run.
type Result struct {
	Plan           Plan    `json:"plan"`
	TotalCostUSD   float64 `json:"total_cost_usd"`
	MeanGrounding  float64 `json:"mean_grounding"`
	DurationMs     int64   `json:"duration_ms"`
}

// StatusEvent is sent over SSE to update the UI in real time.
type StatusEvent struct {
	TaskID       string     `json:"task_id"`
	Status       TaskStatus `json:"status"`
	Model        string     `json:"model,omitempty"`
	Provider     string     `json:"provider,omitempty"`
	InputTokens  int        `json:"input_tokens,omitempty"`
	OutputTokens int        `json:"output_tokens,omitempty"`
	CostUSD      float64    `json:"cost_usd,omitempty"`
	Grounding    float64    `json:"grounding,omitempty"`
	DurationMs   int64      `json:"duration_ms,omitempty"`
	Response     string     `json:"response,omitempty"`
	Error        string     `json:"error,omitempty"`
}
