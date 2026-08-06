package orchestrator

import (
	"encoding/json"
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

// Project completion is the largest single bonus, so it must require that
// every task finished AND that none of them was a placeholder from a keyless
// run.
func TestRecordPlanOutcome_ProjectCompletionRequiresRealTasks(t *testing.T) {
	t.Run("all real and done", func(t *testing.T) {
		dir := t.TempDir()
		plan := donePlan(
			Task{ID: "a", Kind: KindBackend, Status: StatusDone, Model: "m", Grounding: 0.9},
			Task{ID: "b", Kind: KindBackend, Status: StatusDone, Model: "m", Grounding: 0.9},
		)
		if err := RecordPlanOutcome(dir, plan); err != nil {
			t.Fatalf("RecordPlanOutcome: %v", err)
		}
		if !hasXPReason(t, dir, "Proyecto completado") {
			t.Error("a fully real, fully done plan must award project completion")
		}
	})

	t.Run("one synthetic task", func(t *testing.T) {
		dir := t.TempDir()
		plan := donePlan(
			Task{ID: "a", Kind: KindBackend, Status: StatusDone, Model: "m", Grounding: 0.9},
			Task{ID: "b", Kind: KindBackend, Status: StatusDone, Model: "m", Grounding: 0.9, Synthetic: true},
		)
		if err := RecordPlanOutcome(dir, plan); err != nil {
			t.Fatalf("RecordPlanOutcome: %v", err)
		}
		if hasXPReason(t, dir, "Proyecto completado") {
			t.Error("a plan containing placeholder output must not award project completion")
		}
	})

	t.Run("one task still running", func(t *testing.T) {
		dir := t.TempDir()
		plan := donePlan(
			Task{ID: "a", Kind: KindBackend, Status: StatusDone, Model: "m", Grounding: 0.9},
			Task{ID: "b", Kind: KindBackend, Status: StatusRunning, Model: "m"},
		)
		if err := RecordPlanOutcome(dir, plan); err != nil {
			t.Fatalf("RecordPlanOutcome: %v", err)
		}
		if hasXPReason(t, dir, "Proyecto completado") {
			t.Error("an unfinished plan must not award project completion")
		}
	})
}

// The streak must be awarded once for the run, not once per task.
func TestRecordPlanOutcome_StreakAwardedOncePerRun(t *testing.T) {
	dir := t.TempDir()
	plan := donePlan(
		Task{ID: "a", Kind: KindBackend, Status: StatusDone, Model: "m", Grounding: 0.9},
		Task{ID: "b", Kind: KindBackend, Status: StatusDone, Model: "m", Grounding: 0.9},
		Task{ID: "c", Kind: KindBackend, Status: StatusDone, Model: "m", Grounding: 0.9},
	)
	if err := RecordPlanOutcome(dir, plan); err != nil {
		t.Fatalf("RecordPlanOutcome: %v", err)
	}
	if n := countXPReason(t, dir, "Racha diaria"); n != 1 {
		t.Errorf("streak awarded %d times for a 3-task plan, want 1", n)
	}
}

func loadPlayer(t *testing.T, dir string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "player.json"))
	if err != nil {
		t.Fatalf("read player.json: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("parse player.json: %v", err)
	}
	return m
}

func countXPReason(t *testing.T, dir, needle string) int {
	t.Helper()
	m := loadPlayer(t, dir)
	events, _ := m["recent_xp"].([]any)
	n := 0
	for _, e := range events {
		ev, _ := e.(map[string]any)
		reason, _ := ev["reason"].(string)
		if strings.Contains(reason, needle) {
			n++
		}
	}
	return n
}

func hasXPReason(t *testing.T, dir, needle string) bool {
	t.Helper()
	return countXPReason(t, dir, needle) > 0
}
