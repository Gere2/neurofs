package ui

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os/exec"
	"runtime"
	"time"
)

// Options configures the local UI server. Defaults (127.0.0.1:7777, open
// browser on start) match the expected "zero-config local tool" use case —
// override only when embedding NeuroFS somewhere else.
type Options struct {
	Addr        string
	OpenBrowser bool
}

// Run starts the local UI server and blocks until the process is interrupted
// or the listener errors. It binds loopback-only by default; the caller owns
// signal handling.
//
// The server shares no process state across requests. Every HTTP handler
// opens the repo index on its own, mirroring the CLI commands' per-
// invocation pattern.
func Run(opts Options) error {
	if opts.Addr == "" {
		opts.Addr = "127.0.0.1:7777"
	}

	mux := http.NewServeMux()

	// Static assets are served from the embedded subtree rooted at static/.
	// We strip the embed prefix so routes look like /static/app.js, not
	// /static/static/app.js.
	sub, err := fs.Sub(assets, "static")
	if err != nil {
		return fmt.Errorf("ui: embed subfs: %w", err)
	}
	fileServer := http.FileServer(http.FS(sub))
	mux.Handle("/static/", http.StripPrefix("/static/", fileServer))

	// Root serves index.html directly so the app opens at `/` without a
	// trailing /static path. Everything else is delegated by the API.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		data, err := fs.ReadFile(sub, "index.html")
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(data)
	})

	registerAPI(mux)

	srv := &http.Server{
		Addr:    opts.Addr,
		Handler: withJSONLogger(mux),
		// WriteTimeout is deliberately longer than contextFor's 2-minute
		// handler budget so a slow-but-legitimate scan/pack completes its
		// response instead of being killed mid-flush.
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      3 * time.Minute,
		IdleTimeout:       60 * time.Second,
	}

	// The welcome line is printed to the CLI so the user can click the URL
	// even if OpenBrowser is disabled (e.g. on a headless machine).
	url := "http://" + opts.Addr
	fmt.Printf("NeuroFS UI listening at %s\n", url)

	if opts.OpenBrowser {
		// Best-effort; a failure here is logged but never blocks the server.
		go openInBrowser(url)
	}

	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// openInBrowser tries to launch the default browser. Kept platform-light:
// macOS uses `open`, Linux uses `xdg-open`, Windows uses `rundll32`. If none
// are available the UI still works — the user follows the printed URL.
func openInBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "linux":
		cmd = exec.Command("xdg-open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		return
	}
	_ = cmd.Start()
}

// withJSONLogger is a trivial log middleware. API handlers are quick enough
// that per-request timing is useful when debugging "why is pack slow" —
// writing to stderr keeps it out of the embedded HTML response stream.
func withJSONLogger(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		h.ServeHTTP(w, r)
		// Only log API and static-miss paths; /static/... noise is unhelpful.
		if len(r.URL.Path) >= 5 && r.URL.Path[:5] == "/api/" {
			fmt.Printf("  %s %s  %s\n", r.Method, r.URL.Path, time.Since(start).Round(time.Millisecond))
		}
	})
}

// contextFor returns a request-scoped context with a modest timeout. Every
// endpoint is local, but scan + pack do disk I/O and we would rather fail
// fast than hang a browser tab forever.
func contextFor(r *http.Request) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), 2*time.Minute)
}
