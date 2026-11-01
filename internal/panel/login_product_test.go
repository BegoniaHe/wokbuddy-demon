package panel

import (
	"net/http"
	"strings"
	"testing"
)

// loginProductCase 一条设备授权请求：路径相对 realm base，头期望随有无 Bearer 变化。
// state 为真时路径要带该 realm 的 platform 查询参数。
type loginProductCase struct {
	name       string
	method     string
	path       string
	state      bool
	bearer     string
	wantNoAuth string
}

// TestLoginDeviceAuthSendsXProduct 设备授权三条端点都要带官方 REST 拦截器
// 默认补上的 X-Product: SaaS。无 Bearer 的 state/token 仍带四条 X-No-*；
// 带 Bearer 的 account 不发 X-No-Authorization。CN / global 同一产品头，
// 但请求必须打到各自的 base，Origin/Referer 跟随该 realm。
//
//nolint:paralleltest // 串行替换全局 loginHTTP，避免测试间互相干扰
func TestLoginDeviceAuthSendsXProduct(t *testing.T) {
	cases := []loginProductCase{
		{
			name:       "auth/state",
			method:     http.MethodPost,
			path:       "/v2/plugin/auth/state",
			state:      true,
			wantNoAuth: "true",
		},
		{
			name:       "auth/token",
			method:     http.MethodGet,
			path:       "/v2/plugin/auth/token?state=s",
			wantNoAuth: "true",
		},
		{
			name:   "login/account",
			method: http.MethodGet,
			path:   "/v2/plugin/login/account?state=s",
			bearer: "access-token",
		},
	}

	for _, realm := range []string{"cn", "global"} {
		for _, item := range cases {
			t.Run(realm+"/"+item.name, func(t *testing.T) {
				assertLoginProductHeaders(t, realm, item)
			})
		}
	}
}

// loginProductTarget 一条设备授权请求在指定 realm 下的实际上游地址与 Origin。
type loginProductTarget struct {
	url    string
	origin string
}

// loginProductTargetFor 把用例路径接到该 realm 的 base，state 再带上 platform。
func loginProductTargetFor(realm string, item loginProductCase) loginProductTarget {
	base := upstreamBaseCN
	origin := originRefererCN

	if realm == "global" {
		base = upstreamBaseGlobal
		origin = originRefererGlobal
	}

	path := item.path
	if item.state {
		path += "?platform=" + defaultLoginIdentity(realm).platform
	}

	return loginProductTarget{url: base + path, origin: origin}
}

// assertLoginProductHeaders 用该 realm 的真实 base 与 Origin 打一条设备授权请求，
// 断言产品头、未登录态声明头，以及 Origin/Referer 没有串到另一域。
func assertLoginProductHeaders(t *testing.T, realm string, item loginProductCase) {
	t.Helper()

	target := loginProductTargetFor(realm, item)

	var got *http.Request

	replaceLoginHTTP(t, func(r *http.Request) (*http.Response, error) {
		got = r.Clone(r.Context())

		return loginResponse(http.StatusOK, `{"code":0,"data":{}}`), nil
	})

	_, _, err := doJSON(item.method, target.url, item.bearer, strings.NewReader("{}"), target.origin, "ua")
	if err != nil {
		t.Fatalf("doJSON: %v", err)
	}

	if got == nil {
		t.Fatal("请求未发出")
	}

	checkLoginProductRequest(t, got, target, item)
}

// checkLoginProductRequest 核对单条已发出请求的地址、产品头与未登录态声明头。
func checkLoginProductRequest(t *testing.T, got *http.Request, target loginProductTarget, item loginProductCase) {
	t.Helper()

	if got.URL.String() != target.url {
		t.Errorf("url=%s want %s", got.URL.String(), target.url)
	}

	if got := got.Header.Get("X-Product"); got != "SaaS" {
		t.Errorf("X-Product=%q want SaaS", got)
	}

	if got := got.Header.Get("Origin"); got != target.origin {
		t.Errorf("Origin=%q want %q", got, target.origin)
	}

	if got := got.Header.Get("Referer"); got != target.origin+"/" {
		t.Errorf("Referer=%q want %q", got, target.origin+"/")
	}

	if got := got.Header.Get("X-No-Authorization"); got != item.wantNoAuth {
		t.Errorf("X-No-Authorization=%q want %q", got, item.wantNoAuth)
	}

	if item.bearer != "" && got.Header.Get("Authorization") != "Bearer "+item.bearer {
		t.Errorf("Authorization=%q want Bearer %s", got.Header.Get("Authorization"), item.bearer)
	}

	for _, name := range []string{"X-No-User-Id", "X-No-Enterprise-Id", "X-No-Department-Info"} {
		if got := got.Header.Get(name); got != "true" {
			t.Errorf("%s=%q want true", name, got)
		}
	}
}
