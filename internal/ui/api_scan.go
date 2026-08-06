package ui

import (
	"log"
	"net/http"
	"time"

	"github.com/Gere2/neurofs/internal/indexer"
	"github.com/Gere2/neurofs/internal/storage"
)

// --------------------- /api/scan ---------------------

type scanReq struct {
	Repo    string `json:"repo"`
	Verbose bool   `json:"verbose"`
}

type scanResp struct {
	OK      bool           `json:"ok"`
	Summary map[string]any `json:"summary"`
	Lang    map[string]int `json:"lang,omitempty"`
	Errors  []string       `json:"errors,omitempty"`
}

func handleScan(w http.ResponseWriter, r *http.Request) {
	var req scanReq
	if err := decode(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad JSON: "+err.Error())
		return
	}
	cfg, ok := mustRepo(w, req.Repo)
	if !ok {
		return
	}

	db, err := storage.Open(cfg.DBPath)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "open db: "+err.Error())
		return
	}
	defer func() {
		if err := db.Close(); err != nil {
			log.Printf("neurofs ui: close scan database: %v", err)
		}
	}()

	start := time.Now()
	stats, err := indexer.Run(cfg, db, indexer.Options{})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "scan: "+err.Error())
		return
	}

	writeJSON(w, http.StatusOK, scanResp{
		OK: true,
		Summary: map[string]any{
			"discovered": stats.Discovered,
			"indexed":    stats.Indexed,
			"skipped":    stats.Skipped,
			"removed":    stats.Removed,
			"errors":     stats.Errors,
			"symbols":    stats.Symbols,
			"imports":    stats.Imports,
			"chunks":     stats.Chunks,
			"db_path":    cfg.DBPath,
			"elapsed_ms": time.Since(start).Milliseconds(),
		},
	})
}

// --------------------- /api/bootstrap ---------------------

type bootstrapResp struct {
	Cwd string `json:"cwd"`
}

func handleBootstrap(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, bootstrapResp{Cwd: startupDir})
}
