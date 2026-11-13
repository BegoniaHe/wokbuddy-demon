// account_logout_test.go 钉住面板「撤销登录会话」（issue #513）：成功后出池并删凭证，
// 上游失败时不动本地凭证，凭证已失效时直接清本地。
package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
	"github.com/BegoniaHe/wokbuddy-demon/internal/pool"
	"github.com/BegoniaHe/wokbuddy-demon/internal/upstream"
)

// logoutFixture 构造一个带真凭证文件的面板与假上游（status 决定上游应答）。
func logoutFixture(t *testing.T, status int) (*Panel, *pool.Pool, *auth.Auth, *bool) {
	t.Helper()

	called := false

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/plugin/auth/logout" {
			http.Error(w, "not found", http.StatusNotFound)

			return
		}

		called = true

		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"code":0,"msg":"ok"}`))
	}))
	t.Cleanup(srv.Close)

	path := filepath.Join(t.TempDir(), "u1.json")
	if err := os.WriteFile(path, []byte(`{"uid":"u1","accessToken":"at","refreshToken":"rt"}`), 0o600); err != nil {
		t.Fatalf("write auth file: %v", err)
	}

	state := pool.New("")
	account := &auth.Auth{
		UID: "u1", AccessToken: "at", RefreshToken: "rt", Domain: "www.workbuddy.cn",
		ExpiresAt: 9999999999, FilePath: path,
	}
	state.Add(account)

	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL}
	panel := &Panel{cfg: Config{Pool: state, Upstream: up}}

	return panel, state, account, &called
}

func invokeLogout(panel *Panel, uid string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/panel/api/accounts/"+uid+"/logout", nil)
	req.SetPathValue("uid", uid)

	rec := httptest.NewRecorder()
	panel.accountLogout(rec, req)

	return rec
}

// TestAccountLogoutSuccess 上游 2xx：账号出池、凭证文件与 .bak 删除、回执 ok。
func TestAccountLogoutSuccess(t *testing.T) {
	t.Parallel()

	panel, state, account, called := logoutFixture(t, http.StatusOK)

	rec := invokeLogout(panel, "u1")
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}

	if !*called {
		t.Error("应调用上游 logout")
	}

	if state.AuthByUID("u1") != nil {
		t.Error("账号应已出池")
	}

	if _, err := os.Stat(account.FilePath); !os.IsNotExist(err) {
		t.Errorf("凭证文件应已删除，stat err=%v", err)
	}
}

// TestAccountLogoutUpstreamFailure 上游失败：非 2xx，且池与凭证文件保持原样。
func TestAccountLogoutUpstreamFailure(t *testing.T) {
	t.Parallel()

	panel, state, account, _ := logoutFixture(t, http.StatusInternalServerError)

	rec := invokeLogout(panel, "u1")
	if rec.Code < http.StatusBadRequest {
		t.Fatalf("code=%d want 非 2xx", rec.Code)
	}

	if state.AuthByUID("u1") == nil {
		t.Error("撤销失败时账号不应出池")
	}

	if _, err := os.Stat(account.FilePath); err != nil {
		t.Errorf("撤销失败时凭证文件应保留，stat err=%v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v body=%s", err, rec.Body.String())
	}

	if _, ok := got["error"]; !ok {
		t.Errorf("应有错误文案，body=%s", rec.Body.String())
	}
}

// TestAccountLogoutUnknownAccount 未知账号：404，且不发上游请求。
func TestAccountLogoutUnknownAccount(t *testing.T) {
	t.Parallel()

	panel, _, _, called := logoutFixture(t, http.StatusOK)

	rec := invokeLogout(panel, "missing")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("code=%d want 404", rec.Code)
	}

	if *called {
		t.Error("未知账号不应调用上游")
	}
}

// TestAccountLogoutDeadCredential 凭证已失效（401）：已无可撤销会话，照常清本地。
func TestAccountLogoutDeadCredential(t *testing.T) {
	t.Parallel()

	panel, state, account, _ := logoutFixture(t, http.StatusUnauthorized)

	rec := invokeLogout(panel, "u1")
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}

	if state.AuthByUID("u1") != nil {
		t.Error("凭证已失效时应出池")
	}

	if _, err := os.Stat(account.FilePath); !os.IsNotExist(err) {
		t.Errorf("凭证文件应已删除，stat err=%v", err)
	}
}
