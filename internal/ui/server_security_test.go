package ui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNormalizeServerOptions(t *testing.T) {
	t.Run("default stays local", func(t *testing.T) {
		opts := Options{}
		remote, err := normalizeServerOptions(&opts)
		if err != nil {
			t.Fatal(err)
		}
		if remote || opts.Addr != "127.0.0.1:7777" {
			t.Fatalf("remote=%v addr=%q", remote, opts.Addr)
		}
	})

	t.Run("external address requires explicit settings", func(t *testing.T) {
		opts := Options{Addr: "0.0.0.0:7777"}
		if _, err := normalizeServerOptions(&opts); err == nil {
			t.Fatal("expected external address to require explicit access")
		}
		opts.AllowRemote = true
		if _, err := normalizeServerOptions(&opts); err == nil {
			t.Fatal("expected remote access to require an auth token")
		}
		opts.AuthToken = "test-token"
		remote, err := normalizeServerOptions(&opts)
		if err != nil {
			t.Fatal(err)
		}
		if !remote {
			t.Fatal("external address was not classified as remote")
		}
	})
}

func TestRemoteAuthentication(t *testing.T) {
	ran := false
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		ran = true
		w.WriteHeader(http.StatusNoContent)
	})
	h := withRemoteAuthentication(next, true, "test-token")

	req := httptest.NewRequest(http.MethodGet, "/api/stats", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized || ran {
		t.Fatalf("missing token: status=%d ran=%v", rec.Code, ran)
	}

	req = httptest.NewRequest(http.MethodGet, "/api/stats", nil)
	req.Header.Set(remoteTokenHeader, "test-token")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent || !ran {
		t.Fatalf("valid token: status=%d ran=%v", rec.Code, ran)
	}
}

func TestLocalHostValidation(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	h := withLocalHostValidation(next, true)

	req := httptest.NewRequest(http.MethodGet, "/api/stats", nil)
	req.Host = "127.0.0.1:7777"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("loopback host status = %d", rec.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "/api/stats", nil)
	req.Host = "example.test:7777"
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-loopback host status = %d", rec.Code)
	}
}

func TestReadLimitedBodyRejectsDeclaredOversize(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/api/chat", strings.NewReader("{}"))
	req.ContentLength = maxRequestBodyBytes + 1
	rec := httptest.NewRecorder()
	if _, ok := readLimitedBody(rec, req); ok {
		t.Fatal("oversized body was accepted")
	}
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusRequestEntityTooLarge)
	}
}
