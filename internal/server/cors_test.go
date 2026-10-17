package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCORSDisabledByDefault(t *testing.T) {
	h := NewHandler(Config{})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodOptions, "/v1/chat/completions", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("code=%d want 404", rec.Code)
	}
	if rec.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Error("CORS headers must be absent when disabled")
	}
}

func TestCORSPreflightAllowed(t *testing.T) {
	h := NewHandler(Config{CORSOrigins: []string{"http://localhost:3000"}})
	for _, origin := range []string{"http://localhost:3000", "HTTP://LOCALHOST:3000"} {
		req := httptest.NewRequest(http.MethodOptions, "/v1/chat/completions", nil)
		req.Header.Set("Origin", origin)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusNoContent {
			t.Fatalf("origin=%q code=%d want 204", origin, rec.Code)
		}
		if rec.Header().Get("Access-Control-Allow-Origin") != "http://localhost:3000" {
			t.Errorf("origin=%q allow-origin=%q", origin, rec.Header().Get("Access-Control-Allow-Origin"))
		}
		if !strings.Contains(rec.Header().Get("Access-Control-Allow-Methods"), "POST") {
			t.Error("missing allow-methods")
		}
		if !strings.Contains(rec.Header().Get("Access-Control-Allow-Headers"), "Authorization") {
			t.Error("missing allow-headers")
		}
		if rec.Header().Get("Access-Control-Allow-Credentials") != "" {
			t.Error("credentials must not be allowed")
		}
	}
}

func TestCORSPreflightDenied(t *testing.T) {
	h := NewHandler(Config{CORSOrigins: []string{"http://localhost:3000"}})
	req := httptest.NewRequest(http.MethodOptions, "/v1/chat/completions", nil)
	req.Header.Set("Origin", "http://evil.example")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("code=%d want 204", rec.Code)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("denied origin echoed: %q", got)
	}
}

func TestCORSSimpleResponseAndAdminExcluded(t *testing.T) {
	h := NewHandler(Config{APIKey: "secret", CORSOrigins: []string{"http://localhost:3000"}})
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Origin", "http://localhost:3000")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("code=%d want 401", rec.Code)
	}
	if rec.Header().Get("Access-Control-Allow-Origin") != "http://localhost:3000" {
		t.Error("simple response missing CORS header")
	}
	if !strings.Contains(rec.Header().Get("Vary"), "Origin") {
		t.Error("missing Vary: Origin")
	}

	admin := httptest.NewRequest(http.MethodGet, "/status", nil)
	admin.Header.Set("Origin", "http://localhost:3000")
	adminRec := httptest.NewRecorder()
	h.ServeHTTP(adminRec, admin)
	if adminRec.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Error("admin surface must not expose CORS headers")
	}
}
