package panel

import (
	"bytes"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
	"github.com/BegoniaHe/wokbuddy-demon/internal/pool"
	"github.com/BegoniaHe/wokbuddy-demon/internal/upstream"
)

// cockpitUpload 构造 cockpit 导入的 multipart 请求体。
func cockpitUpload(t *testing.T, payload []byte) (*bytes.Buffer, string) {
	t.Helper()

	var body bytes.Buffer

	writer := multipart.NewWriter(&body)

	fw, err := writer.CreateFormFile("file", "cockpit.json")
	if err != nil {
		t.Fatal(err)
	}

	if _, err := fw.Write(payload); err != nil {
		t.Fatal(err)
	}

	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	return &body, writer.FormDataContentType()
}

// cockpitAccountsJSON 生成 n 个合法 cockpit 账号的 JSON。
func cockpitAccountsJSON(t *testing.T, n int) []byte {
	t.Helper()

	accounts := make([]map[string]any, 0, n)
	for i := range n {
		accounts = append(accounts, map[string]any{
			"id":            fmt.Sprintf("id-%d", i),
			"uid":           fmt.Sprintf("u%d", i),
			"access_token":  fmt.Sprintf("at-%d", i),
			"refresh_token": fmt.Sprintf("rt-%d", i),
			"expires_at":    4102444800000,
			"domain":        "workbuddy.cn",
		})
	}

	raw, err := json.Marshal(accounts)
	if err != nil {
		t.Fatal(err)
	}

	return raw
}

// newCockpitPanel 构造带计数器的面板：记录上游调用次数与最大并发。
func newCockpitPanel(t *testing.T) (*Panel, *pool.Pool, *int64, *int32) {
	t.Helper()

	var (
		calls                 int64
		inFlight, maxInFlight int32
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&calls, 1)

		cur := atomic.AddInt32(&inFlight, 1)

		for {
			old := atomic.LoadInt32(&maxInFlight)
			if cur <= old || atomic.CompareAndSwapInt32(&maxInFlight, old, cur) {
				break
			}
		}

		time.Sleep(20 * time.Millisecond) // 让并发窗口可观测
		atomic.AddInt32(&inFlight, -1)

		_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{}}`))
	}))
	t.Cleanup(srv.Close)
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	p := pool.New("")
	pn := &Panel{cfg: Config{Pool: p, Upstream: up, AuthDir: t.TempDir()}}

	return pn, p, &calls, &maxInFlight
}

// doCockpitImport 直接调用 handler（与路由层同语义：鉴权由 withAuth 包裹，不影响本文件断言）。
func doCockpitImport(t *testing.T, panel *Panel, payload []byte) *httptest.ResponseRecorder {
	t.Helper()
	body, ct := cockpitUpload(t, payload)
	req := httptest.NewRequest(http.MethodPost, "/panel/api/import/cockpit", body)
	req.Header.Set("Content-Type", ct)

	rec := httptest.NewRecorder()
	panel.importCockpit(rec, req)

	return rec
}

// TestCockpitImportRejectsOversizedBody 请求体超过上限：解析前拒绝，
// 不读入内存、不落盘、不发起上游调用（issue #107）。
func TestCockpitImportRejectsOversizedBody(t *testing.T) {
	t.Parallel()
	panel, poolState, calls, _ := newCockpitPanel(t)
	oversized := bytes.Repeat([]byte("x"), cockpitMaxUploadBytes+1024)

	rec := doCockpitImport(t, panel, oversized)
	if rec.Code != http.StatusBadRequest && rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("code=%d want 400/413 body=%s", rec.Code, rec.Body)
	}

	if len(poolState.List()) != 0 {
		t.Errorf("oversized import must not add accounts")
	}

	if n := atomic.LoadInt64(calls); n != 0 {
		t.Errorf("upstream calls=%d want 0", n)
	}

	entries, err := os.ReadDir(panel.cfg.AuthDir)
	if err != nil {
		t.Fatal(err)
	}

	if len(entries) != 0 {
		t.Errorf("oversized import wrote files: %v", entries)
	}
}

// TestCockpitImportRejectsTooManyAccounts 超过账号数上限：整体拒绝，
// 不导入任何账号、不发起上游调用（issue #107）。
func TestCockpitImportRejectsTooManyAccounts(t *testing.T) {
	t.Parallel()
	panel, state, calls, _ := newCockpitPanel(t)
	payload := cockpitAccountsJSON(t, maxImportAccounts+1)

	rec := doCockpitImport(t, panel, payload)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d want 400 body=%s", rec.Code, rec.Body)
	}

	if !strings.Contains(rec.Body.String(), "too many accounts") {
		t.Errorf("body=%s want readable limit error", rec.Body)
	}

	if len(state.List()) != 0 {
		t.Errorf("rejected import must not add accounts")
	}

	if n := atomic.LoadInt64(calls); n != 0 {
		t.Errorf("upstream calls=%d want 0", n)
	}

	entries, err := os.ReadDir(panel.cfg.AuthDir)
	if err != nil {
		t.Fatal(err)
	}

	if len(entries) != 0 {
		t.Errorf("rejected import wrote files: %v", entries)
	}
}

// cockpitImportResp 导入接口的响应形态。
type cockpitImportResp struct {
	OK       bool     `json:"ok"`
	Total    int      `json:"total"`
	Imported int      `json:"imported"`
	Skipped  int      `json:"skipped"`
	Errors   []string `json:"errors"`
}

// decodeCockpitResp 解析导入响应（非 200 直接 Fatal）。
func decodeCockpitResp(t *testing.T, rec *httptest.ResponseRecorder) cockpitImportResp {
	t.Helper()

	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}

	var resp cockpitImportResp
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}

	return resp
}

// TestCockpitImportNormal 正常导入：全部落盘、热加载，返回计数不变。
func TestCockpitImportNormal(t *testing.T) {
	t.Parallel()
	panel, state, calls, _ := newCockpitPanel(t)

	resp := decodeCockpitResp(t, doCockpitImport(t, panel, cockpitAccountsJSON(t, 2)))
	if !resp.OK || resp.Total != 2 || resp.Imported != 2 || resp.Skipped != 0 || len(resp.Errors) != 0 {
		t.Fatalf("resp=%+v want total=2 imported=2 skipped=0", resp)
	}

	if got := len(state.List()); got != 2 {
		t.Errorf("pool accounts=%d want 2", got)
	}

	for _, uid := range []string{"u0", "u1"} {
		if _, err := os.Stat(filepath.Join(panel.cfg.AuthDir, "workbuddy-"+uid+".json")); err != nil {
			t.Errorf("auth file for %s missing: %v", uid, err)
		}
	}

	if n := atomic.LoadInt64(calls); n == 0 {
		t.Errorf("expected post-import upstream calls (checkin/resource)")
	}
}

// TestCockpitImportPartialFailure 部分失败：失败项逐条返回，成功项照常导入。
func TestCockpitImportPartialFailure(t *testing.T) {
	t.Parallel()
	panel, state, _, _ := newCockpitPanel(t)
	payload := []byte(`[
		{"id":"good","uid":"u-good","access_token":"at","refresh_token":"rt","domain":"workbuddy.cn"},
		{"id":"bad","uid":"u-bad","access_token":"","refresh_token":"rt","domain":"workbuddy.cn"}
	]`)

	resp := decodeCockpitResp(t, doCockpitImport(t, panel, payload))
	if resp.Total != 2 || resp.Imported != 1 || resp.Skipped != 1 || len(resp.Errors) != 1 {
		t.Fatalf("resp=%+v want total=2 imported=1 skipped=1 errors=1", resp)
	}

	if !strings.Contains(resp.Errors[0], "missing required fields") {
		t.Errorf("error=%q want missing fields reason", resp.Errors[0])
	}

	if got := len(state.List()); got != 1 {
		t.Errorf("pool accounts=%d want 1", got)
	}
}

// TestCockpitImportBoundedConcurrency 导入后的上游调用并发有界（issue #107）：
// 12 个账号时同时在途的请求不得超过 importWorkers。
func TestCockpitImportBoundedConcurrency(t *testing.T) {
	t.Parallel()
	pn, _, _, maxInFlight := newCockpitPanel(t)

	const n = 12

	rec := doCockpitImport(t, pn, cockpitAccountsJSON(t, n))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}

	if got := atomic.LoadInt32(maxInFlight); got > importWorkers {
		t.Errorf("max concurrent upstream calls=%d want <= %d", got, importWorkers)
	}
}

// enterprisePayload 构造带企业标识的 cockpit 账号 JSON（camelCase 与 snake_case
// 两种写法由 enterpriseIDKey 指定）。
func enterprisePayload(t *testing.T, enterpriseIDKey string) []byte {
	t.Helper()

	raw, err := json.Marshal([]map[string]any{{
		"id":            "ent",
		"uid":           "u-ent",
		"access_token":  "at-ent",
		"refresh_token": "rt-ent",
		"expires_at":    4102444800000,
		"domain":        "workbuddy.cn",
		enterpriseIDKey: "ent-1",
	}})
	if err != nil {
		t.Fatal(err)
	}

	return raw
}

// TestCockpitImportKeepsEnterpriseID cockpit 导出的企业标识必须落盘（issue #446）：
// enterprise_id / enterpriseId 两种写法都要写进 auth.Auth.EnterpriseID，否则企业号
// 导入后退化成个人号，后续任务面全部按个人号打上游。
func TestCockpitImportKeepsEnterpriseID(t *testing.T) {
	t.Parallel()

	for _, key := range []string{"enterprise_id", "enterpriseId"} {
		t.Run(key, func(t *testing.T) {
			t.Parallel()
			panel, state, _, _ := newCockpitPanel(t)

			rec := doCockpitImport(t, panel, enterprisePayload(t, key))

			resp := decodeCockpitResp(t, rec)
			if resp.Imported != 1 {
				t.Fatalf("resp=%+v want imported=1", resp)
			}

			list := state.List()
			if len(list) != 1 {
				t.Fatalf("pool accounts=%d want 1", len(list))
			}

			a := state.AuthByUID("u-ent")
			if a == nil {
				t.Fatal("pool missing imported account")
			}

			if got := a.IsEnterprise(); !got {
				t.Errorf("IsEnterprise()=%v want true (%s form lost)", got, key)
			}

			// 落盘形状同样带企业标识：重启加载后不能退化。
			raw, err := os.ReadFile(filepath.Join(panel.cfg.AuthDir, "workbuddy-u-ent.json"))
			if err != nil {
				t.Fatal(err)
			}

			parsed, err := auth.Parse(raw)
			if err != nil {
				t.Fatal(err)
			}

			if !parsed.IsEnterprise() {
				t.Errorf("reloaded auth IsEnterprise()=false, enterprise id not persisted")
			}
		})
	}
}

// TestCockpitImportSkipsEnterpriseCheckin 企业号导入后不得打签到（issue #446）：
// 上游 daily-checkin 对企业号恒 400「企业账号不支持该操作」，白打一次；
// 企业额度查询（UserResource）照常走企业口径端点。
func TestCockpitImportSkipsEnterpriseCheckin(t *testing.T) {
	t.Parallel()
	panel, _, checkinHits, enterpriseHits := newEnterpriseAwareCockpitPanel(t)

	rec := doCockpitImport(t, panel, enterprisePayload(t, "enterprise_id"))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}

	if n := atomic.LoadInt64(checkinHits); n != 0 {
		t.Errorf("daily-checkin hits=%d want 0 (enterprise account)", n)
	}

	if n := atomic.LoadInt64(enterpriseHits); n != 1 {
		t.Errorf("enterprise usage hits=%d want 1 (balance query still runs)", n)
	}
}

// TestCockpitImportPersonalStillCheckins 个人号导入后照常签到：企业门控只跳
// 企业号，不能顺手关掉个人号的导入签到（issue #446 回归护栏）。
func TestCockpitImportPersonalStillCheckins(t *testing.T) {
	t.Parallel()
	panel, _, checkinHits, enterpriseHits := newEnterpriseAwareCockpitPanel(t)

	rec := doCockpitImport(t, panel, cockpitAccountsJSON(t, 1))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}

	if n := atomic.LoadInt64(checkinHits); n != 1 {
		t.Errorf("daily-checkin hits=%d want 1 (personal account)", n)
	}

	if n := atomic.LoadInt64(enterpriseHits); n != 0 {
		t.Errorf("enterprise usage hits=%d want 0 (personal account)", n)
	}
}

// newEnterpriseAwareCockpitPanel 记录 daily-checkin 与企业额度查询两类端点命中。
func newEnterpriseAwareCockpitPanel(t *testing.T) (*Panel, *pool.Pool, *int64, *int64) {
	t.Helper()

	var checkinHits, enterpriseHits int64

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/billing/meter/daily-checkin"):
			atomic.AddInt64(&checkinHits, 1)
		case strings.HasSuffix(r.URL.Path, "/v2/billing/meter/get-enterprise-user-usage"):
			atomic.AddInt64(&enterpriseHits, 1)
		}

		_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{}}`))
	}))
	t.Cleanup(srv.Close)

	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	p := pool.New("")
	pn := &Panel{cfg: Config{Pool: p, Upstream: up, AuthDir: t.TempDir()}}

	return pn, p, &checkinHits, &enterpriseHits
}

// TestCockpitImportCreatesAuthDir 全新部署（auths/ 不存在）直接导入必须成功：
// SaveAtomic 在 auth_dir 内建临时文件，父目录缺失会 ENOENT，整批导入报错。
func TestCockpitImportCreatesAuthDir(t *testing.T) {
	t.Parallel()

	panel, _, _, _ := newCockpitPanel(t)
	panel.cfg.AuthDir = filepath.Join(t.TempDir(), "auths") // 故意不预建

	resp := decodeCockpitResp(t, doCockpitImport(t, panel, cockpitAccountsJSON(t, 1)))
	if resp.Imported != 1 {
		t.Fatalf("resp=%+v want imported=1 (fresh deployment)", resp)
	}

	if _, err := os.Stat(filepath.Join(panel.cfg.AuthDir, "workbuddy-u0.json")); err != nil {
		t.Fatalf("auth file missing after fresh-deployment import: %v", err)
	}
}

// TestCockpitImportExportShapes 四种导出形态都要能导入：顶层数组、顶层单对象、
// {account,auth} 信封、{"<uid>":{…}} 字典；驼峰凭证键同样归一。
func TestCockpitImportExportShapes(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		payload string
		wantUID string
	}{
		{
			name:    "array",
			payload: `[{"uid":"u-arr","access_token":"at","refresh_token":"rt","domain":"workbuddy.cn"}]`,
			wantUID: "u-arr",
		},
		{
			name:    "single_object",
			payload: `{"uid":"u-single","access_token":"at","refresh_token":"rt","domain":"workbuddy.cn"}`,
			wantUID: "u-single",
		},
		{
			// 信封键名不是 uid：凭证在 auth 段，元数据在 account 段。
			name: "envelope",
			payload: `{"account":{"uid":"u-env","nickname":"nick","domain":"workbuddy.cn"},
				"auth":{"access_token":"at","refresh_token":"rt"}}`,
			wantUID: "u-env",
		},
		{
			// 字典键即 uid：值里缺 uid 时用键补齐。
			name:    "dictionary",
			payload: `{"u-dict":{"access_token":"at","refresh_token":"rt","domain":"workbuddy.cn"}}`,
			wantUID: "u-dict",
		},
		{
			name:    "camel_case",
			payload: `{"uid":"u-camel","accessToken":"at","refreshToken":"rt","domain":"workbuddy.cn"}`,
			wantUID: "u-camel",
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			panel, state, _, _ := newCockpitPanel(t)

			resp := decodeCockpitResp(t, doCockpitImport(t, panel, []byte(testCase.payload)))
			if resp.Imported != 1 || resp.Skipped != 0 {
				t.Fatalf("resp=%+v want imported=1 skipped=0", resp)
			}

			if state.AuthByUID(testCase.wantUID) == nil {
				t.Fatalf("pool missing %s (shape not normalized)", testCase.wantUID)
			}
		})
	}
}

// TestCockpitImportExpiresAtUnit 秒/毫秒归一：秒级值不能再被 /1000 打成 1970。
func TestCockpitImportExpiresAtUnit(t *testing.T) {
	t.Parallel()

	const seconds = 4102444800 // 2100-01-01，与毫秒写法 4102444800000 同义

	for name, payload := range map[string]string{
		"seconds": fmt.Sprintf(
			`[{"uid":"u-sec","access_token":"at","refresh_token":"rt","expires_at":%d}]`, seconds),
		"milliseconds": fmt.Sprintf(
			`[{"uid":"u-sec","access_token":"at","refresh_token":"rt","expires_at":%d}]`, seconds*1000),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			panel, state, _, _ := newCockpitPanel(t)

			resp := decodeCockpitResp(t, doCockpitImport(t, panel, []byte(payload)))
			if resp.Imported != 1 {
				t.Fatalf("resp=%+v want imported=1", resp)
			}

			imported := state.AuthByUID("u-sec")
			if imported == nil {
				t.Fatal("pool missing imported account")
			}

			if imported.ExpiresAt != seconds {
				t.Errorf("ExpiresAt=%d want %d", imported.ExpiresAt, seconds)
			}
		})
	}
}

// TestCockpitImportRejectsUnknownRoot 无法识别的根值仍然 400，不静默成功。
func TestCockpitImportRejectsUnknownRoot(t *testing.T) {
	t.Parallel()
	panel, state, _, _ := newCockpitPanel(t)

	rec := doCockpitImport(t, panel, []byte(`"not-a-cockpit-export"`))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d want 400 body=%s", rec.Code, rec.Body)
	}

	if len(state.List()) != 0 {
		t.Errorf("rejected import must not add accounts")
	}
}
