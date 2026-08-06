// Package gamestate manages the player progression system for NeuroFS
// Agent Commander. Every stat, level, and achievement is derived from
// real orchestration data — no synthetic gamification. The player.json
// file persists state across sessions.
//
// XP sources (all from verified backend events):
//   - Task completion with grounding ≥ threshold
//   - Cascade efficiency (resolved by cheap model)
//   - Complex task resolution
//   - Project completion
//   - Daily streaks
//   - Learn loop improvements
package gamestate

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/Gere2/neurofs/internal/atomicfile"
)

// PlayerState is the persistent game state, saved to ~/.neurofs/player.json.
type PlayerState struct {
	mu sync.RWMutex `json:"-"`

	// Core progression
	Level    int    `json:"level"`
	XP       int    `json:"xp"`
	XPToNext int    `json:"xp_to_next"`
	Title    string `json:"title"`

	// Activity tracking
	Streak        int       `json:"streak"`          // consecutive days with ≥1 task
	LastActiveDay string    `json:"last_active_day"` // "2026-08-06"
	TotalMissions int       `json:"total_missions"`
	TotalSavedUSD float64   `json:"total_saved_usd"`
	JoinedAt      time.Time `json:"joined_at"`

	// Agent stats (keyed by model name)
	Agents map[string]*AgentStats `json:"agents"`

	// Achievements
	Achievements []Achievement `json:"achievements"`

	// Aggregate metrics
	MeanGrounding float64 `json:"mean_grounding"`
	TotalCostUSD  float64 `json:"total_cost_usd"`

	// Recent XP events (last 20, for UI feed)
	RecentXP []XPEvent `json:"recent_xp"`
}

// AgentStats tracks real performance data for a model/agent.
type AgentStats struct {
	Name            string   `json:"name"`
	DisplayName     string   `json:"display_name"`
	Emoji           string   `json:"emoji"`
	Wins            int      `json:"wins"`             // grounding ≥ threshold
	Losses          int      `json:"losses"`           // grounding < threshold or error
	Reliability     float64  `json:"reliability"`      // mean grounding (0-1)
	Economy         float64  `json:"economy"`          // % cost saved vs Opus
	Speed           float64  `json:"speed"`            // normalized speed (0-1)
	CascadesAvoided int      `json:"cascades_avoided"` // resolved without escalation
	TotalCostUSD    float64  `json:"total_cost_usd"`
	Specialties     []string `json:"specialties"`
}

// Achievement represents a milestone badge earned from real usage.
type Achievement struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	Emoji       string    `json:"emoji"`
	EarnedAt    time.Time `json:"earned_at"`
}

// XPEvent records a single XP-granting action.
type XPEvent struct {
	Amount    int       `json:"amount"`
	Reason    string    `json:"reason"`
	Timestamp time.Time `json:"timestamp"`
}

// XP amounts for different actions
const (
	XPTaskComplete     = 5   // task with grounding ≥ threshold
	XPCascadeEfficient = 15  // resolved by cheap model, saved money
	XPComplexResolved  = 25  // complex task with grounding ≥ 90%
	XPProjectComplete  = 100 // all tasks in a plan done
	XPStreakDaily      = 10  // per day of streak
	XPGroundingPerfect = 10  // grounding > 95%
	XPLearnImproved    = 50  // learn loop improved weights

	maxRecentXP = 20
)

// Level tiers with titles and XP thresholds
var levelTiers = []struct {
	MinLevel int
	Title    string
}{
	{1, "Aprendiz"},
	{5, "Constructor"},
	{10, "Arquitecto"},
	{20, "Maestro"},
	{35, "Comandante"},
	{50, "Leyenda"},
}

// Feature names a capability that the player unlocks by levelling up.
type Feature string

const (
	FeatureCascadeManual Feature = "cascade_manual"
	FeatureTournament    Feature = "tournament"
	FeatureCrossProject  Feature = "cross_project_memory"
	FeatureHyperAgent    Feature = "hyperagent"
	FeatureAgentCardsA2A Feature = "agent_cards_a2a"
)

// Unlock ties a feature to the level that reveals it.
type Unlock struct {
	Feature  Feature `json:"feature"`
	MinLevel int     `json:"min_level"`
	Label    string  `json:"label"`
	Teaches  string  `json:"teaches"`
}

// featureUnlocks is the progression ladder. It is deliberately about
// *progressive disclosure*, not access control: level 1 shows an input, the
// agents and the result, and everything else appears as the player earns the
// context to understand it. Locking teaches; it does not defend. The CLI and
// the HTTP API stay fully open at every level, so scripts, automation and
// power users are never gated — only the UI reveals gradually.
var featureUnlocks = []Unlock{
	{FeatureCascadeManual, 5, "Cascade manual",
		"Elegir qué modelo intenta cada tarea, y cuándo escalar."},
	{FeatureTournament, 10, "War Room",
		"Qué modelo gana en cada tipo de tarea, medido sobre tu propio historial."},
	{FeatureCrossProject, 20, "Cross-project memory",
		"Lo aprendido en un repo alimenta los demás."},
	{FeatureHyperAgent, 35, "HyperAgent",
		"Re-afinar el routing automáticamente desde los datos del torneo."},
	{FeatureAgentCardsA2A, 50, "Agent Cards A2A",
		"Publicar NeuroFS como agente descubrible por otros agentes."},
}

// FeatureUnlocks returns the full ladder, locked and unlocked alike, so the UI
// can show what is still ahead — a lock the player cannot see teaches nothing.
func FeatureUnlocks() []Unlock {
	out := make([]Unlock, len(featureUnlocks))
	copy(out, featureUnlocks)
	return out
}

// LevelFor returns the level at which feature becomes available, and whether
// it is a known feature.
func LevelFor(feature Feature) (int, bool) {
	for _, u := range featureUnlocks {
		if u.Feature == feature {
			return u.MinLevel, true
		}
	}
	return 0, false
}

// IsUnlocked reports whether the player has reached the level for feature.
// Unknown features are treated as unlocked: a capability nobody put on the
// ladder is not a secret, and failing open keeps a typo from hiding real UI.
func (ps *PlayerState) IsUnlocked(feature Feature) bool {
	minLevel, known := LevelFor(feature)
	if !known {
		return true
	}
	ps.mu.RLock()
	defer ps.mu.RUnlock()
	return ps.Level >= minLevel
}

// UnlockState is the per-feature view handed to the UI.
type UnlockState struct {
	Unlock
	Unlocked bool `json:"unlocked"`
}

// Unlocks returns the ladder annotated with what the player has reached.
func (ps *PlayerState) Unlocks() []UnlockState {
	ps.mu.RLock()
	level := ps.Level
	ps.mu.RUnlock()

	out := make([]UnlockState, 0, len(featureUnlocks))
	for _, u := range featureUnlocks {
		out = append(out, UnlockState{Unlock: u, Unlocked: level >= u.MinLevel})
	}
	return out
}

// NextUnlock returns the next feature the player has not reached yet, so the
// UI can name the reward instead of showing a bare progress bar. The second
// result is false once everything is unlocked.
func (ps *PlayerState) NextUnlock() (Unlock, bool) {
	ps.mu.RLock()
	level := ps.Level
	ps.mu.RUnlock()

	for _, u := range featureUnlocks {
		if level < u.MinLevel {
			return u, true
		}
	}
	return Unlock{}, false
}

// xpForLevel returns the XP needed to reach a given level.
// Uses a gentle curve: level N needs N*50 XP from the previous level.
func xpForLevel(level int) int {
	if level <= 1 {
		return 0
	}
	return level * 50
}

// titleForLevel returns the appropriate title for a level.
func titleForLevel(level int) string {
	title := "Aprendiz"
	for _, tier := range levelTiers {
		if level >= tier.MinLevel {
			title = tier.Title
		}
	}
	return title
}

// NewPlayerState creates a fresh player state.
func NewPlayerState() *PlayerState {
	return &PlayerState{
		Level:    1,
		XP:       0,
		XPToNext: xpForLevel(2),
		Title:    "Aprendiz",
		Agents:   make(map[string]*AgentStats),
		JoinedAt: time.Now(),
	}
}

// Load reads player state from disk. Returns a new state if file doesn't exist.
func Load(dir string) (*PlayerState, error) {
	path := filepath.Join(dir, "player.json")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return NewPlayerState(), nil
		}
		return nil, fmt.Errorf("read player state: %w", err)
	}

	var ps PlayerState
	if err := json.Unmarshal(data, &ps); err != nil {
		return nil, fmt.Errorf("parse player state: %w", err)
	}
	if ps.Agents == nil {
		ps.Agents = make(map[string]*AgentStats)
	}
	return &ps, nil
}

// Save writes the player state to disk atomically.
//
// Atomicity is not a nicety here: player.json IS the whole progression, and
// Load treats a parse failure as a hard error, so a torn write — a crash or a
// full disk mid-save — does not degrade the state, it erases the player's
// history. os.WriteFile truncates in place and cannot offer that guarantee.
func (ps *PlayerState) Save(dir string) error {
	ps.mu.RLock()
	defer ps.mu.RUnlock()

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(ps, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(dir, "player.json")
	return atomicfile.WriteFile(path, append(data, '\n'), 0o644)
}

// AddXP grants experience points, handles leveling up, and records the event.
func (ps *PlayerState) AddXP(amount int, reason string) {
	ps.mu.Lock()
	defer ps.mu.Unlock()

	ps.XP += amount

	// Record event
	event := XPEvent{
		Amount:    amount,
		Reason:    reason,
		Timestamp: time.Now(),
	}
	ps.RecentXP = append(ps.RecentXP, event)
	if len(ps.RecentXP) > maxRecentXP {
		ps.RecentXP = ps.RecentXP[len(ps.RecentXP)-maxRecentXP:]
	}

	// Level up check
	for ps.XP >= ps.XPToNext {
		ps.XP -= ps.XPToNext
		ps.Level++
		ps.XPToNext = xpForLevel(ps.Level + 1)
		ps.Title = titleForLevel(ps.Level)
	}
}

// RecordTaskResult updates game state from a real orchestration task result.
func (ps *PlayerState) RecordTaskResult(
	modelName string,
	grounding float64,
	costUSD float64,
	cascadeLevel int,
	cascadeSaved float64,
	isComplex bool,
	groundingThreshold float64,
) {
	ps.mu.Lock()
	defer ps.mu.Unlock()

	ps.TotalMissions++
	ps.TotalCostUSD += costUSD
	ps.TotalSavedUSD += cascadeSaved

	// Update agent stats
	agent := ps.ensureAgent(modelName)
	if grounding >= groundingThreshold {
		agent.Wins++
	} else {
		agent.Losses++
	}
	// Running average for reliability
	total := agent.Wins + agent.Losses
	agent.Reliability = (agent.Reliability*float64(total-1) + grounding) / float64(total)
	agent.TotalCostUSD += costUSD
	if cascadeLevel == 0 {
		agent.CascadesAvoided++
	}

	// Update global mean grounding
	ps.MeanGrounding = (ps.MeanGrounding*float64(ps.TotalMissions-1) + grounding) / float64(ps.TotalMissions)
}

// AdvanceDailyStreak rolls the daily streak forward and reports whether this
// call was the first activity of a new day. It used to live inside
// RecordTaskResult, which made it per-task rather than per-session and left it
// holding the lock, so no XP could be granted from it — AddXP takes the same
// non-reentrant mutex. Callers now invoke it once per run and grant the streak
// XP themselves.
func (ps *PlayerState) AdvanceDailyStreak() bool {
	ps.mu.Lock()
	defer ps.mu.Unlock()

	today := time.Now().Format("2006-01-02")
	if ps.LastActiveDay == today {
		return false
	}
	yesterday := time.Now().AddDate(0, 0, -1).Format("2006-01-02")
	if ps.LastActiveDay == yesterday {
		ps.Streak++
	} else {
		// Both a first-ever day and a broken streak start at 1. Missing days
		// costs the streak, never the progression: the vision is explicit
		// that not playing must not be punished.
		ps.Streak = 1
	}
	ps.LastActiveDay = today
	return true
}

// GrantStreakXP awards the daily streak bonus if this is the first activity
// today, and reports the XP granted.
//
// The vision's table reads "+10 XP × día", which can be read as a flat daily
// bonus or as ten XP per day of streak. This takes the flat reading: it is the
// conservative one, and an unbounded multiplier would let a long streak
// out-earn every other source combined. Switch the amount here if the intent
// was the multiplier.
func (ps *PlayerState) GrantStreakXP() int {
	if !ps.AdvanceDailyStreak() {
		return 0
	}
	ps.mu.RLock()
	streak := ps.Streak
	ps.mu.RUnlock()

	ps.AddXP(XPStreakDaily, fmt.Sprintf("Racha diaria: %d día(s)", streak))
	return XPStreakDaily
}

// GrantProjectCompleteXP awards the project-completion bonus, the largest
// single source in the vision's table.
func (ps *PlayerState) GrantProjectCompleteXP(planID string) int {
	ps.AddXP(XPProjectComplete, fmt.Sprintf("Proyecto completado: %s", planID))
	return XPProjectComplete
}

// GrantLearnImprovedXP awards the learn-loop bonus. This is the "aprender" edge
// of the vision's triple flywheel (jugar → aprender → mejorar) and was the one
// XP source with no code path at all.
func (ps *PlayerState) GrantLearnImprovedXP(fromRecall, toRecall float64) int {
	ps.AddXP(XPLearnImproved, fmt.Sprintf(
		"Learn loop mejoró los weights (recall %.1f%% → %.1f%%)", fromRecall*100, toRecall*100))
	return XPLearnImproved
}

// ensureAgent returns the agent stats for a model, creating if needed.
func (ps *PlayerState) ensureAgent(modelName string) *AgentStats {
	if ps.Agents == nil {
		ps.Agents = make(map[string]*AgentStats)
	}
	agent, ok := ps.Agents[modelName]
	if !ok {
		agent = defaultAgentProfile(modelName)
		ps.Agents[modelName] = agent
	}
	return agent
}

// GrantXPForTask calculates and grants XP based on task outcome.
// Returns the total XP granted and reasons.
func (ps *PlayerState) GrantXPForTask(
	grounding float64,
	cascadeLevel int,
	cascadeSaved float64,
	isComplex bool,
	groundingThreshold float64,
) (int, []string) {
	totalXP := 0
	var reasons []string

	// Base XP for completion with adequate grounding
	if grounding >= groundingThreshold {
		totalXP += XPTaskComplete
		reasons = append(reasons, fmt.Sprintf("+%d Tarea completada (grounding %.0f%%)", XPTaskComplete, grounding*100))
	}

	// Cascade efficiency bonus
	if cascadeLevel == 0 && cascadeSaved > 0 {
		totalXP += XPCascadeEfficient
		reasons = append(reasons, fmt.Sprintf("+%d Cascade eficiente (ahorro $%.4f)", XPCascadeEfficient, cascadeSaved))
	}

	// Complex task bonus
	if isComplex && grounding >= 0.90 {
		totalXP += XPComplexResolved
		reasons = append(reasons, fmt.Sprintf("+%d Tarea compleja resuelta (%.0f%%)", XPComplexResolved, grounding*100))
	}

	// Perfect grounding bonus
	if grounding >= 0.95 {
		totalXP += XPGroundingPerfect
		reasons = append(reasons, fmt.Sprintf("+%d Grounding perfecto (%.0f%%)", XPGroundingPerfect, grounding*100))
	}

	if totalXP > 0 {
		ps.AddXP(totalXP, fmt.Sprintf("Misión: %s", reasons[0]))
	}

	return totalXP, reasons
}

// CheckAchievements evaluates and grants any newly earned achievements.
func (ps *PlayerState) CheckAchievements() []Achievement {
	ps.mu.Lock()
	defer ps.mu.Unlock()

	earned := make(map[string]bool)
	for _, a := range ps.Achievements {
		earned[a.ID] = true
	}

	var newAchievements []Achievement

	checks := []struct {
		ID          string
		Name        string
		Description string
		Emoji       string
		Condition   func() bool
	}{
		{"first_mission", "Primera Misión", "Completa tu primera tarea", "🎯", func() bool { return ps.TotalMissions >= 1 }},
		{"10_missions", "Veterano", "Completa 10 misiones", "⭐", func() bool { return ps.TotalMissions >= 10 }},
		{"50_missions", "Experimentado", "Completa 50 misiones", "🌟", func() bool { return ps.TotalMissions >= 50 }},
		{"100_missions", "Centurión", "Completa 100 misiones", "💫", func() bool { return ps.TotalMissions >= 100 }},
		{"500_missions", "Leyenda", "Completa 500 misiones", "👑", func() bool { return ps.TotalMissions >= 500 }},
		{"first_cascade", "Primer Cascade", "Tu primer cascade exitoso", "⚡", func() bool {
			for _, a := range ps.Agents {
				if a.CascadesAvoided > 0 {
					return true
				}
			}
			return false
		}},
		{"saver_1", "Ahorrador", "Ahorra $1 con cascades", "💰", func() bool { return ps.TotalSavedUSD >= 1.0 }},
		{"saver_5", "Ahorrador de Oro", "Ahorra $5 con cascades", "💎", func() bool { return ps.TotalSavedUSD >= 5.0 }},
		{"saver_25", "Ahorrador Legendario", "Ahorra $25 con cascades", "🏆", func() bool { return ps.TotalSavedUSD >= 25.0 }},
		{"streak_3", "Constante", "Racha de 3 días", "🔥", func() bool { return ps.Streak >= 3 }},
		{"streak_7", "Imparable", "Racha de 7 días", "🔥🔥", func() bool { return ps.Streak >= 7 }},
		{"streak_30", "Máquina", "Racha de 30 días", "🔥🔥🔥", func() bool { return ps.Streak >= 30 }},
		{"high_ground", "Alta Confianza", "Grounding medio >90%", "🎯", func() bool { return ps.MeanGrounding >= 0.90 && ps.TotalMissions >= 10 }},
		{"level_5", "Constructor", "Alcanza nivel 5", "🏗️", func() bool { return ps.Level >= 5 }},
		{"level_10", "Arquitecto", "Alcanza nivel 10", "🏛️", func() bool { return ps.Level >= 10 }},
		{"level_20", "Maestro", "Alcanza nivel 20", "⚔️", func() bool { return ps.Level >= 20 }},
		{"level_35", "Comandante", "Alcanza nivel 35", "🎖️", func() bool { return ps.Level >= 35 }},
		{"level_50", "Leyenda", "Alcanza nivel 50", "👑", func() bool { return ps.Level >= 50 }},
	}

	for _, check := range checks {
		if !earned[check.ID] && check.Condition() {
			achievement := Achievement{
				ID:          check.ID,
				Name:        check.Name,
				Description: check.Description,
				Emoji:       check.Emoji,
				EarnedAt:    time.Now(),
			}
			ps.Achievements = append(ps.Achievements, achievement)
			newAchievements = append(newAchievements, achievement)
		}
	}

	return newAchievements
}

// defaultAgentProfile returns display metadata for known models.
func defaultAgentProfile(modelName string) *AgentStats {
	profiles := map[string]*AgentStats{
		"gemini-flash": {
			Name:        "gemini-flash",
			DisplayName: "Flash",
			Emoji:       "⚡",
			Specialties: []string{"planning", "sql", "docs", "simple_tasks"},
		},
		"claude-sonnet": {
			Name:        "claude-sonnet",
			DisplayName: "Sonnet",
			Emoji:       "🗡️",
			Specialties: []string{"coding", "tests", "frontend", "backend"},
		},
		"claude-opus": {
			Name:        "claude-opus",
			DisplayName: "Opus",
			Emoji:       "👑",
			Specialties: []string{"complex_reasoning", "architecture", "debugging"},
		},
		"gpt-4o-mini": {
			Name:        "gpt-4o-mini",
			DisplayName: "Mini",
			Emoji:       "🔮",
			Specialties: []string{"formatting", "translations", "simple_coding"},
		},
	}

	if profile, ok := profiles[modelName]; ok {
		return profile
	}
	return &AgentStats{
		Name:        modelName,
		DisplayName: modelName,
		Emoji:       "🤖",
	}
}
