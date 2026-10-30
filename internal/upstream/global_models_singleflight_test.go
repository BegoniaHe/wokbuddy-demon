package upstream

import (
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
)

// globalModelsProbeBody 一份两路探测都能解析的最小目录响应（v3/config 走
// data.models，企业端点家族走通用信封）。
const globalModelsProbeBody = `{"code":0,"data":{"models":[{"id":"m-single","maxInputTokens":1000,"maxOutputTokens":100}]}}`

// TestFetchGlobalModelsSingleFlight 缓存为空时并发 20 个调用只允许一轮探测：
// 一轮 = 3 路 /v3/config + 企业端点首路（/v2 成功），共 4 次出站请求。
// 所有调用必须拿到同一份结果。
func TestFetchGlobalModelsSingleFlight(t *testing.T) {
	t.Parallel()

	var requests atomic.Int64

	release := make(chan struct{})

	client := testClient(func(r *http.Request) (*http.Response, error) {
		requests.Add(1)
		// 首个探测的 4 路请求会全部阻塞在这里，放大并发窗口：没有单飞时其余 19 个
		// 调用会各自再发 4 路请求。
		<-release

		return jsonResp(http.StatusOK, globalModelsProbeBody), nil
	})
	client.GlobalEnabled = true
	client.ChatBaseGlobal = "https://global.example"

	account := &auth.Auth{UID: "u-gl", AccessToken: "at", Domain: "www.workbuddy.ai"}

	const callers = 20

	var wg sync.WaitGroup

	results := make([][]string, callers)

	for i := range callers {
		wg.Go(func() {
			results[i] = client.FetchGlobalModels(account)
		})
	}

	// 等首个探测把 4 路请求都发出来，再给未单飞的实现留一点时间暴露放大。
	waitForCond(t, func() bool { return requests.Load() >= 4 })
	time.Sleep(20 * time.Millisecond)
	close(release)
	wg.Wait()

	if got := requests.Load(); got != 4 {
		t.Fatalf("并发 %d 个调用触发了 %d 次出站请求，单飞未生效（期望 4 次）", callers, got)
	}

	for i := range results {
		if len(results[i]) != 1 || results[i][0] != "m-single" {
			t.Fatalf("第 %d 个调用结果 = %v，期望 [m-single]", i, results[i])
		}
	}
}

// TestFetchGlobalModelsSingleFlightFailure 失败路径同样单飞：一轮探测失败后
// 只发 3 路 /v3/config + 企业端点两路（/v2 → /console 兜底）= 5 次请求，
// 其余调用复用负缓存，且负缓存期内不再探测。
func TestFetchGlobalModelsSingleFlightFailure(t *testing.T) {
	t.Parallel()

	var requests atomic.Int64

	release := make(chan struct{})

	client := testClient(func(r *http.Request) (*http.Response, error) {
		requests.Add(1)
		<-release

		return jsonResp(http.StatusInternalServerError, `{"code":10000,"msg":"boom"}`), nil
	})
	client.GlobalEnabled = true
	client.ChatBaseGlobal = "https://global.example"

	account := &auth.Auth{UID: "u-gl", AccessToken: "at", Domain: "www.workbuddy.ai"}

	const callers = 20

	var wg sync.WaitGroup

	for range callers {
		wg.Go(func() {
			if names := client.FetchGlobalModels(account); names != nil {
				t.Errorf("探测失败应返回 nil，实际 %v", names)
			}
		})
	}

	// 首个探测先发 3 路 /v3/config + 企业端点 /v2（4 次）后才轮到 /console 兜底，
	// 故这里等 4 次即放行，第 5 次在放行后立即完成。
	waitForCond(t, func() bool { return requests.Load() >= 4 })
	time.Sleep(20 * time.Millisecond)
	close(release)
	wg.Wait()

	if got := requests.Load(); got != 5 {
		t.Fatalf("失败路径并发 %d 个调用触发了 %d 次出站请求（期望 5 次）", callers, got)
	}

	// 负缓存期内：直接返回，零新增请求。
	if names := client.FetchGlobalModels(account); names != nil {
		t.Fatalf("负缓存期内应返回 nil，实际 %v", names)
	}

	if got := requests.Load(); got != 5 {
		t.Fatalf("负缓存期内又探测了：%d 次请求", got)
	}
}

// waitForCond 轮询等待条件成立（上限 5s，超时直接失败）。
func waitForCond(t *testing.T, cond func() bool) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("等待条件超时")
		}

		time.Sleep(time.Millisecond)
	}
}
