package orchestrator

import (
	"context"
	"testing"
)

func TestOrchestrator_RunSuccess(t *testing.T) {
	mockClient := &mockLLMClient{
		responses: map[string]string{},
	}

	orc, err := NewOrchestrator(t.TempDir(), mockClient)
	if err != nil {
		t.Fatalf("failed to create orchestrator: %v", err)
	}

	var events []StatusEvent
	opts := OrchestrationOptions{
		Question: "Add user authentication flow",
		RepoRoot: t.TempDir(),
		Callback: func(ev StatusEvent) {
			events = append(events, ev)
		},
	}

	res, err := orc.Run(context.Background(), opts)
	if err != nil {
		t.Fatalf("orchestration failed: %v", err)
	}

	if res.Plan.Status != StatusDone {
		t.Errorf("expected plan status Done, got %s", res.Plan.Status)
	}

	if len(res.Plan.Tasks) == 0 {
		t.Fatal("expected tasks in plan")
	}

	if len(events) == 0 {
		t.Errorf("expected progress events to be emitted")
	}
}
