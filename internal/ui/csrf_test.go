package ui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// safePostHarness wraps a tiny "did it run" handler with safePost so we
// can probe the middleware without driving the real /api/scan flow.
func safePostHarness(t *testing.T) (http.HandlerFunc, *bool) {
	t.Helper()
	allowed := originsForAddr("127.0.0.1:7777")
	ran := false
	return safePost(allowed, func(w http.ResponseWriter, r *http.Request) {
		ran = true
		w.WriteHeader(http.StatusOK)
	}), &ran
}

// CSRF_AttackerOriginRejected reproduces the security traffic agent's
// CRIT-1 attack: a malicious page at https://evil.com tries to POST to
// the loopback server via fetch(). The Origin header gives it away.
func TestCSRF_AttackerOriginRejected(t *testing.T) {
	h, ran := safePostHarness(t)

	r := httptest.NewRequest("POST", "/api/scan", strings.NewReader(`{}`))
	r.Header.Set("Origin", "https://evil.com")
	r.Header.Set("Content-Type", "text/plain") // CORS-preflight-skipping trick
	w := httptest.NewRecorder()
	h(w, r)

	if w.Code != http.StatusForbidden {
		t.Fatalf("evil.com Origin must be rejected with 403; got %d", w.Code)
	}
	if *ran {
		t.Fatalf("handler must NOT execute on a cross-origin request")
	}
}

// Sec-Fetch-Site=cross-site rejection covers the case where a browser
// strips Origin (some configurations) but still sends the navigation
// metadata header.
func TestCSRF_CrossSiteFetchRejected(t *testing.T) {
	h, ran := safePostHarness(t)

	r := httptest.NewRequest("POST", "/api/scan", strings.NewReader(`{}`))
	r.Header.Set("Sec-Fetch-Site", "cross-site")
	w := httptest.NewRecorder()
	h(w, r)

	if w.Code != http.StatusForbidden {
		t.Fatalf("Sec-Fetch-Site=cross-site must be 403; got %d", w.Code)
	}
	if *ran {
		t.Fatalf("handler must NOT execute on a cross-site fetch")
	}
}

// Same-origin browser requests are the legitimate UI-driving flow and
// must keep working.
func TestCSRF_SameOriginAllowed(t *testing.T) {
	h, ran := safePostHarness(t)

	r := httptest.NewRequest("POST", "/api/scan", strings.NewReader(`{}`))
	r.Header.Set("Origin", "http://127.0.0.1:7777")
	r.Header.Set("Sec-Fetch-Site", "same-origin")
	w := httptest.NewRecorder()
	h(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("same-origin request must pass; got %d", w.Code)
	}
	if !*ran {
		t.Fatalf("handler must execute on a same-origin request")
	}
}

// Non-browser clients (curl, native HTTP) send neither Origin nor
// Sec-Fetch-Site. They must keep working — CSRF is a browser concern.
func TestCSRF_NoOriginNoFetchSiteAllowed(t *testing.T) {
	h, ran := safePostHarness(t)

	r := httptest.NewRequest("POST", "/api/scan", strings.NewReader(`{}`))
	w := httptest.NewRecorder()
	h(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("native client (no Origin) must pass; got %d", w.Code)
	}
	if !*ran {
		t.Fatalf("handler must execute for a non-browser caller")
	}
}

// localhost form must also be accepted (UI may be loaded via either).
func TestCSRF_LocalhostFormAllowed(t *testing.T) {
	h, ran := safePostHarness(t)

	r := httptest.NewRequest("POST", "/api/scan", strings.NewReader(`{}`))
	r.Header.Set("Origin", "http://localhost:7777")
	r.Header.Set("Sec-Fetch-Site", "same-origin")
	w := httptest.NewRecorder()
	h(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("localhost Origin must pass; got %d", w.Code)
	}
	if !*ran {
		t.Fatalf("handler must execute for localhost Origin")
	}
}

// Origin-substring trick must NOT slip through: http://127.0.0.1:7777.evil.com
// is a distinct origin and must be rejected. This is why the check is
// equality on the full Origin string, not prefix/suffix.
func TestCSRF_SubdomainTrickRejected(t *testing.T) {
	h, ran := safePostHarness(t)

	r := httptest.NewRequest("POST", "/api/scan", strings.NewReader(`{}`))
	r.Header.Set("Origin", "http://127.0.0.1:7777.evil.com")
	w := httptest.NewRecorder()
	h(w, r)

	if w.Code != http.StatusForbidden {
		t.Fatalf("Origin substring trick must be 403; got %d", w.Code)
	}
	if *ran {
		t.Fatalf("handler must NOT execute on a spoofed Origin")
	}
}

// originsForAddr must produce both 127.0.0.1 and localhost variants for
// a given port, and default to 7777 when the addr is unparseable.
func TestOriginsForAddr(t *testing.T) {
	cases := []struct {
		addr     string
		wantPort string
	}{
		{"127.0.0.1:7777", "7777"},
		{"0.0.0.0:8888", "8888"},
		{"localhost:9000", "9000"},
		{"", "7777"},        // fallback
		{"garbage", "7777"}, // fallback
	}
	for _, c := range cases {
		got := originsForAddr(c.addr)
		want127 := "http://127.0.0.1:" + c.wantPort
		wantLh := "http://localhost:" + c.wantPort
		if !got[want127] || !got[wantLh] {
			t.Errorf("originsForAddr(%q) = %v, want both %q and %q", c.addr, got, want127, wantLh)
		}
	}
}
