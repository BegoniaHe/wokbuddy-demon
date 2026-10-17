package upstream

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
)

func TestModelTimeoutSelection(t *testing.T) {
	c := New()
	c.HeaderTimeout = 60 * time.Second
	c.IdleTimeout = 300 * time.Second
	c.ModelTimeouts = map[string]ModelTimeout{
		"cn:glm-5.2": {Header: 300 * time.Second},
		"gpt-5":      {Idle: 600 * time.Second},
	}
	cases := []struct {
		realm, model string
		header, idle time.Duration
	}{
		{"cn", "glm-5.2", 300 * time.Second, 300 * time.Second},
		{"global", "glm-5.2", 60 * time.Second, 300 * time.Second},
		{"cn", "gpt-5", 60 * time.Second, 600 * time.Second},
		{"global", "gpt-5", 60 * time.Second, 600 * time.Second},
		{"cn", "other", 60 * time.Second, 300 * time.Second},
	}
	for _, tc := range cases {
		h, i := c.modelTimeout(tc.realm, tc.model)
		if h != tc.header || i != tc.idle {
			t.Errorf("%s:%s = (%v,%v), want (%v,%v)", tc.realm, tc.model, h, i, tc.header, tc.idle)
		}
	}
}

// TestModelTimeoutHeaderOverride 模型覆盖的 header 超时比全局更短：慢首字节失败。
// 未配置覆盖的模型仍使用全局值并成功。
func TestModelTimeoutHeaderOverride(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(150 * time.Millisecond)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	c := New()
	c.ChatBaseCN = srv.URL
	c.HeaderTimeout = time.Second
	c.ModelTimeouts = map[string]ModelTimeout{"slow": {Header: 50 * time.Millisecond}}
	a := &auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}

	_, _, _, err := c.ChatStreamContext(context.Background(), a,
		[]byte(`{"model":"slow","messages":[]}`), "", ChatMeta{})
	if err == nil {
		t.Fatal("slow 模型应在 50ms header 超时后失败")
	}

	rc, status, _, err := c.ChatStreamContext(context.Background(), a,
		[]byte(`{"model":"fast","messages":[]}`), "", ChatMeta{})
	if err != nil || status != http.StatusOK {
		t.Fatalf("未覆盖模型应使用全局超时并成功: status=%d err=%v", status, err)
	}
	_, _ = io.Copy(io.Discard, rc)
	_ = rc.Close()
}
