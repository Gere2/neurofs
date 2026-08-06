package orchestrator

import (
	"path/filepath"
	"testing"
	"time"
)

func TestTournament_LoggerAndAnalyzer(t *testing.T) {
	dir := t.TempDir()
	logger := NewTournamentLogger(dir)

	// Log several execution records for different models
	records := []TournamentRecord{
		{
			Timestamp:    time.Now(),
			PlanID:       "plan-1",
			TaskID:       "t1",
			Kind:         KindBackend,
			Complexity:   Medium,
			Model:        "gemini-flash",
			Provider:     "google",
			Grounding:    0.92,
			CostUSD:      0.0005,
			DurationMs:   350,
			CascadeLevel: 0,
			Accepted:     true,
		},
		{
			Timestamp:    time.Now(),
			PlanID:       "plan-1",
			TaskID:       "t2",
			Kind:         KindBackend,
			Complexity:   Medium,
			Model:        "claude-sonnet",
			Provider:     "anthropic",
			Grounding:    0.95,
			CostUSD:      0.0120,
			DurationMs:   1200,
			CascadeLevel: 1,
			Accepted:     true,
		},
		{
			Timestamp:    time.Now(),
			PlanID:       "plan-2",
			TaskID:       "t3",
			Kind:         KindFrontend,
			Complexity:   Simple,
			Model:        "gemini-flash",
			Provider:     "google",
			Grounding:    0.88,
			CostUSD:      0.0003,
			DurationMs:   250,
			CascadeLevel: 0,
			Accepted:     true,
		},
	}

	for _, r := range records {
		if err := logger.LogRecord(r); err != nil {
			t.Fatalf("failed to log record: %v", err)
		}
	}

	// Analyze tournament log
	analysis, err := AnalyzeTournament(filepath.Join(dir, "routing_history.jsonl"), 0.85)
	if err != nil {
		t.Fatalf("failed to analyze tournament: %v", err)
	}

	if analysis.TotalRecords != 3 {
		t.Errorf("expected 3 total records, got %d", analysis.TotalRecords)
	}

	backendPerf, ok := analysis.ByKind["backend"]
	if !ok || len(backendPerf) == 0 {
		t.Fatal("expected backend performance data")
	}

	// Gemini Flash should be recommended for backend due to win rate >= 0.85 and higher cost efficiency
	recBackend, ok := analysis.Recommendations["backend"]
	if !ok {
		t.Fatal("expected recommendation for backend")
	}
	if recBackend != "gemini-flash" {
		t.Errorf("expected recommendation gemini-flash, got %s", recBackend)
	}
}
