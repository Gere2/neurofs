package orchestrator

import (
	"path/filepath"

	"github.com/Gere2/neurofs/internal/gamestate"
)

// acceptedGroundingFloor is the grounding level a task must clear to count as
// a win when scoring the player and the routing tournament.
const acceptedGroundingFloor = 0.85

// RecordPlanOutcome writes the empirical trace of a finished plan: the
// tournament ledger that HyperAgentTuner later retunes routing from, and the
// player's XP and achievements.
//
// Synthetic tasks are skipped. When no API key is set for the routed provider,
// DefaultLLMClient.Complete returns a placeholder string instead of failing,
// so a keyless run still produces a full plan of "done" tasks — with grounding
// scores, costs and cascade levels that describe the placeholder rather than
// any model. Recording those would put fiction in the ledger and let a later
// `AutoTune` rewrite models.json from it.
//
// dir is the NeuroFS home (~/.neurofs). Errors are returned rather than
// logged so the caller can decide whether a bookkeeping failure should
// surface; a nil PlayerState from a load failure is not fatal to the run.
func RecordPlanOutcome(dir string, plan Plan) error {
	ps, err := gamestate.Load(dir)
	if err != nil {
		return err
	}
	logger := NewTournamentLogger(dir)

	// Once per run, not per task: the streak measures days the player showed
	// up, so a 12-task plan is one day of activity, not twelve.
	ps.GrantStreakXP()

	recorded := 0
	for _, t := range plan.Tasks {
		if t.Status != StatusDone || t.Synthetic {
			continue
		}
		recorded++
		isComplex := t.Complexity == Complex
		ps.RecordTaskResult(t.Model, t.Grounding, t.CostUSD, t.CascadeLevel, t.CascadeSaved, isComplex, acceptedGroundingFloor)
		ps.GrantXPForTask(t.Grounding, t.CascadeLevel, t.CascadeSaved, isComplex, acceptedGroundingFloor)

		if err := logger.LogRecord(TournamentRecord{
			PlanID:       plan.ID,
			TaskID:       t.ID,
			Kind:         t.Kind,
			Complexity:   t.Complexity,
			Model:        t.Model,
			Provider:     t.Provider,
			Grounding:    t.Grounding,
			CostUSD:      t.CostUSD,
			DurationMs:   t.Duration().Milliseconds(),
			CascadeLevel: t.CascadeLevel,
			Accepted:     t.Grounding >= acceptedGroundingFloor,
		}); err != nil {
			return err
		}
	}

	// Project completion: every task in the plan finished, and every one of
	// them was real. A keyless run whose tasks all came back as placeholders
	// must not award the largest bonus in the table.
	if recorded > 0 && recorded == len(plan.Tasks) {
		ps.GrantProjectCompleteXP(plan.ID)
	}

	ps.CheckAchievements()
	return ps.Save(dir)
}

// SyntheticTaskCount reports how many completed tasks in the plan came from
// the offline mock. Callers surface it so a keyless run cannot be mistaken
// for a measured one.
func SyntheticTaskCount(plan Plan) int {
	n := 0
	for _, t := range plan.Tasks {
		if t.Synthetic {
			n++
		}
	}
	return n
}

// NeuroFSHome returns the ~/.neurofs directory used for cross-project state.
func NeuroFSHome(home string) string { return filepath.Join(home, ".neurofs") }
