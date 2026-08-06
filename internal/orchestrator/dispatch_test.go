package orchestrator

import (
	"context"
	"testing"
)

type mockLLMClient struct {
	responses map[string]string
	err       error
}

func (m *mockLLMClient) Complete(ctx context.Context, entry ModelEntry, prompt string) (string, int, int, error) {
	if m.err != nil {
		return "", 0, 0, m.err
	}
	resp, ok := m.responses[prompt]
	if !ok {
		resp = "mock completion"
	}
	return resp, 100, 50, nil
}

func TestDispatcher_DispatchSuccess(t *testing.T) {
	cfg := DefaultModelsConfig()
	router := NewRouter(cfg)
	client := &mockLLMClient{
		responses: map[string]string{
			"Task 1": "Done 1",
			"Task 2": "Done 2",
		},
	}
	dispatcher := NewDispatcher(router, client)

	plan := &Plan{
		ID:       "plan-1",
		Question: "Build test feature",
		Tasks: []Task{
			{ID: "t1", Description: "Task 1", Kind: KindBackend, Complexity: Simple},
			{ID: "t2", Description: "Task 2", Kind: KindFrontend, Complexity: Simple, DependsOn: []string{"t1"}},
		},
	}

	var events []StatusEvent
	cb := func(ev StatusEvent) {
		events = append(events, ev)
	}

	err := dispatcher.Dispatch(context.Background(), plan, cb)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if plan.Status != StatusDone {
		t.Errorf("expected plan status Done, got %s", plan.Status)
	}

	if plan.Tasks[0].Status != StatusDone || plan.Tasks[1].Status != StatusDone {
		t.Errorf("expected all tasks Done")
	}

	if len(events) == 0 {
		t.Errorf("expected status events to be published")
	}

	if plan.TotalCost() <= 0 {
		t.Errorf("expected positive total cost")
	}
}

func TestDispatcher_DependencySkipped(t *testing.T) {
	cfg := DefaultModelsConfig()
	router := NewRouter(cfg)
	client := &mockLLMClient{err: context.Canceled}
	dispatcher := NewDispatcher(router, client)

	plan := &Plan{
		ID:       "plan-2",
		Question: "Failing plan",
		Tasks: []Task{
			{ID: "t1", Description: "Task 1", Kind: KindBackend},
			{ID: "t2", Description: "Task 2", Kind: KindFrontend, DependsOn: []string{"t1"}},
		},
	}

	_ = dispatcher.Dispatch(context.Background(), plan, nil)

	if plan.Tasks[0].Status != StatusFailed {
		t.Errorf("expected task 1 Failed, got %s", plan.Tasks[0].Status)
	}
	if plan.Tasks[1].Status != StatusSkipped {
		t.Errorf("expected task 2 Skipped due to failed dependency, got %s", plan.Tasks[1].Status)
	}
}
