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
