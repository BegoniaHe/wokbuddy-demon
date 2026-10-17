package panel

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
	"github.com/BegoniaHe/wokbuddy-demon/internal/pool"
	"github.com/BegoniaHe/wokbuddy-demon/internal/upstream"
)

// probeRoundTripFunc 测试用 RoundTripper。
type probeRoundTripFunc func(*http.Request) (*http.Response, error)

func (f probeRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// newProbePanel 构造带单账号与 fake 上游的面板；handler 由 resp 决定。
func newProbePanel(t *testing.T, resp func(*http.Request) (*http.Response, error)) (*Panel, *string) {
	t.Helper()
	up := &upstream.Client{
		HTTP:              &http.Client{Transport: probeRoundTripFunc(resp)},
		ChatBaseCN:        "https://fake.example",
		BillingBaseCN:     "https://fake.example",
		ChatBaseGlobal:    "https://fake.example",
		BillingBaseGlobal: "https://fake.example",
	}
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999, Domain: "workbuddy.cn"})
	stateFile := t.TempDir() + "/model_probes.json"
	pn := New(Config{
		Version: "test", APIKey: "k", Pool: p, Upstream: up,
		ProbeStateFile: stateFile,
	})
	return pn, &stateFile
}

// sseResponse 构造成功 SSE 响应（首帧内容 + 末帧 usage + DONE）。
func sseResponse() *http.Response {
	body := "data: {\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"pong\"}}]}\n\n" +
		"data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"completion_tokens\":3}}\n\n" +
		"data: [DONE]\n\n"
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

// postProbe 发一次探测请求。
func postProbe(pn *Panel, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/panel/api/model_probes", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer k")
	rec := httptest.NewRecorder()
	pn.ServeHTTP(rec, req)
	return rec
}

// getHealth 查询 model_health。
func getHealth(pn *Panel) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/panel/api/model_health", nil)
	req.Header.Set("Authorization", "Bearer k")
	rec := httptest.NewRecorder()
	pn.ServeHTTP(rec, req)
	return rec
}

// decodeProbeResp 解析探测响应。
func decodeProbeResp(t *testing.T, rec *httptest.ResponseRecorder) (bool, string, *modelProbeSample) {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var resp struct {
		OK     bool              `json:"ok"`
		Key    string            `json:"key"`
		Sample *modelProbeSample `json:"sample"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	return resp.OK, resp.Key, resp.Sample
}

// decodeHealth 解析 model_health 响应。
func decodeHealth(t *testing.T, rec *httptest.ResponseRecorder) map[string]modelProbeHealth {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("health code=%d body=%s", rec.Code, rec.Body)
	}
	var resp struct {
		Models map[string]modelProbeHealth `json:"models"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	return resp.Models
}

// TestModelProbeSuccess 一次成功探测：TTFT、输出 token、状态持久化。
func TestModelProbeSuccess(t *testing.T) {
	pn, stateFile := newProbePanel(t, func(*http.Request) (*http.Response, error) { return sseResponse(), nil })
	ok, key, sample := decodeProbeResp(t, postProbe(pn, `{"model":"glm-5.2","realm":"cn"}`))
	if !ok || key != "cn:glm-5.2" {
		t.Fatalf("ok=%v key=%q want true/cn:glm-5.2", ok, key)
	}
	if sample == nil || sample.TTFTMs <= 0 || sample.OutputTokens != 3 {
		t.Fatalf("sample=%+v want ttft>0 tokens=3", sample)
	}
	if _, err := os.ReadFile(*stateFile); err != nil {
		t.Fatalf("probe state not persisted: %v", err)
	}
}

// TestModelHealthAggregatesProbe 成功探测后 health 返回滚动聚合。
func TestModelHealthAggregatesProbe(t *testing.T) {
	pn, _ := newProbePanel(t, func(*http.Request) (*http.Response, error) { return sseResponse(), nil })
	if _, _, sample := decodeProbeResp(t, postProbe(pn, `{"model":"glm-5.2"}`)); sample == nil {
		t.Fatal("probe sample missing")
	}
	h := decodeHealth(t, getHealth(pn))["cn:glm-5.2"]
	if h.Success != 1 || h.Failure != 0 {
		t.Fatalf("health=%+v want success=1 failure=0", h)
	}
	if h.AvgTTFTMs <= 0 || h.Last == nil || h.LastProbeAt == "" {
		t.Fatalf("health=%+v want avg ttft + last probe", h)
	}
}

// TestModelProbeUpstreamError 上游 5xx：失败样本记录错误分类，不伪造成功。
func TestModelProbeUpstreamError(t *testing.T) {
	pn, _ := newProbePanel(t, func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusInternalServerError,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"code":500,"msg":"boom"}`)),
		}, nil
	})
	rec := postProbe(pn, `{"model":"glm-5.2"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s（探测失败仍返回 200 + 样本）", rec.Code, rec.Body)
	}
	ok, _, sample := decodeProbeResp(t, rec)
	if ok || sample == nil || sample.ErrorClass == "" {
		t.Fatalf("sample=%+v want failed sample with error_class", sample)
	}
	if sample.TTFTMs != 0 {
		t.Errorf("failed probe must not report ttft: %+v", sample)
	}
}

// TestModelProbeTimeout 探测硬超时：样本记 timeout/duration，不无限等待。
func TestModelProbeTimeout(t *testing.T) {
	old := modelProbeTimeout
	modelProbeTimeout = 100 * time.Millisecond
	t.Cleanup(func() { modelProbeTimeout = old })

	pn, _ := newProbePanel(t, func(r *http.Request) (*http.Response, error) {
		<-r.Context().Done() // 永不返回响应：等探测超时取消
		return nil, r.Context().Err()
	})
	start := time.Now()
	rec := postProbe(pn, `{"model":"glm-5.2"}`)
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("probe took %v: timeout not enforced", elapsed)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	ok, _, sample := decodeProbeResp(t, rec)
	if ok || sample == nil || sample.ErrorClass == "" {
		t.Fatalf("sample=%+v want timeout failure", sample)
	}
}

// TestModelProbeNoAccount 无可用账号：明确 no_account，不发上游。
func TestModelProbeNoAccount(t *testing.T) {
	var calls int64
	pn, _ := newProbePanel(t, func(*http.Request) (*http.Response, error) {
		atomic.AddInt64(&calls, 1)
		return sseResponse(), nil
	})
	// 清空池：直接构造无账号面板。
	p2 := New(Config{Version: "test", APIKey: "k", Pool: pool.New(""), Upstream: pn.cfg.Upstream, ProbeStateFile: t.TempDir() + "/s.json"})
	rec := postProbe(p2, `{"model":"glm-5.2"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	ok, _, sample := decodeProbeResp(t, rec)
	if ok || sample == nil || sample.ErrorClass != "no_account" {
		t.Fatalf("sample=%+v want no_account", sample)
	}
	if n := atomic.LoadInt64(&calls); n != 0 {
		t.Errorf("upstream calls=%d want 0", n)
	}
}

// TestModelProbeSingleFlight 同一模型并发探测：第二次 409，不重复打上游。
func TestModelProbeSingleFlight(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	pn, _ := newProbePanel(t, func(r *http.Request) (*http.Response, error) {
		once.Do(func() { close(entered) })
		<-release
		return sseResponse(), nil
	})

	done := make(chan int, 1)
	go func() {
		done <- postProbe(pn, `{"model":"glm-5.2"}`).Code
	}()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("first probe did not reach upstream")
	}
	if code := postProbe(pn, `{"model":"glm-5.2"}`).Code; code != http.StatusConflict {
		t.Errorf("second probe code=%d want 409", code)
	}
	close(release)
	if code := <-done; code != http.StatusOK {
		t.Errorf("first probe code=%d want 200", code)
	}
}

// TestModelHealthEmpty 无历史：空 map（不伪造数值）。
func TestModelHealthEmpty(t *testing.T) {
	pn, _ := newProbePanel(t, func(*http.Request) (*http.Response, error) { return sseResponse(), nil })
	if models := decodeHealth(t, getHealth(pn)); len(models) != 0 {
		t.Fatalf("models=%v want empty", models)
	}
}

// TestSummarizeProbeSamples 聚合：窗口、成功/失败、avg/p50/p95。
func TestSummarizeProbeSamples(t *testing.T) {
	base := time.Now()
	var samples []modelProbeSample
	for i, v := range []int64{10, 20, 30, 40, 50, 60, 70, 80, 90, 100} {
		samples = append(samples, modelProbeSample{At: base.Add(time.Duration(i) * time.Second), OK: true, TTFTMs: v})
	}
	samples = append(samples, modelProbeSample{At: base.Add(11 * time.Second), OK: false, ErrorClass: "timeout"})
	h := summarizeProbeSamples(samples)
	if h.Success != 10 || h.Failure != 1 {
		t.Errorf("success=%d failure=%d want 10/1", h.Success, h.Failure)
	}
	if h.AvgTTFTMs != 55 {
		t.Errorf("avg=%d want 55", h.AvgTTFTMs)
	}
	if h.P50TTFTMs != 50 || h.P95TTFTMs != 100 {
		t.Errorf("p50=%d p95=%d want 50/100", h.P50TTFTMs, h.P95TTFTMs)
	}
	if h.Last == nil || h.Last.OK {
		t.Errorf("last=%+v want latest failure", h.Last)
	}
	if h.LastProbeAt == "" {
		t.Error("last_probe_at missing")
	}
}

// TestProbeStateSurvivesReload 状态文件重载后 health 仍可读（持久化语义）。
func TestProbeStateSurvivesReload(t *testing.T) {
	pn, stateFile := newProbePanel(t, func(*http.Request) (*http.Response, error) { return sseResponse(), nil })
	if rec := postProbe(pn, `{"model":"glm-5.2"}`); rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	// 新 Panel 实例共享同一状态文件：健康数据从磁盘恢复。
	p2 := New(Config{Version: "test", APIKey: "k", Pool: pool.New(""), Upstream: pn.cfg.Upstream, ProbeStateFile: *stateFile})
	if h := decodeHealth(t, getHealth(p2))["cn:glm-5.2"]; h.Success != 1 {
		t.Fatalf("reloaded health=%+v want success=1", h)
	}
}

// TestReadProbeLineLimit 无换行超长行在累计到上限时立即失败，不继续扩容。
func TestReadProbeLineLimit(t *testing.T) {
	br := bufio.NewReaderSize(strings.NewReader(strings.Repeat("x", 100)), 16)
	line, n, err := readProbeLine(br, 32)
	if !errors.Is(err, errProbeLineTooLong) {
		t.Fatalf("err=%v want errProbeLineTooLong", err)
	}
	if line != "" {
		t.Errorf("line=%q want empty（超限不返回部分行）", line)
	}
	if n > 32 {
		t.Errorf("n=%d want <= 32", n)
	}
}

// TestReadProbeLineMatchesReadString 末行无换行时返回该行 + io.EOF；完整行不含错误。
func TestReadProbeLineMatchesReadString(t *testing.T) {
	br := bufio.NewReaderSize(strings.NewReader("data: {}\nrest"), 1024)
	line, n, err := readProbeLine(br, 1024)
	if err != nil || line != "data: {}\n" || n != 9 {
		t.Fatalf("line=%q n=%d err=%v", line, n, err)
	}
	line, n, err = readProbeLine(br, 1024)
	if !errors.Is(err, io.EOF) || line != "rest" || n != 4 {
		t.Fatalf("line=%q n=%d err=%v want rest/4/EOF", line, n, err)
	}
}

// TestReadProbeStreamOverlongLine 无换行超长行按读取上限截断，按解析失败记录。
func TestReadProbeStreamOverlongLine(t *testing.T) {
	rc := strings.NewReader(strings.Repeat("x", modelProbeMaxReadBytes+10))
	_, _, class, _ := readProbeStream(time.Now(), rc)
	if class != "upstream_parse" {
		t.Fatalf("class=%q want upstream_parse", class)
	}
}

// TestReadProbeStreamReasoningIsFirstToken 推理模型只吐 reasoning_content 时
// 不能记为「无内容帧」失败（issue #110）。
func TestReadProbeStreamReasoningIsFirstToken(t *testing.T) {
	raw := "data: {\"choices\":[{\"index\":0,\"delta\":{\"reasoning_content\":\"thinking\"}}]}\n\n" +
		"data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"length\"}],\"usage\":{\"completion_tokens\":16}}\n\n" +
		"data: [DONE]\n\n"
	ttft, tokens, class, msg := readProbeStream(time.Now(), strings.NewReader(raw))
	if class != "" || msg != "" {
		t.Fatalf("class=%q msg=%q want success", class, msg)
	}
	if ttft <= 0 {
		t.Errorf("ttft=%d want > 0（reasoning_content 也是首字）", ttft)
	}
	if tokens != 16 {
		t.Errorf("tokens=%d want 16", tokens)
	}
}

// TestReadProbeStreamToolCallIsFirstToken 只产出 tool_calls 的响应也证明上游
// 已开始有效输出，不计为解析失败。
func TestReadProbeStreamToolCallIsFirstToken(t *testing.T) {
	raw := "data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"id\":\"call_1\",\"function\":{\"name\":\"ping\"}}]}}]}\n\n" +
		"data: [DONE]\n\n"
	ttft, _, class, msg := readProbeStream(time.Now(), strings.NewReader(raw))
	if class != "" || msg != "" {
		t.Fatalf("class=%q msg=%q want success", class, msg)
	}
	if ttft <= 0 {
		t.Errorf("ttft=%d want > 0（tool_calls 也是首字）", ttft)
	}
}
