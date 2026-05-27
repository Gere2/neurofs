package ui

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/neuromfs/neuromfs/internal/config"
)

// registerAPI wires every endpoint onto mux. The routes are flat and match
// the client's fetch() calls 1:1 — no versioning, no middleware layering,
// because the client and server ship in the same binary.
func registerAPI(mux *http.ServeMux) {
	mux.HandleFunc("/api/scan", postOnly(handleScan))
	mux.HandleFunc("/api/pack", postOnly(handlePack))
	mux.HandleFunc("/api/replay", postOnly(handleReplay))
	mux.HandleFunc("/api/records", getOnly(handleRecords))
	mux.HandleFunc("/api/record", getOnly(handleRecord))
	mux.HandleFunc("/api/search", getOnly(handleSearch))
	mux.HandleFunc("/api/diff", postOnly(handleDiff))
	mux.HandleFunc("/api/stats", getOnly(handleStats))
	mux.HandleFunc("/api/resume-seed", getOnly(handleResumeSeed))
	mux.HandleFunc("/api/task", postOnly(handleTask))
	mux.HandleFunc("/api/bootstrap", getOnly(handleBootstrap))
	mux.HandleFunc("/api/proxy/stats", getOnly(handleProxyStats))
	mux.HandleFunc("/api/chat", postOnly(handleChat))
	mux.HandleFunc("/proxy/v1/messages", postOnly(handleProxyMessages))
	mux.HandleFunc("/v1/messages", postOnly(handleProxyMessages))
	mux.HandleFunc("/proxy/v1/chat/completions", postOnly(handleProxyOpenAIMessages))
	mux.HandleFunc("/v1/chat/completions", postOnly(handleProxyOpenAIMessages))
}

// --------------------- method gates ---------------------

func postOnly(fn http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeErr(w, http.StatusMethodNotAllowed, "POST required")
			return
		}
		fn(w, r)
	}
}

func getOnly(fn http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeErr(w, http.StatusMethodNotAllowed, "GET required")
			return
		}
		fn(w, r)
	}
}

// --------------------- helpers ---------------------

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// mustRepo validates the repo path and returns a Config. Empty or relative
// paths are rejected so we never accidentally operate against the server's
// own working directory — this is a local tool, but still a web surface.
func mustRepo(w http.ResponseWriter, repo string) (*config.Config, bool) {
	repo = strings.TrimSpace(repo)
	if repo == "" {
		writeErr(w, http.StatusBadRequest, "repo is required")
		return nil, false
	}
	if !filepath.IsAbs(repo) {
		writeErr(w, http.StatusBadRequest, "repo must be an absolute path")
		return nil, false
	}
	cfg, err := config.New(repo)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return nil, false
	}
	if err := cfg.Validate(); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return nil, false
	}
	return cfg, true
}

// decode reads JSON body into v with a generous size limit. Keeping the
// limit tight protects against accidentally pasting a multi-MB file into
// a textarea — replay responses are rarely more than ~200KB.
//
// The contract is "exactly one JSON object per body": DisallowUnknownFields
// rejects typos, and the post-Decode EOF check rejects trailing content so a
// client that smuggles a second object (or junk) after the payload cannot
// slip past a handler that only reads the first.
func decode(r *http.Request, v any) error {
	r.Body = http.MaxBytesReader(nil, r.Body, 8<<20) // 8 MB
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return fmt.Errorf("body must contain a single JSON object")
	}
	return nil
}
