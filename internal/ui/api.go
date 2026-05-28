package ui

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/neuromfs/neuromfs/internal/config"
)

// registerAPI wires every endpoint onto mux. The routes are flat and match
// the client's fetch() calls 1:1 — no versioning, no middleware layering,
// because the client and server ship in the same binary.
//
// State-changing POST endpoints are wrapped with safePost so a browser
// tab at evil.com cannot drive scan/pack/chat against the user's loopback
// server, spend their API key, or write snapshot files into the repo.
// allowedOrigins comes from the listen addr (see originsForAddr).
func registerAPI(mux *http.ServeMux, allowedOrigins map[string]bool) {
	mux.HandleFunc("/api/scan", safePost(allowedOrigins, handleScan))
	mux.HandleFunc("/api/pack", safePost(allowedOrigins, handlePack))
	mux.HandleFunc("/api/replay", safePost(allowedOrigins, handleReplay))
	mux.HandleFunc("/api/records", getOnly(handleRecords))
	mux.HandleFunc("/api/record", getOnly(handleRecord))
	mux.HandleFunc("/api/search", getOnly(handleSearch))
	mux.HandleFunc("/api/diff", safePost(allowedOrigins, handleDiff))
	mux.HandleFunc("/api/stats", getOnly(handleStats))
	mux.HandleFunc("/api/resume-seed", getOnly(handleResumeSeed))
	mux.HandleFunc("/api/task", safePost(allowedOrigins, handleTask))
	mux.HandleFunc("/api/bootstrap", getOnly(handleBootstrap))
	mux.HandleFunc("/api/proxy/stats", getOnly(handleProxyStats))
	mux.HandleFunc("/api/chat", safePost(allowedOrigins, handleChat))
	mux.HandleFunc("/proxy/v1/messages", safePost(allowedOrigins, handleProxyMessages))
	mux.HandleFunc("/v1/messages", safePost(allowedOrigins, handleProxyMessages))
	mux.HandleFunc("/proxy/v1/chat/completions", safePost(allowedOrigins, handleProxyOpenAIMessages))
	mux.HandleFunc("/v1/chat/completions", safePost(allowedOrigins, handleProxyOpenAIMessages))
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

// safeOrigin rejects any request whose Origin or Sec-Fetch-Site does
// not match the loopback server. Empty Origin AND empty Sec-Fetch-Site
// is treated as a non-browser client (curl, native HTTP) and allowed —
// CSRF only applies when a browser is involved. Modern browsers always
// send Sec-Fetch-Site on cross-origin requests, so an attacker page
// using `Content-Type: text/plain` to skip CORS preflight still gets
// caught by either Origin or Sec-Fetch-Site.
func safeOrigin(allowedOrigins map[string]bool, fn http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if origin := r.Header.Get("Origin"); origin != "" && !allowedOrigins[origin] {
			writeErr(w, http.StatusForbidden, "cross-origin request rejected")
			return
		}
		switch r.Header.Get("Sec-Fetch-Site") {
		case "", "same-origin", "none":
			// ok
		default:
			writeErr(w, http.StatusForbidden, "cross-origin request rejected")
			return
		}
		fn(w, r)
	}
}

// safePost = Origin/CSRF check + POST method gate. Use for any
// state-changing or credential-spending endpoint.
func safePost(allowedOrigins map[string]bool, fn http.HandlerFunc) http.HandlerFunc {
	return safeOrigin(allowedOrigins, postOnly(fn))
}

// originsForAddr returns the HTTP origins (both 127.0.0.1 and localhost
// forms) that match the server's listen address. A non-loopback addr
// (e.g. 0.0.0.0:7777) still produces a loopback allowlist on purpose —
// only the local user is meant to drive the UI even when the listener
// is wider.
func originsForAddr(addr string) map[string]bool {
	_, port, err := net.SplitHostPort(addr)
	if err != nil || port == "" {
		port = "7777"
	}
	return map[string]bool{
		"http://127.0.0.1:" + port: true,
		"http://localhost:" + port: true,
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
