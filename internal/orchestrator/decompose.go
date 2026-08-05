package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Decomposer splits high-level questions into structured sub-task plans.
type Decomposer struct {
	Router    *Router
	LLMClient LLMClient
}

// NewDecomposer creates a new Decomposer instance.
func NewDecomposer(router *Router, client LLMClient) *Decomposer {
	if client == nil {
		client = NewDefaultLLMClient(30 * time.Second)
	}
	return &Decomposer{
		Router:    router,
		LLMClient: client,
	}
}

type decomposedTaskJSON struct {
	ID          string   `json:"id"`
	Description string   `json:"description"`
	Kind        string   `json:"kind"`
	Complexity  string   `json:"complexity"`
	DependsOn   []string `json:"depends_on,omitempty"`
}

// Decompose creates a structured execution plan for a question using LLM or rule-based fallback.
func (d *Decomposer) Decompose(ctx context.Context, question, repoRoot, repoContext string) (Plan, error) {
	question = strings.TrimSpace(question)
	if question == "" {
		return Plan{}, fmt.Errorf("question cannot be empty")
	}

	planID := fmt.Sprintf("plan-%d", time.Now().UnixNano())
	plan := Plan{
		ID:        planID,
		Question:  question,
		Repo:      repoRoot,
		CreatedAt: time.Now(),
		Status:    StatusPending,
	}

	// Determine planner model entry
	plannerTask := Task{Kind: KindPlanning, Complexity: Simple}
	resolved, err := d.Router.SelectModel(plannerTask)
	if err != nil {
		resolved = ResolvedModel{
			Name:  "gemini-flash",
			Entry: d.Router.Config.Models["gemini-flash"],
		}
	}

	prompt := fmt.Sprintf(`You are a software architecture assistant. Decompose the following task into a list of logical sub-tasks for an engineering team or agents.

Question/Task:
%s

Repository Context:
%s

Respond ONLY with a valid JSON array of objects with the following schema:
[
  {
    "id": "t1",
    "description": "Short description of action",
    "kind": "one of [frontend, backend, database, test, design, config, docs, general]",
    "complexity": "one of [simple, medium, complex]",
    "depends_on": []
  }
]
Do not include markdown formatting or prose around the JSON.`, question, truncate(repoContext, 2000))

	resp, _, _, err := d.LLMClient.Complete(ctx, resolved.Entry, prompt)
	if err != nil || isMockResponse(resp) {
		// Fallback to rule-based decomposition when LLM is unavailable or mock
		plan.Tasks = RuleBasedDecompose(question)
		return plan, nil
	}

	// Clean code blocks if present
	jsonStr := extractJSONString(resp)
	var rawTasks []decomposedTaskJSON
	if err := json.Unmarshal([]byte(jsonStr), &rawTasks); err != nil || len(rawTasks) == 0 {
		plan.Tasks = RuleBasedDecompose(question)
		return plan, nil
	}

	for i, rt := range rawTasks {
		id := rt.ID
		if id == "" {
			id = fmt.Sprintf("t%d", i+1)
		}
		kind := parseTaskKind(rt.Kind)
		complexity := parseComplexity(rt.Complexity)

		plan.Tasks = append(plan.Tasks, Task{
			ID:          id,
			Description: rt.Description,
			Kind:        kind,
			Complexity:  complexity,
			DependsOn:   rt.DependsOn,
			Status:      StatusPending,
		})
	}

	return plan, nil
}

// RuleBasedDecompose provides a deterministic fallback task decomposition.
func RuleBasedDecompose(question string) []Task {
	q := strings.ToLower(question)
	var tasks []Task

	if strings.Contains(q, "auth") || strings.Contains(q, "login") || strings.Contains(q, "user") {
		tasks = append(tasks, Task{
			ID:          "t1",
			Description: "Design database schema & user authentication model",
			Kind:        KindDatabase,
			Complexity:  Medium,
		})
		tasks = append(tasks, Task{
			ID:          "t2",
			Description: "Implement backend authentication routes and middleware",
			Kind:        KindBackend,
			Complexity:  Medium,
			DependsOn:   []string{"t1"},
		})
		tasks = append(tasks, Task{
			ID:          "t3",
			Description: "Build frontend login & registration components",
			Kind:        KindFrontend,
			Complexity:  Simple,
			DependsOn:   []string{"t2"},
		})
		tasks = append(tasks, Task{
			ID:          "t4",
			Description: "Write integration tests for authentication flow",
			Kind:        KindTest,
			Complexity:  Simple,
			DependsOn:   []string{"t2", "t3"},
		})
		return tasks
	}

	if strings.Contains(q, "search") || strings.Contains(q, "find") || strings.Contains(q, "index") {
		tasks = append(tasks, Task{
			ID:          "t1",
			Description: "Define search index structures and storage queries",
			Kind:        KindDatabase,
			Complexity:  Simple,
		})
		tasks = append(tasks, Task{
			ID:          "t2",
			Description: "Implement search API endpoint and query logic",
			Kind:        KindBackend,
			Complexity:  Medium,
			DependsOn:   []string{"t1"},
		})
		tasks = append(tasks, Task{
			ID:          "t3",
			Description: "Add search UI interface component and state",
			Kind:        KindFrontend,
			Complexity:  Simple,
			DependsOn:   []string{"t2"},
		})
		return tasks
	}

	// Default 2-step decomposition for general tasks
	tasks = append(tasks, Task{
		ID:          "t1",
		Description: fmt.Sprintf("Implement core logic for: %s", question),
		Kind:        KindBackend,
		Complexity:  Medium,
	})
	tasks = append(tasks, Task{
		ID:          "t2",
		Description: fmt.Sprintf("Write unit tests and verify: %s", question),
		Kind:        KindTest,
		Complexity:  Simple,
		DependsOn:   []string{"t1"},
	})

	return tasks
}

func parseTaskKind(s string) TaskKind {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "frontend":
		return KindFrontend
	case "backend":
		return KindBackend
	case "database", "db", "sql":
		return KindDatabase
	case "test", "testing":
		return KindTest
	case "design", "ui", "ux":
		return KindDesign
	case "config":
		return KindConfig
	case "docs":
		return KindDocs
	default:
		return KindGeneral
	}
}

func parseComplexity(s string) Complexity {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "complex", "high":
		return Complex
	case "medium":
		return Medium
	default:
		return Simple
	}
}

var jsonBlockRegex = regexp.MustCompile("(?s)```(?:json)?\\s*(\\[.*?\\])\\s*```")

func extractJSONString(s string) string {
	s = strings.TrimSpace(s)
	if matches := jsonBlockRegex.FindStringSubmatch(s); len(matches) > 1 {
		return strings.TrimSpace(matches[1])
	}
	start := strings.Index(s, "[")
	end := strings.LastIndex(s, "]")
	if start != -1 && end != -1 && end > start {
		return s[start : end+1]
	}
	return s
}

func isMockResponse(s string) bool {
	return strings.HasPrefix(s, "[Mock")
}
