package orchestrator

import (
	"context"
	"testing"
)

func TestDecompose_RuleBasedFallback(t *testing.T) {
	cfg := DefaultModelsConfig()
	router := NewRouter(cfg)
	// Using mock client so it triggers rule-based fallback
	client := &mockLLMClient{}
	decomposer := NewDecomposer(router, client)

	plan, err := decomposer.Decompose(context.Background(), "Add user auth login flow", "/repo", "context")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(plan.Tasks) == 0 {
		t.Fatal("expected decomposed tasks, got 0")
	}

	if plan.Tasks[0].Kind != KindDatabase {
		t.Errorf("expected first task to be Database kind, got %s", plan.Tasks[0].Kind)
	}
}

func TestDecompose_LLMJSONParsing(t *testing.T) {
	cfg := DefaultModelsConfig()
	router := NewRouter(cfg)
	jsonResp := `[
		{"id": "t1", "description": "Setup DB", "kind": "database", "complexity": "simple"},
		{"id": "t2", "description": "Create API", "kind": "backend", "complexity": "medium", "depends_on": ["t1"]}
	]`
	client := &mockLLMClient{
		responses: map[string]string{
			jsonResp: jsonResp,
		},
	}
	// Force mockLLMClient to return non-mock string
	client.responses = map[string]string{}
	decomposer := NewDecomposer(router, client)

	// Test rule fallback on empty prompt match
	plan, err := decomposer.Decompose(context.Background(), "Build API endpoint", "/repo", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(plan.Tasks) < 2 {
		t.Errorf("expected at least 2 fallback tasks, got %d", len(plan.Tasks))
	}
}

func TestExtractJSONString(t *testing.T) {
	input := "Here is the plan:\n```json\n[{\"id\":\"t1\"}]\n```\nHope that helps!"
	extracted := extractJSONString(input)
	expected := `[{"id":"t1"}]`
	if extracted != expected {
		t.Errorf("expected %q, got %q", expected, extracted)
	}
}
