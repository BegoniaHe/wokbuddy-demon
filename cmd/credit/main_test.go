package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
	"github.com/BegoniaHe/wokbuddy-demon/internal/upstream"
)

func parseTestAuth(t *testing.T, raw string) *auth.Auth {
	t.Helper()

	account, err := auth.Parse([]byte(raw))
	if err != nil {
		t.Fatalf("parse auth: %v", err)
	}

	return account
}

// TestNewUserResourceRequestNestedAndFlat 嵌套形与扁平形 auth 都能直接用于
// 计费请求，身份头来自 auth.Auth 统一字段。
func TestNewUserResourceRequestNestedAndFlat(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		raw  string
	}{
		{
			name: "嵌套形",
			raw:  `{"auth":{"accessToken":"nested-token","domain":"www.codebuddy.cn"},"account":{"uid":"nested-uid","enterpriseId":"nested-eid","nickname":"嵌套"}}`,
		},
		{
			name: "扁平形",
			raw:  `{"accessToken":"flat-token","domain":"www.codebuddy.cn","uid":"flat-uid","enterpriseId":"flat-eid","nickname":"扁平"}`,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			account := parseTestAuth(t, testCase.raw)

			req, err := newUserResourceRequest(billingBaseFor(account), userResourcePaths()[0], account)
			if err != nil {
				t.Fatalf("newUserResourceRequest: %v", err)
			}

			if got := req.Header.Get("Authorization"); got != "Bearer "+account.AccessTokenValue() {
				t.Errorf("Authorization=%q", got)
			}

			if got := req.Header.Get("X-User-Id"); got != account.UID {
				t.Errorf("X-User-Id=%q want %q", got, account.UID)
			}

			if got := req.Header.Get("X-Enterprise-Id"); got != account.EnterpriseID {
				t.Errorf("X-Enterprise-Id=%q want %q", got, account.EnterpriseID)
			}

			if got := req.Header.Get("X-Tenant-Id"); got != account.EnterpriseID {
				t.Errorf("X-Tenant-Id=%q want %q", got, account.EnterpriseID)
			}

			if got := req.Header.Get("X-Domain"); got != account.DomainValue() {
				t.Errorf("X-Domain=%q want %q", got, account.DomainValue())
			}
		})
	}
}

// TestUserResourceRequestUsesIssuingDomain 三域映射与 internal/upstream 同口径：
// CN 账号（含 domain=codebuddy.cn）缺省打 workbuddy.cn，global 按签发域分
// workbuddy.ai / codebuddy.ai；首候选路径统一为无 `/v2` 前缀（官方 5.9.3
// resourcePrefix 为空串，两 realm 同形）。
func TestUserResourceRequestUsesIssuingDomain(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		raw  string
		want string
	}{
		{
			name: "国内站（domain=codebuddy.cn 仍判 cn，缺省打 workbuddy.cn）",
			raw:  `{"auth":{"accessToken":"at","domain":"www.codebuddy.cn"}}`,
			want: "https://www.workbuddy.cn/billing/meter/get-user-resource",
		},
		{
			name: "国内站无 domain",
			raw:  `{"auth":{"accessToken":"at"}}`,
			want: "https://www.workbuddy.cn/billing/meter/get-user-resource",
		},
		{
			name: "官方国际站",
			raw:  `{"auth":{"accessToken":"at","domain":"www.codebuddy.ai"}}`,
			want: "https://www.codebuddy.ai/billing/meter/get-user-resource",
		},
		{
			name: "旧桌面国际站",
			raw:  `{"auth":{"accessToken":"at","domain":"www.workbuddy.ai"}}`,
			want: "https://www.workbuddy.ai/billing/meter/get-user-resource",
		},
		{
			name: "显式 global 无 domain",
			raw:  `{"auth":{"accessToken":"at","realm":"global"}}`,
			want: "https://www.workbuddy.ai/billing/meter/get-user-resource",
		},
		{
			name: "realm=cn 但 domain 属 global",
			raw:  `{"auth":{"accessToken":"at","realm":"cn","domain":"www.workbuddy.ai"}}`,
			want: "https://www.workbuddy.ai/billing/meter/get-user-resource",
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			account := parseTestAuth(t, testCase.raw)

			req, err := newUserResourceRequest(billingBaseFor(account), userResourcePaths()[0], account)
			if err != nil {
				t.Fatalf("newUserResourceRequest: %v", err)
			}

			if got := req.URL.String(); got != testCase.want {
				t.Errorf("url=%q want %q", got, testCase.want)
			}
		})
	}
}

// TestCollectAccountsCountsBadFiles 每个输入文件都产生一条结果，坏文件写
// load: 前缀，total.accounts 与文件数一致。
func TestCollectAccountsCountsBadFiles(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	badJSON := filepath.Join(dir, "workbuddy-bad.json")
	missingToken := filepath.Join(dir, "workbuddy-no-token.json")
	missingFile := filepath.Join(dir, "workbuddy-missing.json")

	if err := os.WriteFile(badJSON, []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(missingToken, []byte(`{"auth":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	accounts := collectAccounts([]string{badJSON, missingToken, missingFile}, upstream.New())
	if len(accounts) != 3 {
		t.Fatalf("accounts=%d want 3", len(accounts))
	}

	for i, item := range accounts {
		if item.OK {
			t.Errorf("accounts[%d] should fail", i)
		}

		if !strings.HasPrefix(item.Error, "load: ") {
			t.Errorf("accounts[%d] error=%q want load: prefix", i, item.Error)
		}
	}
}

// TestRefreshAccountRefreshesAndWritesBack 过期 token 在单次运行内经 refresh
// 端点更新并原子写回 auth 文件。
func TestRefreshAccountRefreshesAndWritesBack(t *testing.T) {
	t.Parallel()

	var gotPath, gotRefreshToken string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotRefreshToken = r.Header.Get("X-Refresh-Token")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"data":{"accessToken":"new-at","refreshToken":"new-rt","expiresIn":3600}}`))
	}))
	t.Cleanup(srv.Close)

	dir := t.TempDir()
	filePath := filepath.Join(dir, "workbuddy-u-refresh.json")

	raw := `{"auth":{"accessToken":"old-at","refreshToken":"old-rt","expiresAt":1,"domain":"www.codebuddy.cn"},"account":{"uid":"u-refresh","nickname":"待刷新"}}`
	if err := os.WriteFile(filePath, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}

	account := parseTestAuth(t, raw)
	account.FilePath = filePath

	client := upstream.New()
	client.HTTP = srv.Client()
	client.ChatBaseCN = srv.URL

	if err := refreshAccount(client, account); err != nil {
		t.Fatalf("refreshAccount: %v", err)
	}

	if gotPath != "/v2/plugin/auth/token/refresh" {
		t.Errorf("refresh path=%q", gotPath)
	}

	if gotRefreshToken != "old-rt" {
		t.Errorf("X-Refresh-Token=%q", gotRefreshToken)
	}

	if got := account.AccessTokenValue(); got != "new-at" {
		t.Errorf("accessToken=%q want new-at", got)
	}

	written, err := os.ReadFile(filePath)
	if err != nil {
		t.Fatalf("read written auth: %v", err)
	}

	saved, err := auth.Parse(written)
	if err != nil {
		t.Fatalf("parse written auth: %v", err)
	}

	if saved.AccessTokenValue() != "new-at" || saved.RefreshTokenValue() != "new-rt" {
		t.Errorf("written auth token=%q refresh=%q", saved.AccessTokenValue(), saved.RefreshTokenValue())
	}
}

// TestRefreshAccountFallsBackWhenTokenStillValid refresh 端点暂时失败但 access token
// 未过期时，继续用旧 token 查询余额；不返回错误、不改写文件。
func TestRefreshAccountFallsBackWhenTokenStillValid(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"code":400,"msg":"bad refresh"}`))
	}))
	t.Cleanup(srv.Close)

	raw := fmt.Sprintf(
		`{"auth":{"accessToken":"still-at","refreshToken":"old-rt","expiresAt":%d,"domain":"www.codebuddy.cn"},"account":{"uid":"u-still"}}`,
		time.Now().Add(time.Hour).Unix(),
	)
	account := parseTestAuth(t, raw)

	client := upstream.New()
	client.HTTP = srv.Client()
	client.ChatBaseCN = srv.URL

	if err := refreshAccount(client, account); err != nil {
		t.Fatalf("未过期 token 的 refresh 失败不应阻断查询: %v", err)
	}

	if got := account.AccessTokenValue(); got != "still-at" {
		t.Errorf("accessToken=%q want still-at", got)
	}
}

// TestRefreshAccountWithoutRefreshTokenKeepsHandwrittenAuth 手写扁平 auth 常无
// expiresAt / refreshToken；不能因为 NeedsRefresh(无过期时间)=true 就判 refresh
// 失败，必须保留 accessToken 继续查询，且不重写原文件。
func TestRefreshAccountWithoutRefreshTokenKeepsHandwrittenAuth(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	filePath := filepath.Join(dir, "workbuddy-u-flat.json")

	raw := `{"accessToken":"flat-at","uid":"u-flat","domain":"www.codebuddy.cn"}`
	if err := os.WriteFile(filePath, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}

	account := parseTestAuth(t, raw)
	account.FilePath = filePath

	if err := refreshAccount(upstream.New(), account); err != nil {
		t.Fatalf("无 refresh token 应跳过刷新: %v", err)
	}

	if got := account.AccessTokenValue(); got != "flat-at" {
		t.Errorf("accessToken=%q want flat-at", got)
	}

	written, err := os.ReadFile(filePath)
	if err != nil {
		t.Fatalf("read auth: %v", err)
	}

	if string(written) != raw {
		t.Errorf("未触发刷新不应重写 auth 文件: %s", written)
	}
}

// TestCollectAccountsRefreshFailureRow refresh 失败记 refresh: 前缀并跳过余额请求。
func TestCollectAccountsRefreshFailureRow(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"code":400,"msg":"bad refresh"}`))
	}))
	t.Cleanup(srv.Close)

	dir := t.TempDir()
	filePath := filepath.Join(dir, "workbuddy-u-fail.json")

	raw := `{"auth":{"accessToken":"old-at","refreshToken":"old-rt","expiresAt":1,"domain":"www.codebuddy.cn"},"account":{"uid":"u-fail"}}`
	if err := os.WriteFile(filePath, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}

	client := upstream.New()
	client.HTTP = srv.Client()
	client.ChatBaseCN = srv.URL

	accounts := collectAccounts([]string{filePath}, client)
	if len(accounts) != 1 {
		t.Fatalf("accounts=%d want 1", len(accounts))
	}

	if accounts[0].OK {
		t.Fatal("refresh failure should not be ok")
	}

	if !strings.HasPrefix(accounts[0].Error, "refresh: ") {
		t.Errorf("error=%q want refresh: prefix", accounts[0].Error)
	}
}

// TestFetchUserResourceFallsBackOn404 无前缀路径 404 时换 `/v2` 重试，且成功响应正常聚合。
func TestFetchUserResourceFallsBackOn404(t *testing.T) {
	t.Parallel()

	var paths []string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)

		if r.URL.Path == "/billing/meter/get-user-resource" {
			w.WriteHeader(http.StatusNotFound)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"data":{"Response":{"Data":{
			"TotalDosage":0,
			"Accounts":[{"CapacityRemain":30,"CapacityUsed":70,"CapacitySize":100}]
		}}}}`))
	}))
	t.Cleanup(srv.Close)

	account := parseTestAuth(t, `{"auth":{"accessToken":"at","domain":"www.codebuddy.cn"},"account":{"uid":"u-fb"}}`)

	remain, used, size, packs, err := fetchUserResourceAt(srv.Client(), srv.URL, account)
	if err != nil {
		t.Fatalf("fetchUserResourceAt: %v", err)
	}

	if want := []string{"/billing/meter/get-user-resource", "/v2/billing/meter/get-user-resource"}; !slices.Equal(paths, want) {
		t.Errorf("paths=%v want %v", paths, want)
	}

	if remain != 30 || used != 70 || size != 100 || packs != 1 {
		t.Errorf("remain/used/size/packs=%d/%d/%d/%d", remain, used, size, packs)
	}
}

// TestFetchUserResourceNoFallbackOnOther4xx 非 404 的 4xx 不换路径（鉴权/参数错误重试无益）。
func TestFetchUserResourceNoFallbackOnOther4xx(t *testing.T) {
	t.Parallel()

	var paths []string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)

		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(srv.Close)

	account := parseTestAuth(t, `{"auth":{"accessToken":"at","domain":"www.codebuddy.cn"},"account":{"uid":"u-401"}}`)

	if _, _, _, _, err := fetchUserResourceAt(srv.Client(), srv.URL, account); err == nil {
		t.Fatal("401 应返回错误")
	}

	if want := []string{"/billing/meter/get-user-resource"}; !slices.Equal(paths, want) {
		t.Errorf("paths=%v want %v", paths, want)
	}
}
