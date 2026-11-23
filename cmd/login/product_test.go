package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/BegoniaHe/wokbuddy-demon/internal/clientprofile"
)

// loginHit 记录一次设备授权请求的路径、查询与头，供头集断言使用。
type loginHit struct {
	path   string
	query  string
	header http.Header
}

// recordLoginFlow 起一个覆盖 state/token/account 的端点，跑完 url+poll，返回命中序列。
func recordLoginFlow(t *testing.T, realm, platform, origin string) []loginHit {
	t.Helper()

	var hits []loginHit

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits = append(hits, loginHit{path: r.URL.Path, query: r.URL.RawQuery, header: r.Header.Clone()})

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

	runURL(srv.URL, origin, realm, statePath, identity, srv.Client(), "", io.Discard)

	if _, err := os.Stat(statePath); err != nil {
		t.Fatalf("state 未落盘: %v", err)
	}

	runPoll(srv.URL, origin, realm, statePath, identity, srv.Client(), "", io.Discard)

	return hits
}

// assertLoginProductHeaders 断言单条设备授权请求的产品头、来源域与未登录态声明头。
// platform 非空时还核对 auth/state 的查询参数，避免两域身份串用。
func assertLoginProductHeaders(
	t *testing.T, hit loginHit, wantPath, wantOrigin, wantNoAuth, platform string, bearer bool,
) {
	t.Helper()

	if hit.path != wantPath {
		t.Errorf("path=%s want %s", hit.path, wantPath)

		return
	}

	if platform != "" && hit.query != "platform="+platform {
		t.Errorf("%s query=%q want platform=%s", wantPath, hit.query, platform)
	}

	assertLoginHeader(t, hit.header, "X-Product", loginProductSaaS, wantPath)
	assertLoginHeader(t, hit.header, "Origin", wantOrigin, wantPath)
	assertLoginHeader(t, hit.header, "Referer", wantOrigin+"/", wantPath)
	assertLoginHeader(t, hit.header, "X-No-Authorization", wantNoAuth, wantPath)

	if bearer {
		assertLoginHeader(t, hit.header, "Authorization", "Bearer tok", wantPath)
	}

	for _, name := range []string{"X-No-User-Id", "X-No-Enterprise-Id", "X-No-Department-Info"} {
		assertLoginHeader(t, hit.header, name, "true", wantPath)
	}
}

// TestLoginStateURLJourneyParams 授权链接补齐 version 与 loginSessionId（issue #534）：
// version 随部署 profile，loginSessionId 只在 CN 站（platform=workbuddy）追加。
func TestLoginStateURLJourneyParams(t *testing.T) {
	t.Parallel()

	cnIdentity := loginIdentity{
		platform:  "workbuddy",
		version:   "5.9.3",
		sessionID: "11111111-2222-4333-8444-555555555555",
	}

	cnQuery := mustLoginStateQuery(t, loginStateURL(upstreamBaseCN, cnIdentity))
	assertLoginStateParam(t, cnQuery, "platform", "workbuddy")
	assertLoginStateParam(t, cnQuery, "version", "5.9.3")
	assertLoginStateParam(t, cnQuery, "loginSessionId", cnIdentity.sessionID)

	global := loginIdentity{platform: "workbuddy-ai", version: "5.9.4"}
	globalQuery := mustLoginStateQuery(t, loginStateURL(upstreamBaseGlobal, global))
	assertLoginStateParam(t, globalQuery, "version", "5.9.4")

	if got := globalQuery.Get("loginSessionId"); got != "" {
		t.Errorf("global 链接不得带 loginSessionId（官方只在 CN 站追加），got %q", got)
	}
}

// TestLoginIdentityForJourneyParams 部署 profile 的 source.version 与 CN 站
// loginSessionId 必须落到登录身份上（仓库不内置版本常量）。
func TestLoginIdentityForJourneyParams(t *testing.T) {
	t.Parallel()

	profiles, err := clientprofile.LoadDir("../../internal/upstream/testdata/profile")
	if err != nil {
		t.Fatalf("load test profiles: %v", err)
	}

	cnIdentity := loginIdentityFor(profiles, realmCN)
	if cnIdentity.version != "9.9.9" {
		t.Errorf("cn version=%q want 9.9.9（来自 profile source.version）", cnIdentity.version)
	}

	if !loginUUIDv4Pattern.MatchString(cnIdentity.sessionID) {
		t.Errorf("cn loginSessionId=%q 不是 UUIDv4", cnIdentity.sessionID)
	}

	global := loginIdentityFor(profiles, realmGlobal)
	if global.version != "9.9.10" {
		t.Errorf("global version=%q want 9.9.10", global.version)
	}

	if global.sessionID != "" {
		t.Errorf("global loginSessionId=%q want 空（官方只在 CN 站生成）", global.sessionID)
	}
}

// mustLoginStateQuery 解析 auth/state 的查询参数。
func mustLoginStateQuery(t *testing.T, raw string) url.Values {
	t.Helper()

	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse state url %q: %v", raw, err)
	}

	return parsed.Query()
}

// assertLoginStateParam 断言 auth/state 查询参数取值。
func assertLoginStateParam(t *testing.T, query url.Values, key, want string) {
	t.Helper()

	if got := query.Get(key); got != want {
		t.Errorf("%s=%q want %q", key, got, want)
	}
}

// loginUUIDv4Pattern UUIDv4 字面量（version=4、variant=10xx）。
var loginUUIDv4Pattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

// assertLoginHeader 核对单个请求头；wantPath 只用于失败定位。
func assertLoginHeader(t *testing.T, header http.Header, name, want, wantPath string) {
	t.Helper()

	if got := header.Get(name); got != want {
		t.Errorf("%s %s=%q want %q", wantPath, name, got, want)
	}
}

// TestLoginDeviceAuthSendsXProduct CLI 设备授权三条端点与面板同口径：
// auth/state、auth/token 带 X-Product: SaaS 与四条 X-No-*；login/account 带
// X-Product: SaaS 与 Bearer，且不带 X-No-Authorization。CN / global 同一头。
func TestLoginDeviceAuthSendsXProduct(t *testing.T) {
	t.Parallel()

	cases := []struct {
		realm  string
		origin string
	}{
		{realm: realmCN, origin: originRefererCN},
		{realm: realmGlobal, origin: originRefererGlobal},
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

			_, origin := realmConfig(item.realm)
			if origin != item.origin {
				t.Fatalf("origin=%q want %q", origin, item.origin)
			}

			platform := authPlatform(item.realm)

			hits := recordLoginFlow(t, item.realm, platform, origin)
			if len(hits) != len(want) {
				t.Fatalf("hits=%d want %d", len(hits), len(want))
			}

			for i := range want {
				queryPlatform := ""
				if want[i].path == "/v2/plugin/auth/state" {
					queryPlatform = platform
				}

				assertLoginProductHeaders(
					t, hits[i], want[i].path, origin, want[i].wantNoAuth, queryPlatform, want[i].bearer,
				)
			}
		})
	}
}
