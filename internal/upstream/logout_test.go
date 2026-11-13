// logout_test.go 钉住撤销登录会话面（issue #513）：路径、body、三件套头与
// 「不带 Authorization」的安全边界。
package upstream

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
)

// TestLogoutRequestShape cn 账号：POST {chat base}/v2/plugin/auth/logout，body `{}`，
// 只带 X-Refresh-Token / X-Auth-Refresh-Source / X-No-Authorization 三件套，
// 不带 Authorization（官方 logout 只认 refreshToken）。
func TestLogoutRequestShape(t *testing.T) {
	t.Parallel()

	var (
		gotMethod string
		gotPath   string
		gotBody   string
		gotHeader http.Header
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		gotBody = string(raw)
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotHeader = r.Header.Clone()

		_, _ = w.Write([]byte(`{"code":0}`))
	}))
	t.Cleanup(srv.Close)

	client := New()
	client.ChatBaseCN = srv.URL
	account := &auth.Auth{
		UID: "u1", AccessToken: "at", RefreshToken: "rt", Domain: "www.workbuddy.cn",
	}

	if err := client.Logout(context.Background(), account); err != nil {
		t.Fatalf("logout: %v", err)
	}

	if gotMethod != http.MethodPost || gotPath != "/v2/plugin/auth/logout" {
		t.Errorf("%s %s want POST /v2/plugin/auth/logout", gotMethod, gotPath)
	}

	if gotBody != "{}" {
		t.Errorf("logout body=%q want {}", gotBody)
	}

	for name, want := range map[string]string{
		"X-Refresh-Token":       "rt",
		"X-Auth-Refresh-Source": "plugin",
		"X-No-Authorization":    "true",
	} {
		if got := gotHeader.Get(name); got != want {
			t.Errorf("%s=%q want %q", name, got, want)
		}
	}

	if got := gotHeader.Get("Authorization"); got != "" {
		t.Errorf("logout 不应带 Authorization，got %q", got)
	}
}

// TestLogoutGlobalRealm global 账号走 global chat base，路径不变。
func TestLogoutGlobalRealm(t *testing.T) {
	t.Parallel()

	var gotPath string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = w.Write([]byte(`{"code":0}`))
	}))
	t.Cleanup(srv.Close)

	client := New()
	client.ChatBaseGlobal = srv.URL

	account := &auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt"}
	if _, err := auth.BackfillRealmFor(account, "global"); err != nil {
		t.Fatalf("BackfillRealmFor: %v", err)
	}

	if err := client.Logout(context.Background(), account); err != nil {
		t.Fatalf("logout: %v", err)
	}

	if gotPath != "/v2/plugin/auth/logout" {
		t.Errorf("path=%q want /v2/plugin/auth/logout", gotPath)
	}
}

// TestLogoutWithoutRefreshToken 无 RT 时不发请求（已无可撤销对象）。
func TestLogoutWithoutRefreshToken(t *testing.T) {
	t.Parallel()

	called := false

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		_, _ = w.Write([]byte(`{"code":0}`))
	}))
	t.Cleanup(srv.Close)

	client := New()
	client.ChatBaseCN = srv.URL

	err := client.Logout(context.Background(), &auth.Auth{UID: "u1", AccessToken: "at"})
	if !errors.Is(err, errNoRefreshToken) {
		t.Fatalf("err=%v want errNoRefreshToken", err)
	}

	if called {
		t.Error("无 RT 不应发起上游请求")
	}
}

// TestLogoutUpstreamFailure 非 2xx 带回状态码，供面板区分「撤销失败」与「凭证已失效」。
func TestLogoutUpstreamFailure(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"code":10000,"msg":"internal error"}`))
	}))
	t.Cleanup(srv.Close)

	client := New()
	client.ChatBaseCN = srv.URL

	err := client.Logout(context.Background(), &auth.Auth{
		UID: "u1", AccessToken: "at", RefreshToken: "rt", Domain: "www.workbuddy.cn",
	})

	var ue *Error
	if !errors.As(err, &ue) || ue.Status != http.StatusInternalServerError {
		t.Fatalf("err=%v want *Error status 500", err)
	}
}
