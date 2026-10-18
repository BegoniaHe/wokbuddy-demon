package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
	"github.com/BegoniaHe/wokbuddy-demon/internal/upstream"
)

// TestHealthz503WhileShuttingDown 停机第一阶段 /healthz 立即 503，保留池计数供排障。
func TestHealthz503WhileShuttingDown(t *testing.T) {
	t.Parallel()

	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
	h := NewHandler(&Config{Pool: p, Upstream: upstream.New()})
	h.BeginShutdown()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code=%d want 503", rec.Code)
	}

	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("healthz not json: %v body=%s", err, rec.Body)
	}

	if resp["shutting_down"] != true || resp["healthy"] != float64(1) || resp["total"] != float64(1) {
		t.Errorf("healthz=%v want shutting_down=true healthy=1 total=1", resp)
	}
}
