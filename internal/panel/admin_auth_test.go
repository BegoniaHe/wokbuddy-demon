package panel

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/BegoniaHe/wokbuddy-demon/internal/httpauth"
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
				"upstash":          map[string]any{"url": "https://x.upstash.io", "token": "upstash-token"},
				"upstream":         map[string]any{"device_token": "device-token"},
				"notify":           map[string]any{"webhook_url": "https://hooks.example/hook?token=wh-secret"},
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
	// 顶层三把 key 与嵌套凭据都必须抹掉：upstash.token 会拼进 rediss:// 连接串、
	// upstream.device_token 与 notify.webhook_url（可内嵌 token）同样是凭据。
	for _, secret := range []string{
		"data-key", "admin-key", "readonly-key",
		"upstash-token", "device-token", "wh-secret",
	} {
		if strings.Contains(body, secret) {
			t.Fatalf("配置回显泄露 %q: %s", secret, body)
		}
	}

	if !strings.Contains(body, `"listen":":7863"`) {
		t.Fatalf("配置回显不应丢失普通字段: %s", body)
	}
}

// TestAuthFailBudgetNotResetByForbidden readonly 打 admin 路由（403）不能清零失败
// 预算：否则「猜 admin key（记一次失败）→ 用 readonly key 打任意接口（清零）」
// 可以无限试探，失败限速形同失效。
func TestAuthFailBudgetNotResetByForbidden(t *testing.T) {
	t.Parallel()

	pnl := newManagedPanel("admin-key", "readonly-key")

	const (
		peerIP    = "192.0.2.7"
		remoteAdd = peerIP + ":1234"
	)

	// 先把失败预算消耗到只剩 1 次。
	for range httpauth.DefaultFailLimit - 1 {
		pnl.authLimiter.Fail(peerIP)
	}

	newReq := func(key string) *http.Request {
		req := httptest.NewRequest(http.MethodPost, "/panel/api/config", nil)
		req.RemoteAddr = remoteAdd

		if key != "" {
			req.Header.Set("Authorization", "Bearer "+key)
		}

		return req
	}

	// readonly 命中 admin 路由：凭据有效但作用域不足 → 403，且不得清零预算。
	rec := httptest.NewRecorder()
	if pnl.allowScope(rec, newReq("readonly-key"), true) {
		t.Fatal("readonly 不应通过 admin 校验")
	}

	if rec.Code != http.StatusForbidden {
		t.Fatalf("code=%d want 403", rec.Code)
	}

	// 预算未被清零：再错一次即达上限。
	badRec := httptest.NewRecorder()
	if pnl.allowScope(badRec, newReq("wrong-key"), true) {
		t.Fatal("错误密钥不应通过")
	}

	// 达到上限后必须限速，而不是继续放行试探。
	limited := httptest.NewRecorder()
	if pnl.allowScope(limited, newReq("wrong-key"), true) {
		t.Fatal("超限后不应通过")
	}

	if limited.Code != http.StatusTooManyRequests {
		t.Fatalf("code=%d want 429（403 不得清零失败预算）", limited.Code)
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

// TestAuthFailBudgetNotResetByReadonlySuccess readonly 凭据成功通过只读路由同样
// 不得清零失败预算。
//
// 失败预算的语义是「这个来源猜测了多少次凭据」。readonly 打只读接口是合法访问，
// 但把它当成清零依据，就等于任何一个持有只读密钥的人可以「猜一次 admin key（记
// 一次失败）→ 用 readonly 打只读接口（清零）」无限试探。
func TestAuthFailBudgetNotResetByReadonlySuccess(t *testing.T) {
	t.Parallel()

	pnl := newManagedPanel("admin-key", "readonly-key")

	const (
		peerIP    = "192.0.2.8"
		remoteAdd = peerIP + ":1234"
	)

	for range httpauth.DefaultFailLimit - 1 {
		pnl.authLimiter.Fail(peerIP)
	}

	newReq := func(key string) *http.Request {
		// overview 属只读接口：readonly 凭据在此合法通过。
		req := httptest.NewRequest(http.MethodGet, "/panel/api/overview", nil)
		req.RemoteAddr = remoteAdd

		if key != "" {
			req.Header.Set("Authorization", "Bearer "+key)
		}

		return req
	}

	if rec := httptest.NewRecorder(); !pnl.allowScope(rec, newReq("readonly-key"), false) {
		t.Fatalf("readonly 只读路由应放行，code=%d", rec.Code)
	}

	// 预算未被清零：再用错一次即达上限，随后必须限速。
	badRec := httptest.NewRecorder()
	if pnl.allowScope(badRec, newReq("wrong-key"), false) {
		t.Fatal("错误密钥不应通过")
	}

	limited := httptest.NewRecorder()
	if pnl.allowScope(limited, newReq("wrong-key"), false) {
		t.Fatal("超限后不应通过")
	}

	if limited.Code != http.StatusTooManyRequests {
		t.Fatalf("code=%d want 429（readonly 成功不得清零失败预算）", limited.Code)
	}
}
