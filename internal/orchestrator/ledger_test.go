package orchestrator

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func donePlan(tasks ...Task) Plan {
	return Plan{ID: "plan-1", Tasks: tasks}
}

// Without an API key DefaultLLMClient.Complete returns a placeholder instead
// of failing, so a keyless run yields a plan full of "done" tasks whose
// grounding and cost describe the placeholder. Those must stay out of the
// tournament ledger, or AutoTune will retune routing from them.
func TestRecordPlanOutcome_SkipsSyntheticTasks(t *testing.T) {
	dir := t.TempDir()

	plan := donePlan(
		Task{ID: "t1", Kind: KindBackend, Status: StatusDone, Model: "gemini-flash", Grounding: 0.91, Synthetic: true},
		Task{ID: "t2", Kind: KindBackend, Status: StatusDone, Model: "claude-sonnet", Grounding: 0.93},
	)

	if err := RecordPlanOutcome(dir, plan); err != nil {
		t.Fatalf("RecordPlanOutcome: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, "routing_history.jsonl"))
	if err != nil {
		t.Fatalf("read ledger: %v", err)
	}
	body := string(data)

	if strings.Contains(body, `"t1"`) {
		t.Errorf("synthetic task must not reach the ledger:\n%s", body)
	}
	if !strings.Contains(body, `"t2"`) {
		t.Errorf("real task must reach the ledger:\n%s", body)
	}
	if n := strings.Count(strings.TrimSpace(body), "\n") + 1; n != 1 {
		t.Errorf("expected exactly 1 ledger row, got %d:\n%s", n, body)
	}
}

// A task that never cleared the grounding floor was still logged with
// Accepted:true, which pinned every model's WinRate at 100% and made the
// tournament ranking meaningless.
func TestRecordPlanOutcome_AcceptedReflectsGrounding(t *testing.T) {
	dir := t.TempDir()

	plan := donePlan(
		Task{ID: "low", Kind: KindBackend, Status: StatusDone, Model: "cheap", Grounding: 0.10},
	)

	if err := RecordPlanOutcome(dir, plan); err != nil {
		t.Fatalf("RecordPlanOutcome: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, "routing_history.jsonl"))
	if err != nil {
		t.Fatalf("read ledger: %v", err)
	}
	if strings.Contains(string(data), `"accepted":true`) {
		t.Errorf("a 10%% grounding result must not be logged as accepted:\n%s", data)
	}
}

func TestSyntheticTaskCount(t *testing.T) {
	plan := donePlan(
		Task{ID: "a", Status: StatusDone, Synthetic: true},
		Task{ID: "b", Status: StatusDone},
		Task{ID: "c", Status: StatusDone, Synthetic: true},
	)
	if got := SyntheticTaskCount(plan); got != 2 {
		t.Errorf("SyntheticTaskCount = %d, want 2", got)
	}
}
