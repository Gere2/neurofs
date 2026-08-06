package orchestrator

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// cascadeMockClient simulates LLM responses with controllable grounding.
// It returns responses that can be tuned to produce specific grounding scores.
type cascadeMockClient struct {
	// responses maps model name → response text
	responses map[string]string
	// callOrder records the order of model calls for verification
	callOrder []string
}

func (c *cascadeMockClient) Complete(ctx context.Context, entry ModelEntry, prompt string) (string, int, int, error) {
	c.callOrder = append(c.callOrder, entry.ModelID)
	resp, ok := c.responses[entry.ModelID]
	if !ok {
		resp = fmt.Sprintf("Generic response from %s", entry.ModelID)
	}
	return resp, 100, 50, nil
}

func TestCascade_EscalatesOnLowGrounding(t *testing.T) {
	cfg := DefaultModelsConfig()
	cfg.Cascade = CascadeConfig{
		Enabled:            true,
		GroundingThreshold: 0.80,
		MaxAttempts:        3,
		EscalationChain:    []string{"gemini-flash", "claude-sonnet", "claude-opus"},
	}

	client := &cascadeMockClient{
		responses: map[string]string{
			"gemini-2.5-flash-preview-05-20": "irrelevant response with no context words",
			"claude-sonnet-4-6":              "irrelevant response still bad grounding",
			"claude-opus-4-0725":             "this is the final attempt response",
		},
	}

	router := NewRouter(cfg)
	dispatcher := NewDispatcher(router, client)

	task := &Task{
		ID:          "t1",
		Description: "Implement auth system",
		Kind:        KindBackend,
		Complexity:  Medium,
		Context:     "File auth.go contains user authentication logic with JWT tokens and bcrypt hashing",
		Status:      StatusPending,
	}

	// Grounding function that returns low scores for Flash/Sonnet, high for Opus
	groundFn := func(response, ctx string) float64 {
		if strings.Contains(response, "final attempt") {
			return 0.95
		}
		return 0.30 // below threshold → escalate
	}

	err := dispatcher.dispatchWithCascade(context.Background(), task, cfg.Cascade, groundFn, nil)
	if err != nil {
		t.Fatalf("cascade dispatch failed: %v", err)
	}

	// Should have tried all 3 models
	if len(client.callOrder) != 3 {
		t.Errorf("expected 3 model calls, got %d: %v", len(client.callOrder), client.callOrder)
	}

	// Final model should be Opus
	if task.Model != "claude-opus" {
		t.Errorf("expected final model claude-opus, got %s", task.Model)
	}

	// Should have 3 cascade attempts
	if len(task.CascadeAttempts) != 3 {
		t.Errorf("expected 3 cascade attempts, got %d", len(task.CascadeAttempts))
	}

	// First two should not be accepted, last should be
	for i, a := range task.CascadeAttempts {
		if i < 2 && a.Accepted {
			t.Errorf("attempt %d should not be accepted", i)
		}
		if i == 2 && !a.Accepted {
			t.Errorf("attempt %d should be accepted", i)
		}
	}

	// Grounding should be high (from Opus)
	if task.Grounding < 0.90 {
		t.Errorf("expected grounding >= 0.90, got %.2f", task.Grounding)
	}

	// Cascade level should be 2 (0-indexed)
	if task.CascadeLevel != 2 {
		t.Errorf("expected cascade level 2, got %d", task.CascadeLevel)
	}

	// Status should be done
	if task.Status != StatusDone {
		t.Errorf("expected status done, got %s", task.Status)
	}
}

func TestCascade_AcceptsFirstModelWhenGroundingHigh(t *testing.T) {
	cfg := DefaultModelsConfig()
	cfg.Cascade = CascadeConfig{
		Enabled:            true,
		GroundingThreshold: 0.80,
		MaxAttempts:        3,
		EscalationChain:    []string{"gemini-flash", "claude-sonnet", "claude-opus"},
	}

	client := &cascadeMockClient{
		responses: map[string]string{
			"gemini-2.5-flash-preview-05-20": "great response with auth.go JWT tokens bcrypt",
		},
	}

	router := NewRouter(cfg)
	dispatcher := NewDispatcher(router, client)

	task := &Task{
		ID:          "t1",
		Description: "Implement auth system",
		Kind:        KindBackend,
		Complexity:  Medium,
		Context:     "File auth.go contains user authentication logic with JWT tokens and bcrypt hashing",
		Status:      StatusPending,
	}

	// High grounding for all responses
	groundFn := func(response, ctx string) float64 {
		return 0.95
	}

	err := dispatcher.dispatchWithCascade(context.Background(), task, cfg.Cascade, groundFn, nil)
	if err != nil {
		t.Fatalf("cascade dispatch failed: %v", err)
	}

	// Should have called only the first (cheapest) model
	if len(client.callOrder) != 1 {
		t.Errorf("expected 1 model call (no escalation), got %d: %v", len(client.callOrder), client.callOrder)
	}

	// Model should be Flash
	if task.Model != "gemini-flash" {
		t.Errorf("expected model gemini-flash, got %s", task.Model)
	}

	// Only 1 cascade attempt
	if len(task.CascadeAttempts) != 1 {
		t.Errorf("expected 1 cascade attempt, got %d", len(task.CascadeAttempts))
	}

	// CascadeLevel should be 0
	if task.CascadeLevel != 0 {
		t.Errorf("expected cascade level 0, got %d", task.CascadeLevel)
	}

	// Cost saved should be positive (Flash is cheaper than Opus)
	if task.CascadeSaved <= 0 {
		t.Errorf("expected positive cost savings, got %.6f", task.CascadeSaved)
	}
}

func TestCascade_DisabledFallsBackToSingleShot(t *testing.T) {
	cfg := DefaultModelsConfig()
	cfg.Cascade.Enabled = false

	client := &cascadeMockClient{
		responses: map[string]string{
			"claude-sonnet-4-6": "sonnet response",
		},
	}

	router := NewRouter(cfg)
	dispatcher := NewDispatcher(router, client)

	task := &Task{
		ID:          "t1",
		Description: "Simple task",
		Kind:        KindBackend,
		Complexity:  Medium,
		Context:     "some context",
		Status:      StatusPending,
		Model:       "claude-sonnet",
		Provider:    "anthropic",
	}

	err := dispatcher.dispatchWithCascade(context.Background(), task, cfg.Cascade, nil, nil)
	if err != nil {
		t.Fatalf("single-shot dispatch failed: %v", err)
	}

	// Should have called only one model (via single-shot)
	if len(client.callOrder) != 1 {
		t.Errorf("expected 1 model call, got %d", len(client.callOrder))
	}

	// No cascade attempts
	if len(task.CascadeAttempts) != 0 {
		t.Errorf("expected 0 cascade attempts with cascade disabled, got %d", len(task.CascadeAttempts))
	}

	if task.Status != StatusDone {
		t.Errorf("expected status done, got %s", task.Status)
	}
}

func TestCascade_CallbacksEmitted(t *testing.T) {
	cfg := DefaultModelsConfig()
	cfg.Cascade = CascadeConfig{
		Enabled:            true,
		GroundingThreshold: 0.80,
		MaxAttempts:        2,
		EscalationChain:    []string{"gemini-flash", "claude-sonnet"},
	}

	client := &cascadeMockClient{
		responses: map[string]string{
			"gemini-2.5-flash-preview-05-20": "weak response",
			"claude-sonnet-4-6":              "strong response with context words",
		},
	}

	router := NewRouter(cfg)
	dispatcher := NewDispatcher(router, client)

	task := &Task{
		ID:          "t1",
		Description: "Build feature",
		Kind:        KindBackend,
		Complexity:  Medium,
		Context:     "context here",
		Status:      StatusPending,
	}

	var events []StatusEvent
	cb := func(ev StatusEvent) {
		events = append(events, ev)
	}

	groundFn := func(response, ctx string) float64 {
		if strings.Contains(response, "strong") {
			return 0.90
		}
		return 0.40
	}

	err := dispatcher.dispatchWithCascade(context.Background(), task, cfg.Cascade, groundFn, cb)
	if err != nil {
		t.Fatalf("cascade dispatch failed: %v", err)
	}

	// Should have emitted multiple events: running(flash), escalation, running(sonnet), done
	if len(events) < 3 {
		t.Errorf("expected at least 3 events, got %d", len(events))
	}

	// Last event should be Done
	lastEvent := events[len(events)-1]
	if lastEvent.Status != StatusDone {
		t.Errorf("expected last event status Done, got %s", lastEvent.Status)
	}

	// At least one event should have cascade info
	hasCascadeInfo := false
	for _, ev := range events {
		if ev.CascadeLevel > 0 || ev.CascadeReason != "" {
			hasCascadeInfo = true
			break
		}
	}
	if !hasCascadeInfo {
		t.Error("expected at least one event with cascade info")
	}
}

func TestCascade_CostSavingsCalculation(t *testing.T) {
	cfg := DefaultModelsConfig()
	cfg.Cascade = CascadeConfig{
		Enabled:            true,
		GroundingThreshold: 0.80,
		MaxAttempts:        3,
		EscalationChain:    []string{"gemini-flash", "claude-sonnet", "claude-opus"},
	}

	client := &cascadeMockClient{
		responses: map[string]string{
			"gemini-2.5-flash-preview-05-20": "good context-grounded response",
		},
	}

	router := NewRouter(cfg)
	dispatcher := NewDispatcher(router, client)

	plan := &Plan{
		ID:       "test-plan",
		Question: "test",
		Tasks: []Task{
			{
				ID:          "t1",
				Description: "Task one",
				Kind:        KindBackend,
				Complexity:  Simple,
				Context:     "context data",
			},
			{
				ID:          "t2",
				Description: "Task two",
				Kind:        KindDatabase,
				Complexity:  Simple,
				Context:     "db context",
			},
		},
	}

	groundFn := func(response, ctx string) float64 {
		return 0.95 // always high → always accept first (cheapest) model
	}

	err := dispatcher.DispatchWithCascade(context.Background(), plan, cfg.Cascade, groundFn, nil)
	if err != nil {
		t.Fatalf("dispatch failed: %v", err)
	}

	// Both tasks should be done with Flash
	for _, task := range plan.Tasks {
		if task.Status != StatusDone {
			t.Errorf("task %s: expected done, got %s", task.ID, task.Status)
		}
		if task.Model != "gemini-flash" {
			t.Errorf("task %s: expected gemini-flash, got %s", task.ID, task.Model)
		}
		if task.CascadeSaved <= 0 {
			t.Errorf("task %s: expected positive savings, got %.6f", task.ID, task.CascadeSaved)
		}
	}

	if plan.Status != StatusDone {
		t.Errorf("expected plan status done, got %s", plan.Status)
	}
}
