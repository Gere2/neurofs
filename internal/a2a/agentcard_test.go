package a2a

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestDefaultAgentCard(t *testing.T) {
	card := DefaultAgentCard("http://localhost:7777")
	if card.Name != "neurofs-agent-commander" {
		t.Errorf("expected name neurofs-agent-commander, got %s", card.Name)
	}
	if card.Protocol != "a2a/1.0" {
		t.Errorf("expected protocol a2a/1.0, got %s", card.Protocol)
	}
	if len(card.Skills) != 3 {
		t.Errorf("expected 3 skills, got %d", len(card.Skills))
	}
}

func TestAgentCardHandler(t *testing.T) {
	req := httptest.NewRequest("GET", "/.well-known/agent.json", nil)
	rr := httptest.NewRecorder()

	handler := Handler("http://localhost:7777")
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", rr.Code)
	}

	var card AgentCard
	if err := json.NewDecoder(rr.Body).Decode(&card); err != nil {
		t.Fatalf("failed to decode agent card: %v", err)
	}

	if !card.Capabilities.Stateless {
		t.Error("expected stateless capability to be true")
	}
}
