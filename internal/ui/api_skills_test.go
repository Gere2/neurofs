package ui

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Gere2/neurofs/internal/orchestrator"
)

func TestAPISkills_GetAndPost(t *testing.T) {
	mux := http.NewServeMux()
	registerAPI(mux, map[string]bool{"http://localhost:8765": true})

	// 1. Post a new skill
	skill := orchestrator.CrossProjectSkill{
		Domain:           "go_cli",
		TaskKind:         orchestrator.KindBackend,
		RecommendedModel: "claude-sonnet",
		Insight:          "Always use bufio.Scanner with 64MB buffer for large JSONL streams",
		Confidence:       0.95,
	}
	bodyBytes, _ := json.Marshal(skill)

	reqPost := httptest.NewRequest("POST", "/api/skills", bytes.NewReader(bodyBytes))
	rrPost := httptest.NewRecorder()
	mux.ServeHTTP(rrPost, reqPost)

	if rrPost.Code != http.StatusOK {
		t.Fatalf("POST /api/skills failed: status %d", rrPost.Code)
	}

	// 2. GET skills
	reqGet := httptest.NewRequest("GET", "/api/skills", nil)
	rrGet := httptest.NewRecorder()
	mux.ServeHTTP(rrGet, reqGet)

	if rrGet.Code != http.StatusOK {
		t.Fatalf("GET /api/skills failed: status %d", rrGet.Code)
	}

	var res map[string][]orchestrator.CrossProjectSkill
	if err := json.NewDecoder(rrGet.Body).Decode(&res); err != nil {
		t.Fatalf("failed to decode GET /api/skills response: %v", err)
	}

	skills := res["skills"]
	if len(skills) == 0 {
		t.Error("expected skills array to contain at least 1 item")
	}
}
