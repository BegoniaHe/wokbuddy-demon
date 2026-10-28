// device_token_test.go X-Device-Token 注入 + 文件兜底读取单测。
package upstream

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
)

// TestDeviceTokenInjectedOnChatAndCheckin 真机 2026-10-28 抓包：X-Device-Token 出现在
// chat（/v2/chat/completions）与签到（daily-checkin / checkin-activity-status）；
// report / billing meter / busy / v3-config / refresh / growth / agenttool 都不带
// （#342 的「只在 auth/state 与签到」对 chat 不成立，见 #376）。
func TestDeviceTokenInjectedOnChatAndCheckin(t *testing.T) {
	t.Parallel()

	account := &auth.Auth{AccessToken: "at", UID: "u1", DeviceToken: "tok-from-auth"}

	var mu sync.Mutex

	seen := map[string]string{}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen[r.URL.Path] = r.Header.Get("X-Device-Token")
		mu.Unlock()

		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"code":0,"data":{"today_checked_in":true}}`))
	}))
	defer srv.Close()

	client := &Client{
		HTTP:          srv.Client(),
		ChatHTTP:      srv.Client(),
		ChatBaseCN:    srv.URL,
		BillingBaseCN: srv.URL,
	}

	if err := client.DailyCheckin(account); err != nil {
		t.Fatalf("daily-checkin: %v", err)
	}

	if _, err := client.CheckinActivityStatus(account); err != nil {
		t.Fatalf("checkin-activity-status: %v", err)
	}

	for _, surface := range []struct {
		path  string
		apply func(*http.Request)
	}{
		{"/v2/chat/completions", func(req *http.Request) { client.ChatHeaders(req, account, "", ChatMeta{}) }},
		{"/v2/report", func(req *http.Request) { client.ReportHeaders(req, account) }},
		{"/billing/meter/get-user-resource", func(req *http.Request) { client.BillingHeaders(req, account) }},
		{"/v2/chat/busy", func(req *http.Request) { client.ModelBusyHeaders(req, account) }},
		{"/v3/config", func(req *http.Request) { client.V3ConfigHeaders(req, account, "") }},
		{"/v2/plugin/auth/token/refresh", func(req *http.Request) { client.RefreshHeaders(req, account) }},
		{"/v2/activity/growth/buddy/info", func(req *http.Request) { client.GrowthHeaders(req, account) }},
		{"/agenttool/v1/search", func(req *http.Request) {
			client.applyHeaders(surfaceSearch, req, account, "", ChatMeta{})
		}},
	} {
		req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, srv.URL+surface.path, nil)
		surface.apply(req)

		resp, err := client.HTTP.Do(req)
		if err != nil {
			t.Fatalf("%s do: %v", surface.path, err)
		}

		resp.Body.Close()
	}

	want := map[string]string{
		"/v2/billing/meter/daily-checkin":           "tok-from-auth",
		"/v2/billing/meter/checkin-activity-status": "tok-from-auth",
		"/v2/chat/completions":                      "tok-from-auth",
		"/v2/report":                                "",
		"/billing/meter/get-user-resource":          "",
		"/v2/chat/busy":                             "",
		"/v3/config":                                "",
		"/v2/plugin/auth/token/refresh":             "",
		"/v2/activity/growth/buddy/info":            "",
		"/agenttool/v1/search":                      "",
	}

	for path, wantTok := range want {
		if got, ok := seen[path]; !ok {
			t.Errorf("%s: 未观察到请求", path)
		} else if got != wantTok {
			t.Errorf("%s: X-Device-Token = %q want %q", path, got, wantTok)
		}
	}
}

// TestDeviceTokenNotInjected_WhenEmpty 所有来源皆空时签到也不注入该头。
func TestDeviceTokenNotInjected_WhenEmpty(t *testing.T) {
	t.Parallel()

	account := &auth.Auth{AccessToken: "at", UID: "u1"} // DeviceToken 空

	var got string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("X-Device-Token")

		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"code":0}`))
	}))
	defer srv.Close()

	client := &Client{
		HTTP:          srv.Client(),
		ChatHTTP:      srv.Client(),
		ChatBaseCN:    srv.URL,
		BillingBaseCN: srv.URL,
		// DeviceToken / DeviceTokenFile 皆空
	}

	if err := client.DailyCheckin(account); err != nil {
		t.Fatalf("daily-checkin: %v", err)
	}

	if got != "" {
		t.Errorf("X-Device-Token = %q want empty (not injected)", got)
	}
}

// TestDeviceTokenFromConfigOrFile_Overrides 优先级：auth > config > 文件（签到注入面）。
// auth 有值时覆盖 config；auth 空时 config 兜底；auth 与 config 皆空时读文件。
func TestDeviceTokenFromConfigOrFile_Overrides(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	filePath := filepath.Join(dir, "device_token")
	if err := os.WriteFile(filePath, []byte("tok-from-file\n"), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}

	cases := []struct {
		name string
		auth string
		cfg  string
		want string
	}{
		{"auth_over_config", "tok-auth", "tok-config", "tok-auth"},
		{"config_when_auth_empty", "", "tok-config", "tok-config"},
		{"file_when_auth_and_config_empty", "", "", "tok-from-file"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			// 每个用例用独立缓存：device token 文件缓存 5 分钟，case 间会串扰。
			save := resetDeviceTokenFileCache(filePath)
			defer save()

			account := &auth.Auth{AccessToken: "at", UID: "u1", DeviceToken: testCase.auth}

			var got string

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				got = r.Header.Get("X-Device-Token")

				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{"code":0}`))
			}))
			defer srv.Close()

			client := &Client{
				HTTP:            srv.Client(),
				ChatHTTP:        srv.Client(),
				ChatBaseCN:      srv.URL,
				BillingBaseCN:   srv.URL,
				DeviceToken:     testCase.cfg,
				DeviceTokenFile: filePath,
			}

			if err := client.DailyCheckin(account); err != nil {
				t.Fatalf("daily-checkin: %v", err)
			}

			if got != testCase.want {
				t.Errorf("X-Device-Token = %q want %q", got, testCase.want)
			}
		})
	}
}

// TestDeviceTokenFileTooLarge 文件超过 1KB 时忽略不注入。
func TestDeviceTokenFileTooLarge(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	filePath := filepath.Join(dir, "device_token")
	if err := os.WriteFile(filePath, []byte(strings.Repeat("x", 2048)), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}

	save := resetDeviceTokenFileCache(filePath)
	defer save()

	a := &auth.Auth{AccessToken: "at", UID: "u1"}

	c := &Client{DeviceTokenFile: filePath}
	if tok := c.resolveDeviceToken(a); tok != "" {
		t.Errorf("resolveDeviceToken() = %q want empty (file too large)", tok)
	}
}

// resetDeviceTokenFileCache 替换全局 device token 文件缓存并返回恢复函数。
// 文件缓存 5 分钟 TTL，测试间需清空避免串扰。
func resetDeviceTokenFileCache(path string) (restore func()) {
	dtFileCache.mu.Lock()
	origPath := dtFileCache.path
	origTok := dtFileCache.token
	origRead := dtFileCache.readAt
	origErr := dtFileCache.lastErr
	dtFileCache.path = path
	dtFileCache.token = ""
	dtFileCache.readAt = time.Time{}
	dtFileCache.lastErr = nil
	dtFileCache.mu.Unlock()

	var once sync.Once

	return func() {
		once.Do(func() {
			dtFileCache.mu.Lock()
			dtFileCache.path = origPath
			dtFileCache.token = origTok
			dtFileCache.readAt = origRead
			dtFileCache.lastErr = origErr
			dtFileCache.mu.Unlock()
		})
	}
}
