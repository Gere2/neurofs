package gamestate

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestNewPlayerState(t *testing.T) {
	ps := NewPlayerState()
	if ps.Level != 1 {
		t.Errorf("expected level 1, got %d", ps.Level)
	}
	if ps.Title != "Aprendiz" {
		t.Errorf("expected title Aprendiz, got %s", ps.Title)
	}
	if ps.XPToNext != xpForLevel(2) {
		t.Errorf("expected XPToNext %d, got %d", xpForLevel(2), ps.XPToNext)
	}
}

func TestAddXP_LevelUp(t *testing.T) {
	ps := NewPlayerState()
	needed := ps.XPToNext

	// Grant exactly enough XP to level up
	ps.AddXP(needed, "test level up")

	if ps.Level != 2 {
		t.Errorf("expected level 2 after %d XP, got level %d", needed, ps.Level)
	}
	if ps.XP != 0 {
		t.Errorf("expected 0 remaining XP after exact level, got %d", ps.XP)
	}
}

func TestAddXP_MultipleLevel(t *testing.T) {
	ps := NewPlayerState()

	// Grant a ton of XP
	ps.AddXP(5000, "mega boost")

	if ps.Level < 5 {
		t.Errorf("expected level >= 5 after 5000 XP, got %d", ps.Level)
	}
	if ps.Title == "Aprendiz" {
		t.Error("expected title to have changed from Aprendiz")
	}
}

func TestAddXP_RecentEvents(t *testing.T) {
	ps := NewPlayerState()

	for i := 0; i < 30; i++ {
		ps.AddXP(1, "test")
	}

	if len(ps.RecentXP) > maxRecentXP {
		t.Errorf("expected max %d recent events, got %d", maxRecentXP, len(ps.RecentXP))
	}
}

func TestTitleForLevel(t *testing.T) {
	tests := []struct {
		level int
		title string
	}{
		{1, "Aprendiz"},
		{4, "Aprendiz"},
		{5, "Constructor"},
		{9, "Constructor"},
		{10, "Arquitecto"},
		{20, "Maestro"},
		{35, "Comandante"},
		{50, "Leyenda"},
		{99, "Leyenda"},
	}

	for _, tt := range tests {
		got := titleForLevel(tt.level)
		if got != tt.title {
			t.Errorf("titleForLevel(%d) = %q, want %q", tt.level, got, tt.title)
		}
	}
}

func TestRecordTaskResult_AgentStats(t *testing.T) {
	ps := NewPlayerState()

	ps.RecordTaskResult("gemini-flash", 0.92, 0.001, 0, 0.05, false, 0.85)

	if ps.TotalMissions != 1 {
		t.Errorf("expected 1 mission, got %d", ps.TotalMissions)
	}

	agent, ok := ps.Agents["gemini-flash"]
	if !ok {
		t.Fatal("expected gemini-flash agent to exist")
	}
	if agent.Wins != 1 {
		t.Errorf("expected 1 win, got %d", agent.Wins)
	}
	if agent.DisplayName != "Flash" {
		t.Errorf("expected display name Flash, got %s", agent.DisplayName)
	}
	if agent.CascadesAvoided != 1 {
		t.Errorf("expected 1 cascade avoided, got %d", agent.CascadesAvoided)
	}
}

func TestRecordTaskResult_Loss(t *testing.T) {
	ps := NewPlayerState()

	ps.RecordTaskResult("gemini-flash", 0.50, 0.001, 1, 0, false, 0.85)

	agent := ps.Agents["gemini-flash"]
	if agent.Wins != 0 {
		t.Errorf("expected 0 wins for low grounding, got %d", agent.Wins)
	}
	if agent.Losses != 1 {
		t.Errorf("expected 1 loss, got %d", agent.Losses)
	}
}

func TestGrantXPForTask(t *testing.T) {
	ps := NewPlayerState()

	// High grounding, cascade avoided, not complex
	xp, reasons := ps.GrantXPForTask(0.92, 0, 0.05, false, 0.85)

	if xp < XPTaskComplete+XPCascadeEfficient {
		t.Errorf("expected at least %d XP, got %d", XPTaskComplete+XPCascadeEfficient, xp)
	}
	if len(reasons) < 2 {
		t.Errorf("expected at least 2 reasons, got %d", len(reasons))
	}
}

func TestGrantXPForTask_Complex(t *testing.T) {
	ps := NewPlayerState()

	// Complex task, perfect grounding
	xp, _ := ps.GrantXPForTask(0.96, 2, 0, true, 0.85)

	expectedMin := XPTaskComplete + XPComplexResolved + XPGroundingPerfect
	if xp < expectedMin {
		t.Errorf("expected at least %d XP for complex+perfect, got %d", expectedMin, xp)
	}
}

func TestGrantXPForTask_LowGrounding(t *testing.T) {
	ps := NewPlayerState()

	// Low grounding — no XP
	xp, reasons := ps.GrantXPForTask(0.50, 2, 0, false, 0.85)

	if xp != 0 {
		t.Errorf("expected 0 XP for low grounding, got %d", xp)
	}
	if len(reasons) != 0 {
		t.Errorf("expected 0 reasons, got %d", len(reasons))
	}
}

func TestCheckAchievements(t *testing.T) {
	ps := NewPlayerState()
	ps.TotalMissions = 1

	newAchievements := ps.CheckAchievements()

	found := false
	for _, a := range newAchievements {
		if a.ID == "first_mission" {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected 'first_mission' achievement")
	}

	// Second check should not re-grant
	newAchievements2 := ps.CheckAchievements()
	for _, a := range newAchievements2 {
		if a.ID == "first_mission" {
			t.Error("should not re-grant first_mission")
		}
	}
}

func TestCheckAchievements_Multiple(t *testing.T) {
	ps := NewPlayerState()
	ps.TotalMissions = 100
	ps.TotalSavedUSD = 6.0
	ps.Streak = 8
	ps.Level = 10

	newAchievements := ps.CheckAchievements()

	// Should have earned multiple achievements
	if len(newAchievements) < 5 {
		t.Errorf("expected at least 5 achievements, got %d", len(newAchievements))
	}
}

// The streak advances per run, not per task: it is driven by
// AdvanceDailyStreak, which RecordPlanOutcome calls once, rather than by
// RecordTaskResult, which runs for every task in a plan.
func TestStreakTracking(t *testing.T) {
	ps := NewPlayerState()

	// Recording task results is not by itself a day of activity.
	ps.RecordTaskResult("gemini-flash", 0.90, 0.001, 0, 0, false, 0.85)
	if ps.Streak != 0 {
		t.Errorf("task recording must not touch the streak, got %d", ps.Streak)
	}

	// First activity of the day
	ps.AdvanceDailyStreak()
	if ps.Streak != 1 {
		t.Errorf("expected streak 1, got %d", ps.Streak)
	}

	// Another run the same day — streak shouldn't change
	ps.RecordTaskResult("gemini-flash", 0.90, 0.001, 0, 0, false, 0.85)
	ps.AdvanceDailyStreak()
	if ps.Streak != 1 {
		t.Errorf("expected streak still 1, got %d", ps.Streak)
	}
}

func TestSaveAndLoad(t *testing.T) {
	dir := t.TempDir()

	ps := NewPlayerState()
	ps.AddXP(200, "test")
	ps.RecordTaskResult("gemini-flash", 0.92, 0.001, 0, 0.05, false, 0.85)

	err := ps.Save(dir)
	if err != nil {
		t.Fatalf("save failed: %v", err)
	}

	// Verify file exists
	if _, err := os.Stat(filepath.Join(dir, "player.json")); err != nil {
		t.Fatalf("player.json not created: %v", err)
	}

	// Load and verify
	loaded, err := Load(dir)
	if err != nil {
		t.Fatalf("load failed: %v", err)
	}

	if loaded.Level != ps.Level {
		t.Errorf("loaded level %d != saved level %d", loaded.Level, ps.Level)
	}
	if loaded.TotalMissions != 1 {
		t.Errorf("loaded missions %d, expected 1", loaded.TotalMissions)
	}
	if _, ok := loaded.Agents["gemini-flash"]; !ok {
		t.Error("expected gemini-flash agent in loaded state")
	}
}

func TestLoad_NoFile(t *testing.T) {
	dir := t.TempDir()

	ps, err := Load(dir)
	if err != nil {
		t.Fatalf("load non-existent should not error: %v", err)
	}
	if ps.Level != 1 {
		t.Errorf("expected fresh state with level 1, got %d", ps.Level)
	}
}

// player.json is the entire progression and Load treats a parse failure as a
// hard error, so Save must replace the file rather than truncate it in place.
// Crash-safety itself needs fault injection to test directly; what is testable
// is the property that provides it — the replacement goes through a rename, so
// it succeeds even when the existing target is not itself writable, and never
// leaves the previous contents partially overwritten.
func TestSave_ReplacesRatherThanTruncates(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "player.json")

	first := NewPlayerState()
	first.AddXP(10, "seed")
	if err := first.Save(dir); err != nil {
		t.Fatalf("first save: %v", err)
	}
	if err := os.Chmod(path, 0o444); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o644) })

	second := NewPlayerState()
	second.AddXP(999, "replacement")
	if err := second.Save(dir); err != nil {
		t.Fatalf("save over a read-only target must succeed via rename: %v", err)
	}

	loaded, err := Load(dir)
	if err != nil {
		t.Fatalf("load after replace: %v", err)
	}
	if loaded.XP == 0 && loaded.Level == 1 {
		t.Fatal("state did not survive the replacement")
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	for _, e := range entries {
		if e.Name() != "player.json" {
			t.Errorf("save left a stray file behind: %s", e.Name())
		}
	}
}

// The streak is a per-day signal, so recording a twelve-task plan must count
// as one day, and a second run the same day must not pay again.
func TestGrantStreakXP_OncePerDay(t *testing.T) {
	ps := NewPlayerState()

	if got := ps.GrantStreakXP(); got != XPStreakDaily {
		t.Fatalf("first activity of the day = %d XP, want %d", got, XPStreakDaily)
	}
	if ps.Streak != 1 {
		t.Errorf("streak = %d, want 1", ps.Streak)
	}
	if got := ps.GrantStreakXP(); got != 0 {
		t.Errorf("second call the same day = %d XP, want 0", got)
	}
	if ps.Streak != 1 {
		t.Errorf("streak must not advance twice in a day, got %d", ps.Streak)
	}
}

// A consecutive day extends the streak; a gap restarts it at 1 rather than
// zeroing progression — the vision forbids punishing absence.
func TestAdvanceDailyStreak_ContinuesAndResets(t *testing.T) {
	ps := NewPlayerState()
	ps.LastActiveDay = time.Now().AddDate(0, 0, -1).Format("2006-01-02")
	ps.Streak = 4
	if !ps.AdvanceDailyStreak() {
		t.Fatal("a new day must advance")
	}
	if ps.Streak != 5 {
		t.Errorf("consecutive day: streak = %d, want 5", ps.Streak)
	}

	gap := NewPlayerState()
	gap.LastActiveDay = time.Now().AddDate(0, 0, -9).Format("2006-01-02")
	gap.Streak = 7
	gap.AdvanceDailyStreak()
	if gap.Streak != 1 {
		t.Errorf("after a gap: streak = %d, want 1", gap.Streak)
	}
}

// All seven XP sources in the vision must have a code path. Three of them
// (project completion, streak, learn loop) were declared and never granted.
func TestEveryXPSourceIsReachable(t *testing.T) {
	ps := NewPlayerState()

	if got := ps.GrantProjectCompleteXP("plan-1"); got != XPProjectComplete {
		t.Errorf("project completion = %d, want %d", got, XPProjectComplete)
	}
	if got := ps.GrantLearnImprovedXP(0.80, 0.91); got != XPLearnImproved {
		t.Errorf("learn improvement = %d, want %d", got, XPLearnImproved)
	}
	if got := ps.GrantStreakXP(); got != XPStreakDaily {
		t.Errorf("streak = %d, want %d", got, XPStreakDaily)
	}
	perTask, _ := ps.GrantXPForTask(0.97, 0, 0.02, true, 0.85)
	want := XPTaskComplete + XPCascadeEfficient + XPComplexResolved + XPGroundingPerfect
	if perTask != want {
		t.Errorf("per-task XP = %d, want %d", perTask, want)
	}

	// Every grant must leave a feed entry the UI can render.
	if len(ps.RecentXP) == 0 {
		t.Fatal("no XP events recorded")
	}
	for _, e := range ps.RecentXP {
		if e.Reason == "" {
			t.Error("XP event with no reason would render as a blank feed row")
		}
	}
}
