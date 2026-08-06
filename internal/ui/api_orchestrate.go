package ui

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync"

	"github.com/Gere2/neurofs/internal/config"
	"github.com/Gere2/neurofs/internal/gamestate"
	"github.com/Gere2/neurofs/internal/orchestrator"
)

type orchestrateRequest struct {
	Question string `json:"question"`
	Repo     string `json:"repo"`
}

type activeRun struct {
	Result    *orchestrator.Result
	Listeners []chan orchestrator.StatusEvent
	Mu        sync.Mutex
}

var (
	orchestrateRuns   = make(map[string]*activeRun)
	orchestrateRunsMu sync.RWMutex
)

func handleOrchestrateRun(w http.ResponseWriter, r *http.Request) {
	var req orchestrateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request json: "+err.Error())
		return
	}

	cfg, ok := mustRepo(w, req.Repo)
	if !ok {
		return
	}
	repo := cfg.RepoRoot

	orc, err := orchestrator.NewOrchestrator(repo, nil)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to init orchestrator: "+err.Error())
		return
	}

	run := &activeRun{}
	
	// Create context with background execution
	ctx := context.Background()

	// Decompose initial plan synchronously so the client immediately gets task list
	initialSearch := req.Question
	plan, err := orc.Decomposer.Decompose(ctx, req.Question, repo, initialSearch)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to decompose question: "+err.Error())
		return
	}

	runID := plan.ID
	orchestrateRunsMu.Lock()
	orchestrateRuns[runID] = run
	orchestrateRunsMu.Unlock()

	// Launch async execution
	go func() {
		res, _ := orc.Run(ctx, orchestrator.OrchestrationOptions{
			Question: req.Question,
			RepoRoot: repo,
			Callback: func(ev orchestrator.StatusEvent) {
				run.Mu.Lock()
				defer run.Mu.Unlock()
				for _, ch := range run.Listeners {
					select {
					case ch <- ev:
					default:
					}
				}
			},
		})

		// Update game state & log tournament records
		if home, err := os.UserHomeDir(); err == nil {
			dir := filepath.Join(home, ".neurofs")
			tLogger := orchestrator.NewTournamentLogger(dir)

			if ps, err := gamestate.Load(dir); err == nil {
				for _, t := range res.Plan.Tasks {
					if t.Status == orchestrator.StatusDone {
						ps.RecordTaskResult(t.Model, t.Grounding, t.CostUSD, t.CascadeLevel, t.CascadeSaved, t.Complexity == orchestrator.Complex, 0.85)
						ps.GrantXPForTask(t.Grounding, t.CascadeLevel, t.CascadeSaved, t.Complexity == orchestrator.Complex, 0.85)

						_ = tLogger.LogRecord(orchestrator.TournamentRecord{
							PlanID:       res.Plan.ID,
							TaskID:       t.ID,
							Kind:         t.Kind,
							Complexity:   t.Complexity,
							Model:        t.Model,
							Provider:     t.Provider,
							Grounding:    t.Grounding,
							CostUSD:      t.CostUSD,
							DurationMs:   t.Duration().Milliseconds(),
							CascadeLevel: t.CascadeLevel,
							Accepted:     true,
						})
					}
				}
				ps.CheckAchievements()
				_ = ps.Save(dir)
			}
		}
	}()

	writeJSON(w, http.StatusOK, map[string]any{
		"run_id": runID,
		"plan":   plan,
	})
}

func handleOrchestrateStream(w http.ResponseWriter, r *http.Request) {
	runID := r.URL.Query().Get("id")
	if runID == "" {
		writeErr(w, http.StatusBadRequest, "missing run id")
		return
	}

	orchestrateRunsMu.RLock()
	run, ok := orchestrateRuns[runID]
	orchestrateRunsMu.RUnlock()

	if !ok {
		writeErr(w, http.StatusNotFound, "run id not found")
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	ch := make(chan orchestrator.StatusEvent, 32)

	run.Mu.Lock()
	run.Listeners = append(run.Listeners, ch)
	run.Mu.Unlock()

	defer func() {
		run.Mu.Lock()
		for i, listener := range run.Listeners {
			if listener == ch {
				run.Listeners = append(run.Listeners[:i], run.Listeners[i+1:]...)
				break
			}
		}
		run.Mu.Unlock()
	}()

	notify := r.Context().Done()
	for {
		select {
		case <-notify:
			return
		case ev, open := <-ch:
			if !open {
				return
			}
			data, err := json.Marshal(ev)
			if err != nil {
				continue
			}
			fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
		}
	}
}

func handleOrchestrateModels(w http.ResponseWriter, r *http.Request) {
	repoParam := r.URL.Query().Get("repo")
	repo := repoParam
	if c, err := config.New(repoParam); err == nil {
		repo = c.RepoRoot
	}

	if r.Method == http.MethodGet {
		cfg, err := orchestrator.LoadModelsConfig(repo)
		if err != nil {
			cfg = orchestrator.DefaultModelsConfig()
		}
		writeJSON(w, http.StatusOK, cfg)
		return
	}

	if r.Method == http.MethodPost {
		var cfg orchestrator.ModelsConfig
		if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
			writeErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
			return
		}
		// Save custom config to repo
		if err := orchestrator.WriteDefaultConfig(repo); err != nil {
			writeErr(w, http.StatusInternalServerError, "failed to save config: "+err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "saved"})
		return
	}

	writeErr(w, http.StatusMethodNotAllowed, "GET or POST required")
}

type nodeControlRequest struct {
	RunID  string `json:"run_id"`
	TaskID string `json:"task_id"`
	Action string `json:"action"` // "override_model", "rerun", "skip"
	Model  string `json:"model,omitempty"`
}

func handleOrchestrateNodeControl(w http.ResponseWriter, r *http.Request) {
	var req nodeControlRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}

	orchestrateRunsMu.RLock()
	run, ok := orchestrateRuns[req.RunID]
	orchestrateRunsMu.RUnlock()

	if !ok {
		writeErr(w, http.StatusNotFound, "run id not found")
		return
	}

	ev := orchestrator.StatusEvent{
		TaskID: req.TaskID,
		Model:  req.Model,
	}

	switch req.Action {
	case "override_model":
		ev.Status = orchestrator.StatusPending
	case "skip":
		ev.Status = orchestrator.StatusSkipped
		ev.Error = "manually skipped by user"
	case "rerun":
		ev.Status = orchestrator.StatusRunning
	default:
		writeErr(w, http.StatusBadRequest, "unknown action: "+req.Action)
		return
	}

	run.Mu.Lock()
	for _, ch := range run.Listeners {
		select {
		case ch <- ev:
		default:
		}
	}
	run.Mu.Unlock()

	writeJSON(w, http.StatusOK, map[string]string{"status": "applied", "action": req.Action})
}

func handleOrchestrateTournament(w http.ResponseWriter, r *http.Request) {
	home, err := os.UserHomeDir()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "cannot resolve home dir: "+err.Error())
		return
	}
	logPath := filepath.Join(home, ".neurofs", "routing_history.jsonl")
	analysis, err := orchestrator.AnalyzeTournament(logPath, 0.85)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to analyze tournament: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, analysis)
}

func handleOrchestrateTune(w http.ResponseWriter, r *http.Request) {
	repoParam := r.URL.Query().Get("repo")
	repo := repoParam
	if c, err := config.New(repoParam); err == nil {
		repo = c.RepoRoot
	}

	tuner := &orchestrator.HyperAgentTuner{RepoRoot: repo}
	res, err := tuner.AutoTune(repo, 3)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "autotune failed: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}


