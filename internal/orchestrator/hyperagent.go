package orchestrator

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// HyperAgentTuner analyzes execution history and automatically optimizes routing rules and cascade thresholds.
type HyperAgentTuner struct {
	mu       sync.Mutex
	RepoRoot string
}

// TuningResult reports the updates applied by the HyperAgent tuner.
type TuningResult struct {
	TunedAt           time.Time         `json:"tuned_at"`
	RulesUpdated      map[string]string `json:"rules_updated"`      // kind -> new model
	ThresholdAdjusted float64           `json:"threshold_adjusted"` // new grounding threshold
	InsightsGenerated []string          `json:"insights_generated"` // human-readable summary of learnings
}

// AutoTune analyzes routing_history.jsonl and applies optimal model assignments to models.json.
func (h *HyperAgentTuner) AutoTune(repoRoot string, minRunsPerKind int) (*TuningResult, error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if minRunsPerKind <= 0 {
		minRunsPerKind = 3
	}

	historyPaths := []string{}
	if repoRoot != "" {
		historyPaths = append(historyPaths,
			filepath.Join(repoRoot, ".neurofs", "routing_history.jsonl"),
			filepath.Join(repoRoot, "routing_history.jsonl"),
		)
	}
	if home, err := os.UserHomeDir(); err == nil {
		historyPaths = append(historyPaths, filepath.Join(home, ".neurofs", "routing_history.jsonl"))
	}

	var historyPath string
	for _, p := range historyPaths {
		if _, err := os.Stat(p); err == nil {
			historyPath = p
			break
		}
	}
	if historyPath == "" && len(historyPaths) > 0 {
		historyPath = historyPaths[len(historyPaths)-1]
	}

	analysis, err := AnalyzeTournament(historyPath, 0.85)
	if err != nil {
		return nil, fmt.Errorf("analyze tournament: %w", err)
	}

	cfg, err := LoadModelsConfig(repoRoot)
	if err != nil {
		cfg = DefaultModelsConfig()
	}

	result := &TuningResult{
		TunedAt:           time.Now(),
		RulesUpdated:      make(map[string]string),
		ThresholdAdjusted: cfg.Cascade.GroundingThreshold,
	}

	// Update routing rules based on tournament recommendations
	for kind, perfList := range analysis.ByKind {
		if len(perfList) == 0 {
			continue
		}

		bestModel := perfList[0]
		if bestModel.TotalRuns >= minRunsPerKind && bestModel.WinRate >= 0.85 {
			currentModel := cfg.Routing[kind]
			if currentModel != bestModel.Model {
				cfg.Routing[kind] = bestModel.Model
				result.RulesUpdated[kind] = bestModel.Model
				result.InsightsGenerated = append(result.InsightsGenerated,
					fmt.Sprintf("Routing for '%s' updated from '%s' to '%s' (WinRate: %.0f%%, Mean Cost: $%.4f)",
						kind, currentModel, bestModel.Model, bestModel.WinRate*100, bestModel.MeanCostUSD),
				)
			}
		}
	}

	// Adjust cascade threshold based on overall grounding performance
	var totalGrounding float64
	var totalRuns int
	for _, perfList := range analysis.ByKind {
		for _, p := range perfList {
			totalGrounding += p.MeanGrounding * float64(p.TotalRuns)
			totalRuns += p.TotalRuns
		}
	}

	if totalRuns >= 10 {
		avgGrounding := totalGrounding / float64(totalRuns)
		if avgGrounding >= 0.90 && cfg.Cascade.GroundingThreshold < 0.88 {
			cfg.Cascade.GroundingThreshold = 0.88
			result.ThresholdAdjusted = 0.88
			result.InsightsGenerated = append(result.InsightsGenerated, "Cascade threshold adjusted to 88% (overall high grounding average)")
		}
	}

	// Save tuned models.json if changes occurred
	if len(result.RulesUpdated) > 0 || result.ThresholdAdjusted != cfg.Cascade.GroundingThreshold {
		_ = WriteModelsConfig(repoRoot, cfg)
	}

	return result, nil
}
