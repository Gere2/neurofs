package ui

import (
	"net/http"
	"os"
	"path/filepath"

	"github.com/Gere2/neurofs/internal/gamestate"
)

func handlePlayer(w http.ResponseWriter, r *http.Request) {
	home, err := os.UserHomeDir()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "cannot resolve home dir: "+err.Error())
		return
	}
	dir := filepath.Join(home, ".neurofs")
	ps, err := gamestate.Load(dir)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to load player state: "+err.Error())
		return
	}
	ps.CheckAchievements()

	// The ladder ships alongside the state so the UI never hardcodes level
	// thresholds: gamestate stays the single source of truth for what is
	// unlocked and what the next reward is.
	payload := map[string]any{
		"player":  ps,
		"unlocks": ps.Unlocks(),
	}
	if next, ok := ps.NextUnlock(); ok {
		payload["next_unlock"] = next
	}
	writeJSON(w, http.StatusOK, payload)
}
