// windowprobe_test.go 钉住额度窗口探针的执行侧（issue #492）：到点发一条
// max_tokens=1 的最小请求，成功出账；失败只记日志、不罚号、不冷却。
package scheduler

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
	"github.com/BegoniaHe/wokbuddy-demon/internal/pool"
	"github.com/BegoniaHe/wokbuddy-demon/internal/upstream"
)

// windowProbeUpstream 返回一个记录探针请求的假上游。
func windowProbeUpstream(
	t *testing.T, status int, bodies *[]map[string]any,
) (*upstream.Client, *atomic.Int64) {
	t.Helper()

	var calls atomic.Int64

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)

		var body map[string]any
		if json.Unmarshal(raw, &body) == nil {
			*bodies = append(*bodies, body)
		}

		calls.Add(1)

		if status >= http.StatusBadRequest {
			w.WriteHeader(status)
			w.Write([]byte(`{"code":500,"msg":"boom","access_token":"sk-secret-value"}`))

			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("data: {\"id\":\"x\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"}}]}\n\n"))
		w.Write([]byte("data: [DONE]\n\n"))
	}))
	t.Cleanup(srv.Close)

	return &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}, &calls
}

// windowProbeScheduler 构造带注入时钟的调度器与到期窗口。
func windowProbeScheduler(
	t *testing.T, upstreamClient *upstream.Client,
) (*Scheduler, *pool.Pool, *time.Time) {
	t.Helper()

	now := time.Now()
	state := pool.New("")
	t.Cleanup(state.Close)
	state.SetClock(func() time.Time { return now })
	state.Add(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	state.CooldownSoftForModel("u1", time.Minute, now.Add(time.Minute), "glm-5.2", "6004")

	now = now.Add(2 * time.Minute)

	sched := New(&Config{Pool: state, Upstream: upstreamClient})

	return sched, state, &now
}

// TestWindowProbeSendsMinimalRequest 到点后发一条最小请求，成功后出账且不罚号。
func TestWindowProbeSendsMinimalRequest(t *testing.T) {
	t.Parallel()

	var bodies []map[string]any

	up, calls := windowProbeUpstream(t, http.StatusOK, &bodies)
	sched, state, _ := windowProbeScheduler(t, up)

	sched.runWindowProbeOnce(context.Background())

	if calls.Load() != 1 {
		t.Fatalf("calls=%d want 1", calls.Load())
	}

	if len(bodies) != 1 {
		t.Fatalf("bodies=%d want 1", len(bodies))
	}

	if bodies[0]["model"] != "glm-5.2" {
		t.Errorf("model=%v want glm-5.2", bodies[0]["model"])
	}

	if tokens, _ := bodies[0]["max_tokens"].(float64); tokens != 1 {
		t.Errorf("max_tokens=%v want 1（最小代价）", bodies[0]["max_tokens"])
	}

	if state.WindowProbeCount() != 0 {
		t.Errorf("成功后应出清台账, count=%d", state.WindowProbeCount())
	}

	st, _ := state.Status("u1")
	if st.ConsecutiveFails != 0 || st.BreakerFails != 0 || st.Cooling {
		t.Errorf("探针成功后不应罚号: %+v", st)
	}
}

// TestWindowProbeFailureDoesNotPenalize 上游 5xx：只记日志、保留台账待重试、不罚号。
func TestWindowProbeFailureDoesNotPenalize(t *testing.T) {
	t.Parallel()

	var bodies []map[string]any

	up, calls := windowProbeUpstream(t, http.StatusInternalServerError, &bodies)
	sched, state, _ := windowProbeScheduler(t, up)

	var buf strings.Builder

	sched.logger = log.New(&buf, "", 0)

	sched.runWindowProbeOnce(context.Background())

	if calls.Load() != 1 {
		t.Fatalf("calls=%d want 1", calls.Load())
	}

	if state.WindowProbeCount() != 1 {
		t.Errorf("失败应保留台账待重试, count=%d", state.WindowProbeCount())
	}

	st, _ := state.Status("u1")
	if st.ConsecutiveFails != 0 || st.BreakerFails != 0 || st.Cooling {
		t.Errorf("探针失败不应罚号: %+v", st)
	}

	if !strings.Contains(buf.String(), "额度窗口探针失败") {
		t.Errorf("应记一条失败日志, got %q", buf.String())
	}

	if strings.Contains(buf.String(), "sk-secret-value") {
		t.Errorf("日志不应回显凭证, got %q", buf.String())
	}
}

// TestWindowProbePausedWhenDisabled 间隔为 0 时循环暂停，但手动一轮仍可执行。
func TestWindowProbePausedWhenDisabled(t *testing.T) {
	t.Parallel()

	var bodies []map[string]any

	up, _ := windowProbeUpstream(t, http.StatusOK, &bodies)
	sched, _, _ := windowProbeScheduler(t, up)

	sched.SetWindowProbeInterval(0)

	if got := sched.windowProbeInterval.Load(); got != 0 {
		t.Fatalf("interval=%d want 0", got)
	}
}
