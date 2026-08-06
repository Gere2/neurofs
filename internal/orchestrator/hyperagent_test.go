package orchestrator

import (
	"testing"
	"time"
)

func TestHyperAgent_AutoTune(t *testing.T) {
	dir := t.TempDir()

	// Write mock routing_history.jsonl
	logger := NewTournamentLogger(dir)
	for i := 0; i < 5; i++ {
		_ = logger.LogRecord(TournamentRecord{
			Timestamp:  time.Now(),
			Kind:       KindBackend,
			Model:      "gemini-flash",
			Provider:   "google",
			Grounding:  0.94,
			CostUSD:    0.0002,
			DurationMs: 200,
			Accepted:   true,
		})
	}

	tuner := &HyperAgentTuner{RepoRoot: dir}
	res, err := tuner.AutoTune(dir, 3)
	if err != nil {
		t.Fatalf("AutoTune failed: %v", err)
	}

	if res == nil {
		t.Fatal("expected non-nil TuningResult")
	}

	// Should have updated backend routing rule from claude-sonnet to gemini-flash
	newModel, ok := res.RulesUpdated["backend"]
	if !ok {
		t.Error("expected backend routing rule to be updated")
	}
	if newModel != "gemini-flash" {
		t.Errorf("expected new backend model gemini-flash, got %s", newModel)
	}

	if len(res.InsightsGenerated) == 0 {
		t.Error("expected human-readable insights to be generated")
	}
}

func TestSkillStore_SaveAndLoad(t *testing.T) {
	dir := t.TempDir()
	store := NewSkillStore(dir)

	skill := CrossProjectSkill{
		ID:               "skill-1",
		Domain:           "go_cli",
		TaskKind:         KindBackend,
		RecommendedModel: "gemini-flash",
		Insight:          "flash is 95% reliable for Go CLI tasks",
		Confidence:       0.95,
	}

	if err := store.SaveSkill(skill); err != nil {
		t.Fatalf("failed to save skill: %v", err)
	}

	skills, err := store.LoadSkills()
	if err != nil {
		t.Fatalf("failed to load skills: %v", err)
	}

	if len(skills) != 1 {
		t.Fatalf("expected 1 skill, got %d", len(skills))
	}

	if skills[0].Domain != "go_cli" {
		t.Errorf("expected domain go_cli, got %s", skills[0].Domain)
	}
	if skills[0].RecommendedModel != "gemini-flash" {
		t.Errorf("expected recommended model gemini-flash, got %s", skills[0].RecommendedModel)
	}
}
