// warnonce_test.go 钉住模型目录降级 WARN 的进程内去重（issue #630）：
// 同一路降级每轮目录拉取都会命中，但只在进程内打一次；真故障（两路全挂）
// 仍走 error 返回路径，不受去重影响。
package upstream

import (
	"bytes"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
)

// warnLogBuffer 并发安全的日志捕获（探测扇出在后台协程里打日志）。
type warnLogBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *warnLogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	written, err := b.buf.Write(p)
	if err != nil {
		return written, fmt.Errorf("log buffer write: %w", err)
	}

	return written, nil
}

func (b *warnLogBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.String()
}

// captureWarnLog 把标准 logger 重定向到返回的 buffer，测试结束恢复。
func captureWarnLog(t *testing.T) *warnLogBuffer {
	t.Helper()

	out := &warnLogBuffer{}
	prev := log.Writer()

	log.SetOutput(out)

	t.Cleanup(func() { log.SetOutput(prev) })

	return out
}

// resetDegradedWarnOnce 清空去重表：去重状态是进程级的，用例之间必须隔离，
// 否则先跑的用例会把后跑用例的 key 提前吃掉。
func resetDegradedWarnOnce() {
	degradedWarnMu.Lock()
	clear(degradedWarnSeen)
	degradedWarnMu.Unlock()
}

// TestDegradedWarnOnceLogsOncePerKey 同 key 只打一行；不同 key 各打一行。
//
//nolint:paralleltest // 该用例改写进程级日志出口，必须串行执行
func TestDegradedWarnOnceLogsOncePerKey(t *testing.T) {
	out := captureWarnLog(t)

	resetDegradedWarnOnce()

	degradedWarnOnce("test-degraded-a", "第一次降级")
	degradedWarnOnce("test-degraded-a", "第一次降级（重复）")
	degradedWarnOnce("test-degraded-b", "第二次降级")

	lines := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("同 key 降级应只打 1 行、不同 key 各打 1 行，实得 %d 行: %q", len(lines), lines)
	}

	// log 前缀带时间戳，只断言尾部文案。
	if !strings.HasSuffix(lines[0], "WARN: 第一次降级") {
		t.Errorf("第 1 行 = %q，期望以 %q 结尾", lines[0], "WARN: 第一次降级")
	}

	if !strings.HasSuffix(lines[1], "WARN: 第二次降级") {
		t.Errorf("第 2 行 = %q，期望以 %q 结尾", lines[1], "WARN: 第二次降级")
	}
}

// degradedProbeBody 一份企业端点能解析出单个模型的目录响应（含默认 agent 白名单，
// 否则 allowFull=false 的首路会被判成「无 agent models」）。
const degradedProbeBody = `{"code":0,"data":{"models":[` +
	`{"id":"m-degraded","name":"M","maxInputTokens":131072,"maxOutputTokens":8192}],` +
	`"agents":[{"name":"cli","models":["m-degraded"]}]}}`

// degradedModelClient 造一个「/v3/config 持续 500、企业端点正常」的探测客户端：
// 这是降级永续的典型形态，每轮探测都会命中同一条降级路径。global=true 时按
// global 域配置（ChatBaseGlobal + GlobalEnabled）。
func degradedModelClient(global bool) *Client {
	client := testClient(func(r *http.Request) (*http.Response, error) {
		if strings.Contains(r.URL.Path, "/v3/config") {
			return jsonResp(http.StatusInternalServerError, `{"code":10000,"msg":"boom"}`), nil
		}

		return jsonResp(http.StatusOK, degradedProbeBody), nil
	})
	if global {
		client.GlobalEnabled = true
		client.ChatBaseGlobal = "https://global.example"
	}

	return client
}

// TestFetchModelsDegradedWarnOnce CN 侧连续两轮降级只留一条 WARN，
// 且目录照常下发（降级不是失败）。
//
//nolint:paralleltest // 该用例改写进程级日志出口，必须串行执行
func TestFetchModelsDegradedWarnOnce(t *testing.T) {
	out := captureWarnLog(t)

	resetDegradedWarnOnce()

	client := degradedModelClient(false)
	account := &auth.Auth{UID: "u-cn", AccessToken: "at"}

	for round := range 2 {
		infos, err := client.FetchModels(account)
		if err != nil || len(infos) == 0 {
			t.Fatalf("第 %d 轮降级应照常下发目录，got infos=%d err=%v", round+1, len(infos), err)
		}
	}

	if got := strings.Count(out.String(), "fetch models: v3/config failed"); got != 1 {
		t.Fatalf("同一条 CN 降级应只打 1 行 WARN，实得 %d 行:\n%s", got, out.String())
	}
}

// TestFetchGlobalModelsDegradedWarnOnce global 侧同样只报一次：两轮独立探测
// （各自新建 Client，缓存互不复用）产生的降级日志行数必须与首轮相同。
//
//nolint:paralleltest // 该用例改写进程级日志出口，必须串行执行
func TestFetchGlobalModelsDegradedWarnOnce(t *testing.T) {
	out := captureWarnLog(t)

	resetDegradedWarnOnce()

	probe := func(round int) []string {
		client := degradedModelClient(true)

		names := client.FetchGlobalModels(&auth.Auth{
			UID: "u-gl", AccessToken: "at", Domain: "www.workbuddy.ai",
		})
		if len(names) == 0 {
			t.Fatalf("第 %d 轮降级应照常下发目录", round)
		}

		return names
	}

	probe(1)

	firstRound := strings.Count(out.String(), "WARN:")
	if firstRound == 0 {
		t.Fatalf("降级应至少留一条 WARN，实得 0 行")
	}

	probe(2)

	if secondRound := strings.Count(out.String(), "WARN:"); secondRound != firstRound {
		t.Fatalf("两轮降级日志行数应相同（去重），首轮 %d 行、两轮后 %d 行:\n%s",
			firstRound, secondRound, out.String())
	}
}

// TestFetchModelsBothRoutesFailReturnsError 真故障不被去重吞掉：两路全失败仍
// 返回错误（调用方据此进负缓存）。
//
//nolint:paralleltest // 该用例改写进程级日志出口，必须串行执行
func TestFetchModelsBothRoutesFailReturnsError(t *testing.T) {
	out := captureWarnLog(t)

	resetDegradedWarnOnce()

	client := testClient(func(*http.Request) (*http.Response, error) {
		return jsonResp(http.StatusInternalServerError, `{"code":10000,"msg":"boom"}`), nil
	})

	if infos, err := client.FetchModels(&auth.Auth{UID: "u-cn", AccessToken: "at"}); err == nil {
		t.Fatalf("两路全失败应返回错误，got infos=%d", len(infos))
	}

	// 两路全挂不是降级，不应有任何「degraded to」行。
	if got := strings.Count(out.String(), "degraded to"); got != 0 {
		t.Fatalf("真故障不应打降级 WARN，实得 %d 行:\n%s", got, out.String())
	}
}
