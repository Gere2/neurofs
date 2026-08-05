package ui

import (
	"crypto/subtle"
	"fmt"
	"net"
	"net/http"
	"strings"
)

const (
	maxRequestBodyBytes = int64(8 << 20)
	remoteTokenHeader   = "X-NeuroFS-Token"
)

func normalizeServerOptions(opts *Options) (bool, error) {
	if opts.Addr == "" {
		opts.Addr = "127.0.0.1:7777"
	}
	host, _, err := net.SplitHostPort(opts.Addr)
	if err != nil {
		return false, fmt.Errorf("invalid listen address %q: %w", opts.Addr, err)
	}
	remote := !isLoopbackHost(host)
	if !remote {
		return false, nil
	}
	if !opts.AllowRemote {
		return false, fmt.Errorf("non-loopback address %q requires explicit remote access", opts.Addr)
	}
	if strings.TrimSpace(opts.AuthToken) == "" {
		return false, fmt.Errorf("remote access requires an authentication token")
	}
	return true, nil
}

func isLoopbackHost(host string) bool {
	host = strings.TrimSuffix(strings.TrimSpace(host), ".")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func requestHostIsLoopback(hostport string) bool {
	hostport = strings.TrimSpace(hostport)
	if hostport == "" {
		return true
	}
	host, _, err := net.SplitHostPort(hostport)
	if err != nil {
		host = strings.Trim(hostport, "[]")
	}
	return isLoopbackHost(host)
}

func secureServerHandler(next http.Handler, remote bool, token string) http.Handler {
	h := withRequestBodyLimit(next, maxRequestBodyBytes)
	h = withRemoteAuthentication(h, remote, token)
	h = withLocalHostValidation(h, !remote)
	return h
}

func withRequestBodyLimit(next http.Handler, limit int64) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body != nil {
			r.Body = http.MaxBytesReader(w, r.Body, limit)
		}
		next.ServeHTTP(w, r)
	})
}

func withLocalHostValidation(next http.Handler, enabled bool) http.Handler {
	if !enabled {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !requestHostIsLoopback(r.Host) {
			writeErr(w, http.StatusForbidden, "request host is not allowed")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func withRemoteAuthentication(next http.Handler, enabled bool, token string) http.Handler {
	if !enabled {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !requiresRemoteAuthentication(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		provided := r.Header.Get(remoteTokenHeader)
		if subtle.ConstantTimeCompare([]byte(provided), []byte(token)) != 1 {
			w.Header().Set("WWW-Authenticate", remoteTokenHeader)
			writeErr(w, http.StatusUnauthorized, "authentication required")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func requiresRemoteAuthentication(path string) bool {
	return strings.HasPrefix(path, "/api/") ||
		strings.HasPrefix(path, "/v1/") ||
		strings.HasPrefix(path, "/proxy/")
}
