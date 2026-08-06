package orchestrator

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// TournamentRecord represents one logged task execution record in routing_history.jsonl.
type TournamentRecord struct {
	Timestamp    time.Time  `json:"timestamp"`
	PlanID       string     `json:"plan_id"`
	TaskID       string     `json:"task_id"`
	Kind         TaskKind   `json:"kind"`
	Complexity   Complexity `json:"complexity"`
	Model        string     `json:"model"`
	Provider     string     `json:"provider"`
	Grounding    float64    `json:"grounding"`
	CostUSD      float64    `json:"cost_usd"`
	DurationMs   int64      `json:"duration_ms"`
	CascadeLevel int        `json:"cascade_level"`
	Accepted     bool       `json:"accepted"`
}

// ModelPerformance aggregates empirical execution stats for a model under a specific task kind.
type ModelPerformance struct {
	Model           string  `json:"model"`
	Provider        string  `json:"provider"`
	TotalRuns       int     `json:"total_runs"`
	Wins            int     `json:"wins"`           // grounding >= threshold
	WinRate         float64 `json:"win_rate"`       // 0.0 - 1.0
	MeanGrounding   float64 `json:"mean_grounding"` // 0.0 - 1.0
	MeanCostUSD     float64 `json:"mean_cost_usd"`
	MeanDurationMs  float64 `json:"mean_duration_ms"`
	EfficiencyScore float64 `json:"efficiency_score"` // grounding / cost ratio
}

// TournamentAnalysis summarizes routing recommendations across task kinds.
type TournamentAnalysis struct {
	TotalRecords    int                           `json:"total_records"`
	AnalyzedAt      time.Time                     `json:"analyzed_at"`
	ByKind          map[string][]ModelPerformance `json:"by_kind"`
	Recommendations map[string]string             `json:"recommendations"` // kind -> recommended model
}

// TournamentLogger logs execution records to routing_history.jsonl.
type TournamentLogger struct {
	mu   sync.Mutex
	path string
}

// NewTournamentLogger initializes the routing_history.jsonl logger in the given directory.
func NewTournamentLogger(dir string) *TournamentLogger {
	if dir == "" {
		if home, err := os.UserHomeDir(); err == nil {
			dir = filepath.Join(home, ".neurofs")
		} else {
			dir = "."
		}
	}
	_ = os.MkdirAll(dir, 0o755)
	return &TournamentLogger{
		path: filepath.Join(dir, "routing_history.jsonl"),
	}
}

// LogRecord appends one execution record to routing_history.jsonl.
func (tl *TournamentLogger) LogRecord(rec TournamentRecord) error {
	if tl == nil || tl.path == "" {
		return nil
	}
	tl.mu.Lock()
	defer tl.mu.Unlock()

	if rec.Timestamp.IsZero() {
		rec.Timestamp = time.Now()
	}

	data, err := json.Marshal(rec)
	if err != nil {
		return err
	}

	f, err := os.OpenFile(tl.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()

	_, err = f.Write(append(data, '\n'))
	return err
}

// AnalyzeTournament reads routing_history.jsonl and computes performance & routing recommendations.
func AnalyzeTournament(path string, groundingThreshold float64) (TournamentAnalysis, error) {
	if groundingThreshold <= 0 {
		groundingThreshold = 0.85
	}

	analysis := TournamentAnalysis{
		AnalyzedAt:      time.Now(),
		ByKind:          make(map[string][]ModelPerformance),
		Recommendations: make(map[string]string),
	}

	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return analysis, nil
		}
		return analysis, fmt.Errorf("open routing_history: %w", err)
	}
	defer f.Close()

	type stats struct {
		model        string
		provider     string
		runs         int
		wins         int
		groundingSum float64
		costSum      float64
		durationSum  int64
	}

	kindMap := make(map[string]map[string]*stats)

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if len(line) == 0 {
			continue
		}
		var rec TournamentRecord
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			continue
		}

		analysis.TotalRecords++

		kindKey := string(rec.Kind)
		if kindKey == "" {
			kindKey = "general"
		}

		if _, ok := kindMap[kindKey]; !ok {
			kindMap[kindKey] = make(map[string]*stats)
		}

		mStats, ok := kindMap[kindKey][rec.Model]
		if !ok {
			mStats = &stats{model: rec.Model, provider: rec.Provider}
			kindMap[kindKey][rec.Model] = mStats
		}

		mStats.runs++
		if rec.Grounding >= groundingThreshold {
			mStats.wins++
		}
		mStats.groundingSum += rec.Grounding
		mStats.costSum += rec.CostUSD
		mStats.durationSum += rec.DurationMs
	}

	for kind, models := range kindMap {
		var perfList []ModelPerformance
		for _, s := range models {
			if s.runs == 0 {
				continue
			}
			winRate := float64(s.wins) / float64(s.runs)
			meanG := s.groundingSum / float64(s.runs)
			meanCost := s.costSum / float64(s.runs)
			meanDur := float64(s.durationSum) / float64(s.runs)

			effScore := winRate / (meanCost + 0.001)

			perfList = append(perfList, ModelPerformance{
				Model:           s.model,
				Provider:        s.provider,
				TotalRuns:       s.runs,
				Wins:            s.wins,
				WinRate:         winRate,
				MeanGrounding:   meanG,
				MeanCostUSD:     meanCost,
				MeanDurationMs:  meanDur,
				EfficiencyScore: effScore,
			})
		}

		sort.Slice(perfList, func(i, j int) bool {
			if (perfList[i].WinRate >= 0.85) != (perfList[j].WinRate >= 0.85) {
				return perfList[i].WinRate >= 0.85
			}
			return perfList[i].EfficiencyScore > perfList[j].EfficiencyScore
		})

		analysis.ByKind[kind] = perfList

		if len(perfList) > 0 {
			analysis.Recommendations[kind] = perfList[0].Model
		}
	}

	return analysis, nil
}
