package panel

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/BegoniaHe/wokbuddy-demon/internal/pool"
)

func newManagedPanel(adminKey, readonlyKey string) *Panel {
	return New(&Config{
		Pool:           pool.New(""),
		APIKey:         "data-key",
		AdminAPIKey:    adminKey,
		ReadonlyAPIKey: readonlyKey,
		LoadConfig: func() (any, error) {
			return map[string]any{
				"api_key":          "data-key",
				"admin_api_key":    "admin-key",
				"readonly_api_key": "readonly-key",
				"listen":           ":7863",
			}, nil
		},
		ConfigPath: "config.json",
	})
}

func panelDo(pnl *Panel, method, path, key string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}

	rec := httptest.NewRecorder()
	pnl.ServeHTTP(rec, req)

	return rec
}

// TestPanelReadonlyScope readonly 可读、不可写；admin 可读写；默认 api_key 失去
// 管理面权限。
func TestPanelReadonlyScope(t *testing.T) {
	t.Parallel()

	pnl := newManagedPanel("admin-key", "readonly-key")

	cases := []struct {
		name, method, path, key string
		want                    int
	}{
		{"readonly 读 overview", http.MethodGet, "/panel/api/overview", "readonly-key", http.StatusOK},
		{"admin 读 overview", http.MethodGet, "/panel/api/overview", "admin-key", http.StatusOK},
		{"默认密钥读 overview 拒绝", http.MethodGet, "/panel/api/overview", "data-key", http.StatusUnauthorized},
		{"readonly 读配置 403", http.MethodGet, "/panel/api/config", "readonly-key", http.StatusForbidden},
		{"admin 读配置 200", http.MethodGet, "/panel/api/config", "admin-key", http.StatusOK},
		{"readonly 写操作 403", http.MethodPost, "/panel/api/usage/save", "readonly-key", http.StatusForbidden},
		{"admin 写操作通过鉴权", http.MethodPost, "/panel/api/usage/save", "admin-key", http.StatusNotImplemented},
		{"默认密钥写操作拒绝", http.MethodPost, "/panel/api/usage/save", "data-key", http.StatusUnauthorized},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			t.Parallel()

			rec := panelDo(pnl, item.method, item.path, item.key)
			if rec.Code != item.want {
				t.Fatalf("code=%d want %d body=%s", rec.Code, item.want, rec.Body)
			}

			for _, secret := range []string{"data-key", "admin-key", "readonly-key"} {
				if strings.Contains(rec.Body.String(), secret) {
					t.Fatalf("响应泄露凭据 %q: %s", secret, rec.Body)
				}
			}
		})
	}
}

// TestPanelConfigRedactsKeys GET /panel/api/config 不回显任何凭据原文。
func TestPanelConfigRedactsKeys(t *testing.T) {
	t.Parallel()

	pnl := newManagedPanel("admin-key", "readonly-key")

	rec := panelDo(pnl, http.MethodGet, "/panel/api/config", "admin-key")
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}

	body := rec.Body.String()
	for _, secret := range []string{"data-key", "admin-key", "readonly-key"} {
		if strings.Contains(body, secret) {
			t.Fatalf("配置回显泄露 %q: %s", secret, body)
		}
	}

	if !strings.Contains(body, `"listen":":7863"`) {
		t.Fatalf("配置回显不应丢失普通字段: %s", body)
	}
}

// TestPanelFallbackSingleKey 两把管理密钥都为空时 api_key 兼管理面。
func TestPanelFallbackSingleKey(t *testing.T) {
	t.Parallel()

	pnl := New(&Config{Pool: pool.New(""), APIKey: "data-key"})

	if rec := panelDo(pnl, http.MethodGet, "/panel/api/overview", "data-key"); rec.Code != http.StatusOK {
		t.Fatalf("单密钥默认 code=%d", rec.Code)
	}

	rec := panelDo(pnl, http.MethodGet, "/panel/api/config", "data-key")
	if rec.Code != http.StatusNotImplemented {
		// LoadConfig 未注入时 501 说明鉴权已放行。
		t.Fatalf("配置端点 code=%d", rec.Code)
	}
}
