package upstream

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
)

// TestRefreshSnapshotKeepsRealm refresh 的出站快照必须保留显式 realm。
//
// 登录路径（panel login / 导入）只写 auth.realm=global，Domain 可能为空；手写字段表
// 的快照漏了 realm，Realm() 回落 cn ⇒ refresh 打到 CN origin 并按 CN 头族发，
// global 账号的 token 刷新必然失败（并可能被判 session dead）。
func TestRefreshSnapshotKeepsRealm(t *testing.T) {
	t.Parallel()

	var gotPath, gotDomain, gotGlobalHeader string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotDomain = r.Header.Get("X-Domain")
		gotGlobalHeader = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{"accessToken":"at2","refreshToken":"rt2","expiresIn":3600}}`))
	}))
	t.Cleanup(server.Close)

	client := New()
	// 两域 base 都钉到同一个 httptest，断言靠路径/头区分「走了哪个域」。
	client.ChatBaseGlobal = server.URL
	client.ChatBaseCN = server.URL
	client.ChatBaseGlobal = server.URL
	client.RetryDelay = time.Millisecond

	account := &auth.Auth{UID: "u-global", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 1}
	if _, err := auth.BackfillRealmFor(account, "global"); err != nil {
		t.Fatal(err)
	}

	if got := account.Realm(); got != "global" {
		t.Fatalf("前置：账号 realm=%q want global", got)
	}

	if err := client.RefreshToken(account); err != nil {
		t.Fatalf("RefreshToken: %v", err)
	}

	if gotPath != "/v2/plugin/auth/token/refresh" {
		t.Fatalf("path=%q want /v2/plugin/auth/token/refresh", gotPath)
	}
	// global 账号无 domain 时 refresh 头族固定回落 www.workbuddy.ai（headers.go 的
	// surfaceRefresh 分支）；判成 cn 时会回落本次请求 host，等于泄出测试主机名。
	if gotDomain != "www.workbuddy.ai" {
		t.Fatalf("X-Domain=%q want www.workbuddy.ai（说明 refresh 快照按 global 判域）", gotDomain)
	}

	if gotGlobalHeader != "Bearer at" {
		t.Fatalf("Authorization=%q want Bearer at", gotGlobalHeader)
	}
}

// TestRefreshSnapshotKeepsRealmRouting 出口选路同样依赖快照的 realm：
// 只配 realm 默认槽时，global 账号的 refresh 必须落到 global 槽。
func TestRefreshSnapshotKeepsRealmRouting(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{"accessToken":"at2","refreshToken":"rt2","expiresIn":3600}}`))
	}))
	t.Cleanup(server.Close)

	client := New()
	client.ChatBaseCN = server.URL
	client.ChatBaseGlobal = server.URL
	client.RetryDelay = time.Millisecond

	var sawRealm []string

	probe := &egressProbe{}

	client.SetAccountRoute(func(a *auth.Auth) (string, testDial) {
		sawRealm = append(sawRealm, a.Realm())

		return "global-slot", probe.dial
	})

	account := &auth.Auth{UID: "u-global2", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 1}
	if _, err := auth.BackfillRealmFor(account, "global"); err != nil {
		t.Fatal(err)
	}

	if err := client.RefreshToken(account); err != nil {
		t.Fatalf("RefreshToken: %v", err)
	}

	if probe.calls.Load() == 0 {
		t.Fatal("refresh 未经账号出口（说明快照丢了 realm，选路拿不到槽）")
	}

	for _, realm := range sawRealm {
		if realm != "global" {
			t.Fatalf("选路看到的 realm=%q want global（快照必须带 realm）", realm)
		}
	}
}
