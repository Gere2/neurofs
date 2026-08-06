package orchestrator

import (
	"context"
	"fmt"
	"time"
)

// dispatchWithCascade executes a single task using speculative execution:
// it starts with the cheapest viable model, checks grounding, and only
// escalates to a more powerful (expensive) model if grounding is below
// the configured threshold. Each attempt enriches the prompt with the
// previous attempt's failure analysis.
//
// If cascade is disabled or the chain is empty, it falls back to
// single-shot dispatch using the router's selection.
func (d *Dispatcher) dispatchWithCascade(
	ctx context.Context,
	t *Task,
	cascadeCfg CascadeConfig,
	groundFn func(response, context string) float64,
	cb ProgressCallback,
) error {
	if !cascadeCfg.Enabled || len(cascadeCfg.EscalationChain) == 0 {
		return d.dispatchSingle(ctx, t, cb)
	}

	maxAttempts := cascadeCfg.MaxAttempts
	if maxAttempts <= 0 || maxAttempts > len(cascadeCfg.EscalationChain) {
		maxAttempts = len(cascadeCfg.EscalationChain)
	}

	var attempts []CascadeAttempt
	var lastResponse string
	var lastGrounding float64
	var totalCascadeCost float64

	for level := 0; level < maxAttempts; level++ {
		select {
		case <-ctx.Done():
			t.Status = StatusFailed
			t.Error = ctx.Err().Error()
			return ctx.Err()
		default:
		}

		modelName := cascadeCfg.EscalationChain[level]
		entry, ok := d.Router.Config.Models[modelName]
		if !ok {
			// Skip unknown models in chain
			continue
		}

		// Build prompt — enrich with failure analysis on escalation
		prompt := t.Description
		if t.Context != "" {
			prompt = fmt.Sprintf("Context:\n%s\n\nTask:\n%s", t.Context, t.Description)
		}
		if level > 0 && lastResponse != "" {
			prompt = fmt.Sprintf("%s\n\n---\nPrevious attempt (model: %s, grounding: %.0f%%) produced an insufficient response. "+
				"The response was not well-grounded in the provided context. Please provide a more accurate, "+
				"context-grounded response.\n\nPrevious response for reference:\n%s",
				prompt,
				attempts[level-1].Model,
				lastGrounding*100,
				truncate(lastResponse, 1500),
			)
		}

		t.Prompt = prompt
		t.Model = modelName
		t.Provider = entry.Provider
		t.CascadeLevel = level

		// Emit running event with cascade info
		now := time.Now()
		if level == 0 {
			t.StartedAt = &now
		}
		t.Status = StatusRunning

		if cb != nil {
			cb(StatusEvent{
				TaskID:        t.ID,
				Status:        StatusRunning,
				Model:         modelName,
				Provider:      entry.Provider,
				CascadeLevel:  level,
				CascadeReason: cascadeRunningReason(level),
			})
		}

		// Call LLM
		response, inTokens, outTokens, err := d.LLMClient.Complete(ctx, entry, prompt)
		attemptEnd := time.Now()
		attemptDuration := attemptEnd.Sub(now)
		attemptCost := entry.EstimateCost(inTokens, outTokens)
		totalCascadeCost += attemptCost

		if err != nil {
			attempts = append(attempts, CascadeAttempt{
				Level:     level,
				Model:     modelName,
				Provider:  entry.Provider,
				CostUSD:   attemptCost,
				DurationMs: attemptDuration.Milliseconds(),
				Accepted:  false,
				Reason:    fmt.Sprintf("error: %s", err.Error()),
			})
			// On error, try next model in chain
			if cb != nil {
				cb(StatusEvent{
					TaskID:        t.ID,
					Status:        StatusRunning,
					Model:         modelName,
					Provider:      entry.Provider,
					CascadeLevel:  level,
					CascadeReason: fmt.Sprintf("escalated: error %s", truncate(err.Error(), 80)),
					Error:         err.Error(),
				})
			}
			continue
		}

		// Calculate grounding
		grounding := 0.0
		if t.Context != "" && response != "" {
			grounding = groundFn(response, t.Context)
		}

		accepted := grounding >= cascadeCfg.GroundingThreshold || level == maxAttempts-1
		reason := ""
		if accepted {
			if grounding >= cascadeCfg.GroundingThreshold {
				reason = fmt.Sprintf("accepted: grounding %.0f%% >= %.0f%% threshold", grounding*100, cascadeCfg.GroundingThreshold*100)
			} else {
				reason = fmt.Sprintf("accepted: max cascade depth reached (grounding %.0f%%)", grounding*100)
			}
		} else {
			reason = fmt.Sprintf("escalated: grounding %.0f%% < %.0f%% threshold", grounding*100, cascadeCfg.GroundingThreshold*100)
		}

		attempt := CascadeAttempt{
			Level:        level,
			Model:        modelName,
			Provider:     entry.Provider,
			Response:     response,
			Grounding:    grounding,
			CostUSD:      attemptCost,
			InputTokens:  inTokens,
			OutputTokens: outTokens,
			DurationMs:   attemptDuration.Milliseconds(),
			Accepted:     accepted,
			Reason:       reason,
		}
		attempts = append(attempts, attempt)

		if accepted {
			// This attempt passed — use it
			finished := time.Now()
			t.FinishedAt = &finished
			t.Status = StatusDone
			t.Response = response
			t.InputTokens = inTokens
			t.OutputTokens = outTokens
			t.CostUSD = totalCascadeCost
			t.Grounding = grounding
			t.CascadeLevel = level
			t.CascadeAttempts = attempts

			// Calculate savings: cost of running top-tier model minus actual cascade cost
			topModel := cascadeCfg.EscalationChain[len(cascadeCfg.EscalationChain)-1]
			if topEntry, ok := d.Router.Config.Models[topModel]; ok && level < len(cascadeCfg.EscalationChain)-1 {
				hypotheticalCost := topEntry.EstimateCost(inTokens, outTokens)
				if hypotheticalCost > totalCascadeCost {
					t.CascadeSaved = hypotheticalCost - totalCascadeCost
				}
			}

			if cb != nil {
				cb(StatusEvent{
					TaskID:        t.ID,
					Status:        StatusDone,
					Model:         modelName,
					Provider:      entry.Provider,
					InputTokens:   inTokens,
					OutputTokens:  outTokens,
					CostUSD:       totalCascadeCost,
					Grounding:     grounding,
					DurationMs:    t.Duration().Milliseconds(),
					Response:      response,
					CascadeLevel:  level,
					CascadeReason: reason,
				})
			}
			return nil
		}

		// Not accepted — emit escalation event and try next
		lastResponse = response
		lastGrounding = grounding

		if cb != nil {
			cb(StatusEvent{
				TaskID:        t.ID,
				Status:        StatusRunning,
				Model:         modelName,
				Provider:      entry.Provider,
				Grounding:     grounding,
				CostUSD:       attemptCost,
				CascadeLevel:  level,
				CascadeReason: reason,
			})
		}
	}

	// Should not reach here — last attempt is always accepted
	// But handle defensively
	t.Status = StatusFailed
	t.Error = "cascade exhausted without acceptance"
	t.CascadeAttempts = attempts
	if cb != nil {
		cb(StatusEvent{
			TaskID: t.ID,
			Status: StatusFailed,
			Error:  t.Error,
		})
	}
	return fmt.Errorf("cascade exhausted for task %s", t.ID)
}

// dispatchSingle is the original single-shot dispatch (no cascade).
func (d *Dispatcher) dispatchSingle(ctx context.Context, t *Task, cb ProgressCallback) error {
	resolved, err := d.Router.SelectModel(*t)
	if err != nil {
		t.Status = StatusFailed
		t.Error = err.Error()
		if cb != nil {
			cb(StatusEvent{TaskID: t.ID, Status: StatusFailed, Error: t.Error})
		}
		return nil
	}
	t.Model = resolved.Name
	t.Provider = resolved.Entry.Provider

	t.Status = StatusRunning
	now := time.Now()
	t.StartedAt = &now
	if cb != nil {
		cb(StatusEvent{TaskID: t.ID, Status: StatusRunning, Model: t.Model, Provider: t.Provider})
	}

	prompt := t.Description
	if t.Context != "" {
		prompt = fmt.Sprintf("Context:\n%s\n\nTask:\n%s", t.Context, t.Description)
	}
	t.Prompt = prompt

	response, inTokens, outTokens, err := d.LLMClient.Complete(ctx, resolved.Entry, prompt)
	finished := time.Now()
	t.FinishedAt = &finished

	if err != nil {
		t.Status = StatusFailed
		t.Error = err.Error()
		if cb != nil {
			cb(StatusEvent{
				TaskID:     t.ID,
				Status:     StatusFailed,
				Model:      t.Model,
				Provider:   t.Provider,
				Error:      err.Error(),
				DurationMs: t.Duration().Milliseconds(),
			})
		}
		return nil
	}

	t.Status = StatusDone
	t.Response = response
	t.InputTokens = inTokens
	t.OutputTokens = outTokens
	t.CostUSD = resolved.Entry.EstimateCost(inTokens, outTokens)

	if cb != nil {
		cb(StatusEvent{
			TaskID:       t.ID,
			Status:       StatusDone,
			Model:        t.Model,
			Provider:     t.Provider,
			InputTokens:  t.InputTokens,
			OutputTokens: t.OutputTokens,
			CostUSD:      t.CostUSD,
			DurationMs:   t.Duration().Milliseconds(),
			Response:     t.Response,
		})
	}
	return nil
}

func cascadeRunningReason(level int) string {
	if level == 0 {
		return "speculative: trying cheapest model first"
	}
	return fmt.Sprintf("escalating: attempt %d with stronger model", level+1)
}
