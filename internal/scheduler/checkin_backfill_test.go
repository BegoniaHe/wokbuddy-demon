// checkin_backfill_test.go 钉住签到瞬时失败的分钟级补签（issue #501）：主批三次
// 全败后按窗口补签、已签账号零上游调用、ctx 取消立即退出、非瞬时错误不进队列。
package scheduler

import (
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
	"github.com/BegoniaHe/wokbuddy-demon/internal/pool"
	"github.com/BegoniaHe/wokbuddy-demon/internal/upstream"
)

// backfillTestServer 前 failFirst 次签到请求返回 500「请稍后重试」，之后成功；
// 余额查询恒成功。calls 是签到请求总数（含批内 retryBillingTransient 补打）。
func backfillTestServer(t *testing.T, failFirst int32) (*httptest.Server, *atomic.Int32) {
	t.Helper()

	var calls atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/daily-checkin"):
			if calls.Add(1) <= failFirst {
				w.WriteHeader(http.StatusInternalServerError)
				w.Write([]byte(`{"code":10001,"msg":"签到失败，请稍后重试"}`))

				return
			}

			// 成功回执按官方平铺形态（UPSTREAM.md）：空 data 信封没有判据字段，
			// 会被 checkinReceipt 判失败，补签路径就永远等不到 CheckinDone。
			w.Write([]byte(`{"credit":100,"streak_days":1,"is_streak_day":true}`))
		case strings.HasSuffix(r.URL.Path, "/get-user-resource"):
			w.Write([]byte(`{"code":0,"data":{"Response":{"Data":{"Accounts":[` +
				`{"CycleCapacitySize":1000,"CycleCapacityRemain":500,"CycleCapacityUsed":0}]}}}}`))
		default:
			http.Error(w, "not found", http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	return srv, &calls
}

// backfillTestScheduler 单账号调度器；批内重试间隔压到 1ms，补签窗口由调用方注入。
func backfillTestScheduler(
	t *testing.T, srv *httptest.Server, delays []time.Duration,
) (*Scheduler, *pool.Pool) {
	t.Helper()

	state := pool.New("")
	state.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})

	client := &upstream.Client{
		HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL,
		RetryDelay: time.Millisecond,
	}
	s := New(&Config{Pool: state, Upstream: client})
	s.backfillDelays = delays
	s.logger = log.New(io.Discard, "", 0)

	return s, state
}

// waitCheckinDone 轮询等待账号标记为今日已签（上限 timeout）。
func waitCheckinDone(t *testing.T, state *pool.Pool, timeout time.Duration) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if st, _ := state.Status("u1"); st.CheckinDone {
			return
		}

		time.Sleep(5 * time.Millisecond)
	}

	t.Fatal("等待补签成功超时")
}

// TestCheckinTransientBackfillSucceeds 主批 3 次全败（首签 + 2 次批内重试）→
// 补签窗口内签成，checkin_done 落库；补签不阻塞本轮返回。
func TestCheckinTransientBackfillSucceeds(t *testing.T) {
	t.Parallel()

	srv, calls := backfillTestServer(t, 3)
	scheduler, state := backfillTestScheduler(t, srv, []time.Duration{40 * time.Millisecond, 400 * time.Millisecond})

	ctx, cancel := context.WithCancel(context.Background())
	scheduler.startBackfillWorker(ctx)

	t.Cleanup(func() {
		cancel()
		scheduler.backfillWG.Wait()
	})

	scheduler.RunCheckinNowCtx(ctx)

	if st, _ := state.Status("u1"); st.CheckinDone {
		t.Fatal("主批 3 次全败不应标记已签")
	}

	if got := calls.Load(); got != 3 {
		t.Fatalf("主批签到调用=%d want 3（首签 + 2 次批内重试）", got)
	}

	waitCheckinDone(t, state, 2*time.Second)

	if got := calls.Load(); got != 4 {
		t.Errorf("补签后签到调用=%d want 4（窗口内补一次即成功）", got)
	}
}

// TestCheckinBackfillSkipsAlreadyChecked 用户已在手机上签到（本地 checkin_done）
// → 补签窗口零上游调用，不重复打接口。
func TestCheckinBackfillSkipsAlreadyChecked(t *testing.T) {
	t.Parallel()

	srv, calls := backfillTestServer(t, 3)
	scheduler, state := backfillTestScheduler(t, srv, []time.Duration{60 * time.Millisecond})

	ctx, cancel := context.WithCancel(context.Background())
	scheduler.startBackfillWorker(ctx)

	t.Cleanup(func() {
		cancel()
		scheduler.backfillWG.Wait()
	})

	scheduler.RunCheckinNowCtx(ctx)
	state.NoteCheckinDone("u1") // 模拟用户在别处已签到

	time.Sleep(200 * time.Millisecond)

	if got := calls.Load(); got != 3 {
		t.Errorf("已签账号不应补签，签到调用=%d want 3", got)
	}
}

// TestCheckinBackfillStopsOnCancel ctx 取消后补签立即退出且不再打上游。
func TestCheckinBackfillStopsOnCancel(t *testing.T) {
	t.Parallel()

	srv, calls := backfillTestServer(t, 3)
	scheduler, _ := backfillTestScheduler(t, srv, []time.Duration{500 * time.Millisecond})

	ctx, cancel := context.WithCancel(context.Background())
	scheduler.startBackfillWorker(ctx)
	scheduler.RunCheckinNowCtx(ctx)

	cancel()
	scheduler.backfillWG.Wait()

	if got := calls.Load(); got != 3 {
		t.Errorf("取消后不应补签，签到调用=%d want 3", got)
	}
}

// TestCheckinNonTransientSkipsBackfill 业务拒绝（400 参数错）不进补签队列：
// 排进队列只会每天多打几轮注定失败的请求。
func TestCheckinNonTransientSkipsBackfill(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/daily-checkin") {
			calls.Add(1)
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`{"code":11101,"msg":"Unmarshal chat params failed"}`))

			return
		}

		http.Error(w, "not found", http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)

	scheduler, _ := backfillTestScheduler(t, srv, []time.Duration{40 * time.Millisecond})

	ctx, cancel := context.WithCancel(context.Background())
	scheduler.startBackfillWorker(ctx)

	t.Cleanup(func() {
		cancel()
		scheduler.backfillWG.Wait()
	})

	scheduler.RunCheckinNowCtx(ctx)
	time.Sleep(200 * time.Millisecond)

	if got := calls.Load(); got != 1 {
		t.Errorf("非瞬时错误不应补签，签到调用=%d want 1", got)
	}
}

// TestCheckinBackfillWorkerStartConcurrentWithEnqueue 钉住生产路径的并发安全：
// main.go 的 go sch.Run(ctx) 启动 worker 与面板手动签到任务的入队可能并发。
// backfillCh 必须在 New 构造期创建、worker 启动路径只置原子哨兵，不能再对字段
// 赋值——否则启动写与入队读形成 data race（-race 必报）。空账号池 + 不存在的
// uid 保证 worker 即便消费到该批也直接跳过，不触网上游、不留悬挂 goroutine。
func TestCheckinBackfillWorkerStartConcurrentWithEnqueue(t *testing.T) {
	t.Parallel()

	scheduler := New(&Config{Pool: pool.New("")})
	scheduler.backfillDelays = []time.Duration{time.Millisecond}

	ctx, cancel := context.WithCancel(context.Background())

	t.Cleanup(func() {
		cancel()
		scheduler.backfillWG.Wait()
	})

	start := make(chan struct{})

	var wg sync.WaitGroup

	wg.Add(2)

	go func() {
		defer wg.Done()

		<-start
		scheduler.startBackfillWorker(ctx)
	}()

	go func() {
		defer wg.Done()

		<-start
		scheduler.enqueueCheckinBackfill([]checkinBackfillTarget{{uid: "ghost"}})
	}()

	close(start)
	wg.Wait()
}
