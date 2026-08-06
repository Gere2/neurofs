package orchestrator

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// ModelEntry describes a single LLM available for dispatch.
type ModelEntry struct {
	Provider       string   `json:"provider"`        // "anthropic", "google", "openai"
	ModelID        string   `json:"model_id"`         // API model identifier
	APIKeyEnv      string   `json:"api_key_env"`      // env var holding the API key
	CostInputPer1M  float64 `json:"cost_input_per_1m"`  // USD per 1M input tokens
	CostOutputPer1M float64 `json:"cost_output_per_1m"` // USD per 1M output tokens
	Strengths      []string `json:"strengths,omitempty"`
	MaxContext     int      `json:"max_context,omitempty"`
}

// RoutingRules maps a task kind (or "default"/"planner") to a model name.
type RoutingRules map[string]string

// CascadeConfig controls speculative execution: start cheap, escalate
// to stronger models only when grounding falls below the threshold.
type CascadeConfig struct {
	Enabled            bool     `json:"enabled"`
	GroundingThreshold float64  `json:"grounding_threshold"` // escalate below this (0.0-1.0)
	MaxAttempts        int      `json:"max_attempts"`        // hard cap on cascade depth
	EscalationChain    []string `json:"escalation_chain"`    // model names cheapest→strongest
}

// DefaultCascadeConfig returns a sensible cascade: Flash → Sonnet → Opus,
// escalating when grounding < 85%, capped at 3 attempts.
func DefaultCascadeConfig() CascadeConfig {
	return CascadeConfig{
		Enabled:            true,
		GroundingThreshold: 0.85,
		MaxAttempts:        3,
		EscalationChain:    []string{"gemini-flash", "claude-sonnet", "claude-opus"},
	}
}

// ModelsConfig is the user-editable model registry and routing rules.
// It is read from ~/.neurofs/models.json or a repo-local override.
type ModelsConfig struct {
	Models  map[string]ModelEntry `json:"models"`
	Routing RoutingRules          `json:"routing"`
	Cascade CascadeConfig         `json:"cascade"`
}

// DefaultModelsConfig returns a sensible starter config with the three
// major providers. The user can override this by creating models.json.
func DefaultModelsConfig() ModelsConfig {
	return ModelsConfig{
		Models: map[string]ModelEntry{
			"claude-opus": {
				Provider:       "anthropic",
				ModelID:        "claude-opus-4-0725",
				APIKeyEnv:      "ANTHROPIC_API_KEY",
				CostInputPer1M:  15.0,
				CostOutputPer1M: 75.0,
				Strengths:      []string{"complex_reasoning", "architecture", "debugging"},
				MaxContext:     200000,
			},
			"claude-sonnet": {
				Provider:       "anthropic",
				ModelID:        "claude-sonnet-4-6",
				APIKeyEnv:      "ANTHROPIC_API_KEY",
				CostInputPer1M:  3.0,
				CostOutputPer1M: 15.0,
				Strengths:      []string{"coding", "tests", "frontend", "backend"},
				MaxContext:     200000,
			},
			"gemini-flash": {
				Provider:       "google",
				ModelID:        "gemini-2.5-flash-preview-05-20",
				APIKeyEnv:      "GEMINI_API_KEY",
				CostInputPer1M:  0.15,
				CostOutputPer1M: 0.60,
				Strengths:      []string{"planning", "simple_tasks", "sql", "design"},
				MaxContext:     1000000,
			},
			"gpt-4o-mini": {
				Provider:       "openai",
				ModelID:        "gpt-4o-mini",
				APIKeyEnv:      "OPENAI_API_KEY",
				CostInputPer1M:  0.15,
				CostOutputPer1M: 0.60,
				Strengths:      []string{"formatting", "translations", "simple_coding"},
				MaxContext:     128000,
			},
		},
		Routing: RoutingRules{
			"planner":  "gemini-flash",
			"frontend": "claude-sonnet",
			"backend":  "claude-sonnet",
			"database": "gemini-flash",
			"test":     "claude-sonnet",
			"design":   "gemini-flash",
			"complex":  "claude-opus",
			"docs":     "gpt-4o-mini",
			"config":   "gemini-flash",
			"general":  "claude-sonnet",
			"default":  "claude-sonnet",
		},
		Cascade: DefaultCascadeConfig(),
	}
}

// modelsConfigPaths returns the candidate paths for models.json,
// in priority order (repo-local first, then global).
func modelsConfigPaths(repoRoot string) []string {
	var paths []string
	if repoRoot != "" {
		paths = append(paths, filepath.Join(repoRoot, ".neurofs", "models.json"))
	}
	if home, err := os.UserHomeDir(); err == nil {
		paths = append(paths, filepath.Join(home, ".neurofs", "models.json"))
	}
	return paths
}

// LoadModelsConfig reads models.json from the first available path
// (repo-local, then ~/.neurofs/). If no file exists, returns defaults.
func LoadModelsConfig(repoRoot string) (ModelsConfig, error) {
	for _, p := range modelsConfigPaths(repoRoot) {
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var cfg ModelsConfig
		if err := json.Unmarshal(data, &cfg); err != nil {
			return ModelsConfig{}, fmt.Errorf("parse %s: %w", p, err)
		}
		if cfg.Models == nil {
			cfg.Models = make(map[string]ModelEntry)
		}
		if cfg.Routing == nil {
			cfg.Routing = make(RoutingRules)
		}
		// Merge defaults for any missing routing rules
		defaults := DefaultModelsConfig()
		for k, v := range defaults.Routing {
			if _, ok := cfg.Routing[k]; !ok {
				cfg.Routing[k] = v
			}
		}
		return cfg, nil
	}
	return DefaultModelsConfig(), nil
}

// WriteModelsConfig writes a ModelsConfig struct to .neurofs/models.json in the given directory.
func WriteModelsConfig(dir string, cfg ModelsConfig) error {
	if dir == "" {
		if home, err := os.UserHomeDir(); err == nil {
			dir = filepath.Join(home, ".neurofs")
		} else {
			dir = "."
		}
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(dir, ".neurofs", "models.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}

// WriteDefaultConfig writes the default models.json to the given directory.
func WriteDefaultConfig(dir string) error {
	return WriteModelsConfig(dir, DefaultModelsConfig())
}

// ResolveAPIKey returns the API key for a model entry from the environment.
func (m ModelEntry) ResolveAPIKey() string {
	if m.APIKeyEnv == "" {
		return ""
	}
	return os.Getenv(m.APIKeyEnv)
}

// EstimateCost calculates the cost in USD for the given token counts.
func (m ModelEntry) EstimateCost(inputTokens, outputTokens int) float64 {
	inCost := float64(inputTokens) / 1_000_000.0 * m.CostInputPer1M
	outCost := float64(outputTokens) / 1_000_000.0 * m.CostOutputPer1M
	return inCost + outCost
}
