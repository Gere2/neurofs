// Package a2a implements the Agent-to-Agent (A2A) v1.0 Protocol specification.
// It exposes Agent Cards and capability discovery endpoints so any external AI agent
// (Cursor, Claude Code, Devin, custom sub-agents) can discover and invoke NeuroFS
// as a verification & context provider.
package a2a

import (
	"encoding/json"
	"net/http"
	"time"
)

// AgentCard defines the A2A v1.0 Agent Discovery Specification payload.
type AgentCard struct {
	Name         string            `json:"name"`
	Description  string            `json:"description"`
	URL          string            `json:"url"`
	Version      string            `json:"version"`
	Protocol     string            `json:"protocol"`
	Capabilities AgentCapabilities `json:"capabilities"`
	Skills       []AgentSkill      `json:"skills"`
	UpdatedAt    time.Time         `json:"updated_at"`
}

// AgentCapabilities advertises protocol-level options.
type AgentCapabilities struct {
	Streaming         bool `json:"streaming"`
	Stateless         bool `json:"stateless"`
	PushNotifications bool `json:"push_notifications"`
}

// AgentSkill represents one exported capability of the agent.
type AgentSkill struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	InputSchema string   `json:"input_schema,omitempty"`
	Tags        []string `json:"tags,omitempty"`
}

// DefaultAgentCard returns the official A2A v1.0 discovery payload for NeuroFS.
func DefaultAgentCard(baseURL string) AgentCard {
	if baseURL == "" {
		baseURL = "http://127.0.0.1:7777"
	}
	return AgentCard{
		Name:        "neurofs-agent-commander",
		Description: "Multi-model task dispatcher, iso-recall context provider, and receipt-based verifier",
		URL:         baseURL,
		Version:     "0.8.0",
		Protocol:    "a2a/1.0",
		Capabilities: AgentCapabilities{
			Streaming:         true,
			Stateless:         true,
			PushNotifications: false,
		},
		Skills: []AgentSkill{
			{
				ID:          "context-retrieval",
				Name:        "Iso-Recall Context Retrieval",
				Description: "Returns minimal, grounded code context for developer queries",
				Tags:        []string{"context", "code", "retrieval"},
			},
			{
				ID:          "grounding-verification",
				Name:        "Receipt-Based Verification",
				Description: "Decomposes output claims, checks entailment against context, and runs build sandbox",
				Tags:        []string{"verification", "audit", "sandbox"},
			},
			{
				ID:          "speculative-orchestration",
				Name:        "Speculative Cascade Orchestration",
				Description: "Decomposes complex requests, routes tasks across LLMs, and escalates on low grounding",
				Tags:        []string{"orchestration", "multi-agent", "cascade"},
			},
		},
		UpdatedAt: time.Now(),
	}
}

// Handler returns an http.HandlerFunc that serves the A2A v1.0 Agent Card JSON.
func Handler(baseURL string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		card := DefaultAgentCard(baseURL)
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		// No wildcard CORS: agent-to-agent discovery clients are not browsers
		// and do not enforce it, so the header would only serve to let an
		// arbitrary web page fingerprint the local NeuroFS instance.
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(card)
	}
}
