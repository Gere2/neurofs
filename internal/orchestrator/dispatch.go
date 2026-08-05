package orchestrator

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// LLMClient abstracts calling raw provider APIs.
type LLMClient interface {
	Complete(ctx context.Context, entry ModelEntry, prompt string) (string, int, int, error)
}

// DefaultLLMClient implements LLMClient for Anthropic, Google (Gemini), and OpenAI.
type DefaultLLMClient struct {
	HTTPClient *http.Client
}

// NewDefaultLLMClient creates an LLMClient with a timeout.
func NewDefaultLLMClient(timeout time.Duration) *DefaultLLMClient {
	if timeout == 0 {
		timeout = 60 * time.Second
	}
	return &DefaultLLMClient{
		HTTPClient: &http.Client{Timeout: timeout},
	}
}

func (c *DefaultLLMClient) Complete(ctx context.Context, entry ModelEntry, prompt string) (string, int, int, error) {
	apiKey := entry.ResolveAPIKey()
	provider := strings.ToLower(entry.Provider)

	if apiKey == "" {
		// Mock response for offline/testing when no key is set
		mockText := fmt.Sprintf("[Mock %s Response for: %s]\nCompleted sub-task using %s.", entry.Provider, truncate(prompt, 60), entry.ModelID)
		return mockText, estimateTokens(prompt), estimateTokens(mockText), nil
	}

	switch provider {
	case "anthropic":
		return c.completeAnthropic(ctx, entry, apiKey, prompt)
	case "google", "gemini":
		return c.completeGemini(ctx, entry, apiKey, prompt)
	case "openai":
		return c.completeOpenAI(ctx, entry, apiKey, prompt)
	default:
		return "", 0, 0, fmt.Errorf("unsupported provider: %s", entry.Provider)
	}
}

func (c *DefaultLLMClient) completeAnthropic(ctx context.Context, entry ModelEntry, apiKey, prompt string) (string, int, int, error) {
	url := "https://api.anthropic.com/v1/messages"
	reqBody := map[string]any{
		"model":      entry.ModelID,
		"max_tokens": 4096,
		"messages": []map[string]string{
			{"role": "user", "content": prompt},
		},
	}
	data, _ := json.Marshal(reqBody)
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(data))
	if err != nil {
		return "", 0, 0, err
	}
	req.Header.Set("x-api-key", apiKey)
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("content-type", "application/json")

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return "", 0, 0, err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", 0, 0, fmt.Errorf("anthropic api error (%d): %s", resp.StatusCode, string(body))
	}

	var parsed struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
		Usage struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", 0, 0, err
	}

	text := ""
	if len(parsed.Content) > 0 {
		text = parsed.Content[0].Text
	}
	return text, parsed.Usage.InputTokens, parsed.Usage.OutputTokens, nil
}

func (c *DefaultLLMClient) completeGemini(ctx context.Context, entry ModelEntry, apiKey, prompt string) (string, int, int, error) {
	model := entry.ModelID
	if model == "" {
		model = "gemini-2.5-flash-preview-05-20"
	}
	url := fmt.Sprintf("https://generativelanguage.googleapis.com/v1beta/models/%s:generateContent?key=%s", model, apiKey)
	reqBody := map[string]any{
		"contents": []map[string]any{
			{
				"parts": []map[string]string{
					{"text": prompt},
				},
			},
		},
	}
	data, _ := json.Marshal(reqBody)
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(data))
	if err != nil {
		return "", 0, 0, err
	}
	req.Header.Set("content-type", "application/json")

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return "", 0, 0, err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", 0, 0, fmt.Errorf("gemini api error (%d): %s", resp.StatusCode, string(body))
	}

	var parsed struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"content"`
		} `json:"candidates"`
		UsageMetadata struct {
			PromptTokenCount     int `json:"promptTokenCount"`
			CandidatesTokenCount int `json:"candidatesTokenCount"`
		} `json:"usageMetadata"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", 0, 0, err
	}

	text := ""
	if len(parsed.Candidates) > 0 && len(parsed.Candidates[0].Content.Parts) > 0 {
		text = parsed.Candidates[0].Content.Parts[0].Text
	}
	return text, parsed.UsageMetadata.PromptTokenCount, parsed.UsageMetadata.CandidatesTokenCount, nil
}

func (c *DefaultLLMClient) completeOpenAI(ctx context.Context, entry ModelEntry, apiKey, prompt string) (string, int, int, error) {
	url := "https://api.openai.com/v1/chat/completions"
	reqBody := map[string]any{
		"model": entry.ModelID,
		"messages": []map[string]string{
			{"role": "user", "content": prompt},
		},
	}
	data, _ := json.Marshal(reqBody)
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(data))
	if err != nil {
		return "", 0, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("content-type", "application/json")

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return "", 0, 0, err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", 0, 0, fmt.Errorf("openai api error (%d): %s", resp.StatusCode, string(body))
	}

	var parsed struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", 0, 0, err
	}

	text := ""
	if len(parsed.Choices) > 0 {
		text = parsed.Choices[0].Message.Content
	}
	return text, parsed.Usage.PromptTokens, parsed.Usage.CompletionTokens, nil
}

// ProgressCallback is invoked as tasks transition status.
type ProgressCallback func(event StatusEvent)

// Dispatcher executes a plan's tasks in topological order.
type Dispatcher struct {
	Router    *Router
	LLMClient LLMClient
}

// NewDispatcher creates a new task dispatcher.
func NewDispatcher(router *Router, client LLMClient) *Dispatcher {
	if client == nil {
		client = NewDefaultLLMClient(60 * time.Second)
	}
	return &Dispatcher{
		Router:    router,
		LLMClient: client,
	}
}

// Dispatch executes the plan tasks sequentially respecting dependencies.
func (d *Dispatcher) Dispatch(ctx context.Context, plan *Plan, cb ProgressCallback) error {
	tasksByID := make(map[string]*Task)
	completed := make(map[string]bool)

	for i := range plan.Tasks {
		plan.Tasks[i].Status = StatusPending
		tasksByID[plan.Tasks[i].ID] = &plan.Tasks[i]
	}

	for i := range plan.Tasks {
		select {
		case <-ctx.Done():
			plan.Status = StatusFailed
			return ctx.Err()
		default:
		}

		t := &plan.Tasks[i]

		// Check dependencies
		depsSatisfied := true
		for _, depID := range t.DependsOn {
			if !completed[depID] {
				depsSatisfied = false
				break
			}
		}

		if !depsSatisfied {
			t.Status = StatusSkipped
			t.Error = "dependency failed or skipped"
			if cb != nil {
				cb(StatusEvent{TaskID: t.ID, Status: StatusSkipped, Error: t.Error})
			}
			continue
		}

		// Select model if not already set
		resolved, err := d.Router.SelectModel(*t)
		if err != nil {
			t.Status = StatusFailed
			t.Error = err.Error()
			if cb != nil {
				cb(StatusEvent{TaskID: t.ID, Status: StatusFailed, Error: t.Error})
			}
			continue
		}
		t.Model = resolved.Name
		t.Provider = resolved.Entry.Provider

		// Mark running
		t.Status = StatusRunning
		now := time.Now()
		t.StartedAt = &now
		if cb != nil {
			cb(StatusEvent{TaskID: t.ID, Status: StatusRunning, Model: t.Model, Provider: t.Provider})
		}

		// Build prompt with dependency context if available
		prompt := t.Description
		if t.Context != "" {
			prompt = fmt.Sprintf("Context:\n%s\n\nTask:\n%s", t.Context, t.Description)
		}

		t.Prompt = prompt

		// Call LLM
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
			continue
		}

		t.Status = StatusDone
		t.Response = response
		t.InputTokens = inTokens
		t.OutputTokens = outTokens
		t.CostUSD = resolved.Entry.EstimateCost(inTokens, outTokens)
		completed[t.ID] = true

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
	}

	plan.Status = StatusDone
	for _, t := range plan.Tasks {
		if t.Status == StatusFailed {
			plan.Status = StatusFailed
			break
		}
	}

	return nil
}

func estimateTokens(s string) int {
	words := len(strings.Fields(s))
	if words == 0 {
		return 0
	}
	return int(float64(words) * 1.3)
}

func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}
