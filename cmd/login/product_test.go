package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// loginHit 记录一次设备授权请求的路径与头，供头集断言使用。
type loginHit struct {
	path   string
	header http.Header
}

// recordLoginFlow 起一个覆盖 state/token/account 的端点，跑完 url+poll，返回命中序列。
func recordLoginFlow(t *testing.T, realm, platform, origin string) []loginHit {
	t.Helper()

	var hits []loginHit

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits = append(hits, loginHit{path: r.URL.Path, header: r.Header.Clone()})

		switch r.URL.Path {
		case "/v2/plugin/auth/state":
			_, _ = w.Write([]byte(`{"code":0,"data":{"state":"s-1","authUrl":"https://example.invalid/auth"}}`))
		case "/v2/plugin/auth/token":
			_, _ = w.Write([]byte(`{"code":0,"data":{"accessToken":"tok","refreshToken":"ref","expiresIn":3600}}`))
		case "/v2/plugin/login/account":
			_, _ = w.Write([]byte(`{"code":0,"data":{"uid":"u-1","nickname":"n"}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	statePath := filepath.Join(t.TempDir(), "state.json")
	identity := loginIdentity{userAgent: "ua", platform: platform}

	runURL(srv.URL, origin, realm, statePath, identity, srv.Client(), io.Discard)

	if _, err := os.Stat(statePath); err != nil {
		t.Fatalf("state 未落盘: %v", err)
	}

	runPoll(srv.URL, origin, realm, statePath, identity, srv.Client(), io.Discard)

	return hits
}

// assertLoginProductHeaders 断言单条设备授权请求的产品头与未登录态声明头。
func assertLoginProductHeaders(t *testing.T, hit loginHit, wantPath, wantNoAuth string, bearer bool) {
	t.Helper()

	if hit.path != wantPath {
		t.Errorf("path=%s want %s", hit.path, wantPath)

		return
	}

	header := hit.header
	if got := header.Get("X-Product"); got != "SaaS" {
		t.Errorf("%s X-Product=%q want SaaS", wantPath, got)
	}

	if got := header.Get("X-No-Authorization"); got != wantNoAuth {
		t.Errorf("%s X-No-Authorization=%q want %q", wantPath, got, wantNoAuth)
	}

	if bearer && header.Get("Authorization") != "Bearer tok" {
		t.Errorf("%s Authorization=%q want Bearer tok", wantPath, header.Get("Authorization"))
	}

	for _, name := range []string{"X-No-User-Id", "X-No-Enterprise-Id", "X-No-Department-Info"} {
		if got := header.Get(name); got != "true" {
			t.Errorf("%s %s=%q want true", wantPath, name, got)
		}
	}
}

// TestLoginDeviceAuthSendsXProduct CLI 设备授权三条端点与面板同口径：
// auth/state、auth/token 带 X-Product: SaaS 与四条 X-No-*；login/account 带
// X-Product: SaaS 与 Bearer，且不带 X-No-Authorization。CN / global 同一头。
func TestLoginDeviceAuthSendsXProduct(t *testing.T) {
	t.Parallel()

	cases := []struct {
		realm    string
		platform string
		origin   string
	}{
		{realm: realmCN, platform: "workbuddy", origin: "https://www.codebuddy.cn"},
		{realm: "global", platform: "workbuddy-ai", origin: "https://www.workbuddy.ai"},
	}
	want := []struct {
		path       string
		wantNoAuth string
		bearer     bool
	}{
		{path: "/v2/plugin/auth/state", wantNoAuth: "true"},
		{path: "/v2/plugin/auth/token", wantNoAuth: "true"},
		{path: "/v2/plugin/login/account", bearer: true},
	}

	for _, item := range cases {
		t.Run(item.realm, func(t *testing.T) {
			t.Parallel()

			hits := recordLoginFlow(t, item.realm, item.platform, item.origin)
			if len(hits) < len(want) {
				t.Fatalf("hits=%d want %d", len(hits), len(want))
			}

			for i := range want {
				assertLoginProductHeaders(t, hits[i], want[i].path, want[i].wantNoAuth, want[i].bearer)
			}
		})
	}
}
