package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/BegoniaHe/wokbuddy-demon/internal/apikey"
	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
	"github.com/BegoniaHe/wokbuddy-demon/internal/httpauth"
	"github.com/BegoniaHe/wokbuddy-demon/internal/livecfg"
	"github.com/BegoniaHe/wokbuddy-demon/internal/panel"
)

func doWithKey(handler http.Handler, method, path, key string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	return rec
}

// TestManagementCredentialMatrix 默认/admin/readonly/命名密钥在数据面与管理面的
// 作用域矩阵。默认 api_key 在配置管理凭据后失去管理面权限，admin/readonly 永远
// 不能进入 /v1/*。
func TestManagementCredentialMatrix(t *testing.T) {
	t.Parallel()

	store := apikey.New("")

	_, rawNamed, err := store.Create("client")
	if err != nil {
		t.Fatal(err)
	}

	handler := NewHandler(&Config{
		Pool:           testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}),
		Upstream:       newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true }),
		APIKey:         "data-key",
		AdminAPIKey:    "admin-key",
		ReadonlyAPIKey: "readonly-key",
		Keys:           store,
	})

	cases := []struct {
		name, method, path, key string
		want                    int
	}{
		{"默认密钥 status 拒绝", http.MethodGet, "/status", "data-key", http.StatusUnauthorized},
		{"admin status 通过", http.MethodGet, "/status", "admin-key", http.StatusOK},
		{"readonly status 通过", http.MethodGet, "/status", "readonly-key", http.StatusOK},
		{"无凭据 status 拒绝", http.MethodGet, "/status", "", http.StatusUnauthorized},
		{"默认密钥 v1 通过", http.MethodPost, "/v1/chat/completions", "data-key", http.StatusOK},
		{"命名密钥 v1 通过", http.MethodPost, "/v1/chat/completions", rawNamed, http.StatusOK},
		{"admin 不得进入 v1", http.MethodPost, "/v1/chat/completions", "admin-key", http.StatusUnauthorized},
		{"readonly 不得进入 v1", http.MethodPost, "/v1/chat/completions", "readonly-key", http.StatusUnauthorized},
		{"readonly pprof 非 GET 403", http.MethodPost, "/debug/pprof/", "readonly-key", http.StatusForbidden},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			t.Parallel()

			rec := httptest.NewRecorder()

			req := httptest.NewRequest(item.method, item.path, nil)
			if item.method == http.MethodPost && strings.HasPrefix(item.path, "/v1/") {
				req = httptest.NewRequest(item.method, item.path,
					strings.NewReader(`{"model":"glm-5.2","messages":[]}`))
			}

			if item.key != "" {
				req.Header.Set("Authorization", "Bearer "+item.key)
			}

			handler.ServeHTTP(rec, req)

			if rec.Code != item.want {
				t.Fatalf("code=%d want %d body=%s", rec.Code, item.want, rec.Body)
			}

			body := rec.Body.String()
			for _, secret := range []string{"data-key", "admin-key", "readonly-key", rawNamed} {
				if strings.Contains(body, secret) {
					t.Fatalf("错误体泄露密钥: %s", body)
				}
			}
		})
	}
}

// TestManagementFallbackSingleKey 两者都为空时保持现状：api_key 兼管理面。
func TestManagementFallbackSingleKey(t *testing.T) {
	t.Parallel()

	handler := NewHandler(&Config{
		Pool:   testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}),
		APIKey: "data-key",
	})

	if rec := doWithKey(handler, http.MethodGet, "/status", "data-key"); rec.Code != http.StatusOK {
		t.Fatalf("单密钥默认 status code=%d", rec.Code)
	}

	if rec := doWithKey(handler, http.MethodGet, "/status", "wrong"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("单密钥错误 code=%d", rec.Code)
	}
}

// TestManagementHotUpdate admin/readonly 经 livecfg 热生效，无需重启。
func TestManagementHotUpdate(t *testing.T) {
	t.Parallel()

	live := livecfg.New(livecfg.Snapshot{APIKey: "data-key", AdminAPIKey: "old-admin"})
	handler := NewHandler(&Config{
		Pool:   testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}),
		APIKey: "data-key",
		Live:   live,
	})

	if rec := doWithKey(handler, http.MethodGet, "/status", "old-admin"); rec.Code != http.StatusOK {
		t.Fatalf("旧 admin code=%d", rec.Code)
	}

	live.Store(livecfg.Snapshot{
		APIKey: "data-key", AdminAPIKey: "new-admin", ReadonlyAPIKey: "new-readonly",
	})

	if rec := doWithKey(handler, http.MethodGet, "/status", "old-admin"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("旧 admin 应失效 code=%d", rec.Code)
	}

	if rec := doWithKey(handler, http.MethodGet, "/status", "new-admin"); rec.Code != http.StatusOK {
		t.Fatalf("新 admin code=%d", rec.Code)
	}

	if rec := doWithKey(handler, http.MethodGet, "/status", "new-readonly"); rec.Code != http.StatusOK {
		t.Fatalf("新 readonly code=%d", rec.Code)
	}

	if rec := doWithKey(handler, http.MethodGet, "/status", "data-key"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("配置管理凭据后默认密钥应失去管理面 code=%d", rec.Code)
	}
}

// TestSharedManagementLimiter 管理面与运维面共用同一个 Limiter：server 侧失败
// 计数会直接让同实例的 panel 进入 429。
func TestSharedManagementLimiter(t *testing.T) {
	t.Parallel()

	limiter := httpauth.NewLimiter(3, time.Minute)
	handler := NewHandler(&Config{
		Pool:        testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}),
		APIKey:      "secret",
		AuthLimiter: limiter,
	})
	pnl := panel.New(&panel.Config{APIKey: "secret", AuthLimiter: limiter})

	const ip = "198.51.100.77:1000"

	for i := range 3 {
		req := httptest.NewRequest(http.MethodGet, "/status", nil)
		req.RemoteAddr = ip
		req.Header.Set("Authorization", "Bearer wrong")

		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("第 %d 次失败 code=%d", i+1, rec.Code)
		}
	}

	req := httptest.NewRequest(http.MethodGet, "/panel/api/overview", nil)
	req.RemoteAddr = ip
	req.Header.Set("Authorization", "Bearer wrong")

	rec := httptest.NewRecorder()
	pnl.ServeHTTP(rec, req)

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("共享预算未生效 code=%d body=%s", rec.Code, rec.Body)
	}
}
