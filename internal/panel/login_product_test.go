package panel

import (
	"net/http"
	"testing"
)

// loginProductCase 一条设备授权请求：走面板真实入口，头期望随有无 Bearer 变化。
type loginProductCase struct {
	name       string
	wantPath   string
	wantNoAuth string
	bearer     bool
}

// TestLoginDeviceAuthSendsXProduct 设备授权三条端点都要带官方 REST 拦截器
// 默认补上的 X-Product: SaaS。无 Bearer 的 state/token 仍带四条 X-No-*；
// 带 Bearer 的 account 不发 X-No-Authorization。CN / global 同一产品头，
// 但请求必须打到各自的 base，Origin/Referer 跟随该 realm，platform 取 profile。
//
//nolint:paralleltest // 串行替换全局 loginHTTP，避免测试间互相干扰
func TestLoginDeviceAuthSendsXProduct(t *testing.T) {
	cases := []loginProductCase{
		{name: "auth/state", wantPath: "/v2/plugin/auth/state", wantNoAuth: "true"},
		{name: "auth/token", wantPath: "/v2/plugin/auth/token", wantNoAuth: "true"},
		{name: "login/account", wantPath: "/v2/plugin/login/account", bearer: true},
	}

	for _, realm := range []string{"cn", "global"} {
		for _, item := range cases {
			t.Run(realm+"/"+item.name, func(t *testing.T) {
				assertLoginProductHeaders(t, realm, item)
			})
		}
	}
}

// loginProductTarget 一条设备授权请求在指定 realm 下的期望地址与 Origin。
type loginProductTarget struct {
	url    string
	origin string
}

// assertLoginProductHeaders 用该 realm 的部署 profile 走真实登录入口，
// 断言产品头、未登录态声明头，以及地址没有串到另一域。
func assertLoginProductHeaders(t *testing.T, realm string, item loginProductCase) {
	t.Helper()

	panel := loginTestPanel(t)
	identity := panel.loginIdentityFor(realm)
	target := loginProductTargetFor(realm, identity, item)

	var got *http.Request

	replaceLoginHTTP(t, func(r *http.Request) (*http.Response, error) {
		got = r.Clone(r.Context())

		return loginResponse(http.StatusOK, loginProductBody(item)), nil
	})

	driveLoginProduct(t, panel, realm, identity.userAgent, item)

	if got == nil {
		t.Fatal("请求未发出")
	}

	checkLoginProductRequest(t, got, target, identity.platform, item)
}

// loginProductTargetFor 按真实入口拼出该 realm 的端点：state 带 profile platform，
// token / account 带固定 state。
func loginProductTargetFor(realm string, identity loginIdentity, item loginProductCase) loginProductTarget {
	endpoint := loginCandidatesWithIdentity(realm, identity)[0]
	origin := endpoint.origin

	switch item.wantPath {
	case "/v2/plugin/auth/state":
		return loginProductTarget{url: endpoint.state, origin: origin}
	case "/v2/plugin/auth/token":
		return loginProductTarget{url: endpoint.token + "s-1", origin: origin}
	default:
		return loginProductTarget{url: endpoint.account + "s-1", origin: origin}
	}
}

// loginProductBody 按入口返回能走完该步的最小信封。
func loginProductBody(item loginProductCase) string {
	switch item.wantPath {
	case "/v2/plugin/auth/state":
		return `{"code":0,"data":{"state":"s-1","authUrl":"https://example.invalid/auth"}}`
	case "/v2/plugin/auth/token":
		return `{"code":0,"data":{"accessToken":"tok","refreshToken":"ref","expiresIn":3600}}`
	default:
		return `{"code":0,"data":{"uid":"u-1","nickname":"n"}}`
	}
}

// driveLoginProduct 调面板真实入口，而不是直接打 doJSON。
func driveLoginProduct(t *testing.T, panel *Panel, realm, wantUA string, item loginProductCase) {
	t.Helper()

	switch item.wantPath {
	case "/v2/plugin/auth/state":
		if _, _, _, err := panel.loginStartState(t.Context(), realm); err != nil {
			t.Fatalf("loginStartState: %v", err)
		}
	case "/v2/plugin/auth/token":
		if _, _, pending, _, err := panel.pollLoginToken(t.Context(), realm, "", "s-1"); err != nil || pending {
			t.Fatalf("pollLoginToken pending=%v err=%v", pending, err)
		}
	default:
		endpoint := loginCandidatesWithIdentity(realm, panel.loginIdentityFor(realm))[0]
		if endpoint.userAgent != wantUA {
			t.Fatalf("ua=%q want %q", endpoint.userAgent, wantUA)
		}

		if _, err := panel.fetchLoginAccount(t.Context(), &endpoint, "s-1", "access-token"); err != nil {
			t.Fatalf("fetchLoginAccount: %v", err)
		}
	}
}

// checkLoginProductRequest 核对单条已发出请求的地址、产品头与未登录态声明头。
func checkLoginProductRequest(
	t *testing.T, got *http.Request, target loginProductTarget, platform string, item loginProductCase,
) {
	t.Helper()

	if got.URL.String() != target.url {
		t.Errorf("url=%s want %s", got.URL.String(), target.url)
	}

	if item.wantPath == "/v2/plugin/auth/state" && got.URL.Query().Get("platform") != platform {
		t.Errorf("platform=%q want %q", got.URL.Query().Get("platform"), platform)
	}

	assertPanelLoginHeader(t, got.Header, "X-Product", loginProductSaaS)
	assertPanelLoginHeader(t, got.Header, "Origin", target.origin)
	assertPanelLoginHeader(t, got.Header, "Referer", target.origin+"/")
	assertPanelLoginHeader(t, got.Header, "X-No-Authorization", item.wantNoAuth)

	if got.Header.Get("User-Agent") == "" {
		t.Error("User-Agent 为空，登录入口未带上 profile loginUA")
	}

	if item.bearer {
		assertPanelLoginHeader(t, got.Header, "Authorization", "Bearer access-token")
	}

	for _, name := range []string{"X-No-User-Id", "X-No-Enterprise-Id", "X-No-Department-Info"} {
		assertPanelLoginHeader(t, got.Header, name, "true")
	}
}

// assertPanelLoginHeader 核对单个请求头。
func assertPanelLoginHeader(t *testing.T, header http.Header, name, want string) {
	t.Helper()

	if got := header.Get(name); got != want {
		t.Errorf("%s=%q want %q", name, got, want)
	}
}
