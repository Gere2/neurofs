package orchestrator

import (
	"encoding/json"
	"os"
	"path/filepath"
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
	res, err := tuner.AutoTune(dir, 3, false)
	if err != nil {
		t.Fatalf("AutoTune failed: %v", err)
	}

	if res == nil {
		t.Fatal("expected non-nil TuningResult")
		return
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
	if !res.Changed {
		t.Error("a proposed rule change must set Changed")
	}
	if res.Applied {
		t.Error("AutoTune(apply=false) must not report Applied")
	}
	if _, err := os.Stat(filepath.Join(dir, ".neurofs", "models.json")); !os.IsNotExist(err) {
		t.Errorf("AutoTune(apply=false) must not write models.json; stat err=%v", err)
	}
}

// A threshold-only tune used to be reported but never persisted, because the
// save condition compared the new threshold against the already-mutated
// config value and so was always false.
func TestHyperAgent_AutoTuneApplyWrites(t *testing.T) {
	dir := t.TempDir()

	logger := NewTournamentLogger(dir)
	for i := 0; i < 5; i++ {
		_ = logger.LogRecord(TournamentRecord{
			Timestamp: time.Now(),
			Kind:      KindBackend,
			Model:     "gemini-flash",
			Provider:  "google",
			Grounding: 0.94,
			CostUSD:   0.0002,
			Accepted:  true,
		})
	}

	tuner := &HyperAgentTuner{RepoRoot: dir}
	res, err := tuner.AutoTune(dir, 3, true)
	if err != nil {
		t.Fatalf("AutoTune failed: %v", err)
	}
	if !res.Applied {
		t.Fatal("AutoTune(apply=true) with changes must report Applied")
	}

	data, err := os.ReadFile(filepath.Join(dir, ".neurofs", "models.json"))
	if err != nil {
		t.Fatalf("models.json must exist after apply: %v", err)
	}

	var cfg ModelsConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("models.json must be valid: %v", err)
	}
	if cfg.Routing["backend"] != "gemini-flash" {
		t.Errorf("persisted routing must carry the tune; got %q", cfg.Routing["backend"])
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
