// refresh_singleflight_test.go 钉住同账号 refresh 单飞（上游 #217 / issue #184）。
//
// 背景：上游对 refreshToken 反重放，同一 RT 被消费后再用会返回 12153 并整条 RT 链
// 作废（账号从此恒 401，只能人工重登）。本 fork 的并发来源更多（scheduler keepalive、
// handler 临近过期刷新与 401 重放、cmd/signin），因此单飞是保号手段。
package upstream

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
)

// refreshProbe 记录上游 refresh 端点的调用次数与并发峰值。
type refreshProbe struct {
	mu       sync.Mutex
	calls    int
	inFlight int
	maxPeak  int
	delay    time.Duration
	// respond 返回该次调用的 (状态码, body)。
	respond func(call int, refreshToken string) (int, string)
}

func (p *refreshProbe) roundTrip(r *http.Request) (*http.Response, error) {
	call, token := p.enter(r.Header.Get("X-Refresh-Token"))

	defer p.leave()

	if p.delay > 0 {
		time.Sleep(p.delay)
	}

	status, body := p.respond(call, token)

	return jsonResp(status, body), nil
}

// enter 记一次调用并返回调用序号与该请求携带的 RT。
func (p *refreshProbe) enter(token string) (call int, refreshToken string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.calls++
	p.inFlight++

	if p.inFlight > p.maxPeak {
		p.maxPeak = p.inFlight
	}

	return p.calls, token
}

// leave 结束一次调用。
func (p *refreshProbe) leave() {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.inFlight--
}

func (p *refreshProbe) snapshot() (calls, peak int) {
	p.mu.Lock()
	defer p.mu.Unlock()

	return p.calls, p.maxPeak
}

const refreshedOK = `{"code":0,"msg":"ok","data":{"accessToken":"newat","refreshToken":"newrt","expiresIn":3600}}`

// TestRefreshSingleFlightPerAccount 同账号 8 并发只打上游一次，其余等待并复用结果。
func TestRefreshSingleFlightPerAccount(t *testing.T) {
	t.Parallel()

	probe := &refreshProbe{
		delay: 20 * time.Millisecond,
		respond: func(int, string) (int, string) {
			return http.StatusOK, refreshedOK
		},
	}
	client := testClient(probe.roundTrip)

	account := &auth.Auth{UID: "u1", AccessToken: "at1", RefreshToken: "rt1", ExpiresAt: 1}

	var wg sync.WaitGroup

	errs := make([]error, 8)

	for i := range errs {
		wg.Go(func() {
			errs[i] = client.RefreshToken(account)
		})
	}

	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("goroutine %d: refresh err=%v", i, err)
		}
	}

	calls, peak := probe.snapshot()
	if calls != 1 {
		t.Errorf("upstream refresh calls=%d want 1（单飞后并发只打一次）", calls)
	}

	if peak != 1 {
		t.Errorf("max in-flight=%d want 1", peak)
	}

	if account.AccessToken != "newat" || account.RefreshToken != "newrt" {
		t.Errorf("tokens not updated by the single flight: %+v", account)
	}
}

// TestRefreshSingleFlightKeepsAccountsParallel 不同账号仍可并行：单飞按 UID 分桶，
// 不能退化成全局串行（否则 keepalive 全量刷新会被拖慢）。
func TestRefreshSingleFlightKeepsAccountsParallel(t *testing.T) {
	t.Parallel()

	probe := &refreshProbe{
		delay: 30 * time.Millisecond,
		respond: func(int, string) (int, string) {
			return http.StatusOK, refreshedOK
		},
	}
	client := testClient(probe.roundTrip)

	accounts := []*auth.Auth{
		{UID: "a", AccessToken: "at-a", RefreshToken: "rt-a"},
		{UID: "b", AccessToken: "at-b", RefreshToken: "rt-b"},
		{UID: "c", AccessToken: "at-c", RefreshToken: "rt-c"},
	}

	var wg sync.WaitGroup

	for _, account := range accounts {
		wg.Go(func() {
			if err := client.RefreshToken(account); err != nil {
				t.Errorf("refresh %s: %v", account.UID, err)
			}
		})
	}

	wg.Wait()

	calls, peak := probe.snapshot()
	if calls != 3 {
		t.Errorf("upstream calls=%d want 3（每个账号一次）", calls)
	}

	if peak < 2 {
		t.Errorf("max in-flight=%d want >=2（不同账号必须并行）", peak)
	}
}

// writeAuthFile 落一份扁平形 auth 文件，返回路径。
func writeAuthFile(t *testing.T, doc string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "workbuddy-test.json")
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatalf("write auth file: %v", err)
	}

	return path
}

// TestRefreshAdoptsNewerDiskTokens 12153 后磁盘 RT 已被别的进程 rotate：
// 采纳磁盘 token 重试一次，成功。
func TestRefreshAdoptsNewerDiskTokens(t *testing.T) {
	t.Parallel()

	probe := &refreshProbe{
		respond: func(_ int, refreshToken string) (int, string) {
			if refreshToken == "disk-rt" {
				return http.StatusOK, refreshedOK
			}

			return http.StatusUnauthorized, `{"code":12153,"msg":"Offline user session not found"}`
		},
	}
	client := testClient(probe.roundTrip)

	path := writeAuthFile(t, `{"accessToken":"disk-at","refreshToken":"disk-rt","uid":"u1"}`)
	account := &auth.Auth{UID: "u1", AccessToken: "stale-at", RefreshToken: "stale-rt", FilePath: path}

	if err := client.RefreshToken(account); err != nil {
		t.Fatalf("refresh should succeed after adopting disk tokens, err=%v", err)
	}

	calls, _ := probe.snapshot()
	if calls != 2 {
		t.Errorf("upstream calls=%d want 2（12153 一次 + 采纳磁盘 token 重试一次）", calls)
	}

	if account.AccessToken != "newat" || account.RefreshToken != "newrt" {
		t.Errorf("tokens not updated: %+v", account)
	}
}

// TestRefreshDiskUnchangedDoesNotRetry 磁盘 RT 与内存一致时不重试，错误原样返回。
func TestRefreshDiskUnchangedDoesNotRetry(t *testing.T) {
	t.Parallel()

	probe := &refreshProbe{
		respond: func(int, string) (int, string) {
			return http.StatusUnauthorized, `{"code":12153,"msg":"Offline user session not found"}`
		},
	}
	client := testClient(probe.roundTrip)

	path := writeAuthFile(t, `{"accessToken":"at","refreshToken":"rt1","uid":"u1"}`)
	account := &auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt1", FilePath: path}

	err := client.RefreshToken(account)
	if err == nil {
		t.Fatal("want session-dead error")
	}

	if !isSessionDeadErr(err) {
		t.Errorf("err=%v want session-dead", err)
	}

	calls, _ := probe.snapshot()
	if calls != 1 {
		t.Errorf("upstream calls=%d want 1（磁盘 RT 未变不得重试）", calls)
	}
}

// TestRefreshWithoutFilePathDoesNotRetry 无 FilePath（内存 auth）时不做磁盘采纳，
// 12153 直接失败且只打一次上游。
func TestRefreshWithoutFilePathDoesNotRetry(t *testing.T) {
	t.Parallel()

	probe := &refreshProbe{
		respond: func(int, string) (int, string) {
			return http.StatusUnauthorized, `{"code":12153,"msg":"Offline user session not found"}`
		},
	}
	client := testClient(probe.roundTrip)

	account := &auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt1"}
	if err := client.RefreshToken(account); err == nil {
		t.Fatal("want session-dead error")
	}

	calls, _ := probe.snapshot()
	if calls != 1 {
		t.Errorf("upstream calls=%d want 1", calls)
	}
}

// TestRefreshSingleFlightSharesError 首个调用失败时，等待者拿到同一错误（不重打上游）。
func TestRefreshSingleFlightSharesError(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32

	client := testClient(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		time.Sleep(10 * time.Millisecond)

		return jsonResp(http.StatusInternalServerError, `{"code":500,"msg":"boom"}`), nil
	})

	account := &auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt1"}

	var wg sync.WaitGroup

	for range 4 {
		wg.Go(func() {
			if err := client.RefreshToken(account); err == nil {
				t.Error("want error")
			}
		})
	}

	wg.Wait()

	if got := calls.Load(); got != 1 {
		t.Errorf("upstream calls=%d want 1（失败结果同样复用）", got)
	}

	if !strings.Contains(account.RefreshToken, "rt1") {
		t.Errorf("失败不得改写 refreshToken, got %q", account.RefreshToken)
	}
}
