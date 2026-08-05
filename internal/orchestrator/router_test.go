package orchestrator

import "testing"

func TestRouter_SelectModel(t *testing.T) {
	cfg := DefaultModelsConfig()
	router := NewRouter(cfg)

	tests := []struct {
		name          string
		task          Task
		expectedModel string
	}{
		{
			name:          "explicit model override",
			task:          Task{Model: "gpt-4o-mini", Kind: KindFrontend},
			expectedModel: "gpt-4o-mini",
		},
		{
			name:          "complex task escalation",
			task:          Task{Kind: KindBackend, Complexity: Complex},
			expectedModel: "claude-opus",
		},
		{
			name:          "kind routing database",
			task:          Task{Kind: KindDatabase, Complexity: Medium},
			expectedModel: "gemini-flash",
		},
		{
			name:          "kind routing frontend",
			task:          Task{Kind: KindFrontend, Complexity: Simple},
			expectedModel: "claude-sonnet",
		},
		{
			name:          "unknown kind fallback to default",
			task:          Task{Kind: TaskKind("unknown_kind"), Complexity: Simple},
			expectedModel: "claude-sonnet",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res, err := router.SelectModel(tt.task)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if res.Name != tt.expectedModel {
				t.Errorf("expected model %q, got %q", tt.expectedModel, res.Name)
			}
		})
	}
}
