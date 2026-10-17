package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
	"github.com/BegoniaHe/wokbuddy-demon/internal/upstream"
)

// captureBodyUpstream 记录发往上游的请求体，返回固定 SSE 成功流。
func captureBodyUpstream(t *testing.T, seen *string) *upstream.Client {
	t.Helper()
	return &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			raw, _ := io.ReadAll(r.Body)
			*seen = string(raw)
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader(sseOK)),
			}, nil
		})},
		ChatBaseCN: "https://fake.example", BillingBaseCN: "https://fake.example",
	}
}

func TestBackgroundModelRedirect(t *testing.T) {
	cases := []struct {
		name  string
		model string
		want  string
	}{
		{"hit", "gpt-5-background", "deepseek-v4.1-flash"},
		{"miss", "glm-5.2", "glm-5.2"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var seen string
			up := captureBodyUpstream(t, &seen)
			p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
			h := NewHandler(Config{
				Pool: p, Upstream: up,
				BackgroundNames:  map[string]bool{"gpt-5-background": true},
				BackgroundTarget: "deepseek-v4.1-flash",
			})
			body := `{"model":"` + tc.model + `","messages":[]}`
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))
			if rec.Code != http.StatusOK {
				t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
			}
			var got map[string]any
			if err := json.Unmarshal([]byte(seen), &got); err != nil {
				t.Fatal(err)
			}
			if got["model"] != tc.want {
				t.Errorf("upstream model=%v want %q", got["model"], tc.want)
			}
		})
	}
}

func TestBackgroundModelEmptyListUnchanged(t *testing.T) {
	h := NewHandler(Config{BackgroundTarget: "deepseek-v4.1-flash"})
	if got := h.backgroundTarget("gpt-5-background"); got != "" {
		t.Fatalf("target=%q want empty", got)
	}
}
