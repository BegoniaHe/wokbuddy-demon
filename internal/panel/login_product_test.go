package panel

import (
	"net/http"
	"strings"
	"testing"
)

// TestLoginDeviceAuthSendsXProduct 设备授权三条端点都要带官方 REST 拦截器
// 默认补上的 X-Product: SaaS。无 Bearer 的 state/token 仍带四条 X-No-*；
// 带 Bearer 的 account 不发 X-No-Authorization。CN / global 同一头。
//
//nolint:paralleltest // 串行替换全局 loginHTTP，避免测试间互相干扰
func TestLoginDeviceAuthSendsXProduct(t *testing.T) {
	realms := []string{"cn", "global"}
	cases := []struct {
		name       string
		method     string
		url        string
		bearer     string
		wantNoAuth string
	}{
		{
			name:       "auth/state",
			method:     http.MethodPost,
			url:        "https://www.workbuddy.cn/v2/plugin/auth/state?platform=workbuddy",
			wantNoAuth: "true",
		},
		{
			name:       "auth/token",
			method:     http.MethodGet,
			url:        "https://www.workbuddy.cn/v2/plugin/auth/token?state=s",
			wantNoAuth: "true",
		},
		{
			name:   "login/account",
			method: http.MethodGet,
			url:    "https://www.workbuddy.ai/v2/plugin/login/account?state=s",
			bearer: "access-token",
		},
	}

	for _, realm := range realms {
		for _, item := range cases {
			t.Run(realm+"/"+item.name, func(t *testing.T) {
				var got *http.Request

				replaceLoginHTTP(t, func(r *http.Request) (*http.Response, error) {
					got = r.Clone(r.Context())

					return loginResponse(http.StatusOK, `{"code":0,"data":{}}`), nil
				})

				origin := originRefererCN
				if realm == "global" {
					origin = originRefererGlobal
				}

				_, _, err := doJSON(item.method, item.url, item.bearer, strings.NewReader("{}"), origin, "ua")
				if err != nil {
					t.Fatalf("doJSON: %v", err)
				}

				if got == nil {
					t.Fatal("请求未发出")
				}

				if got := got.Header.Get("X-Product"); got != "SaaS" {
					t.Errorf("X-Product=%q want SaaS", got)
				}

				if got := got.Header.Get("X-No-Authorization"); got != item.wantNoAuth {
					t.Errorf("X-No-Authorization=%q want %q", got, item.wantNoAuth)
				}

				for _, name := range []string{"X-No-User-Id", "X-No-Enterprise-Id", "X-No-Department-Info"} {
					if got := got.Header.Get(name); got != "true" {
						t.Errorf("%s=%q want true", name, got)
					}
				}
			})
		}
	}
}
