// upstreamhealth_test.go 钉住上游劣化滑窗（issue #488）：TTFB / 5xx 阈值告警、
// 样本不足不告警、关闭时零行为，且统计不改变选号。
package server

import (
	"log"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
)

// newHealthForTest 构造注入时钟与日志缓冲的滑窗。
func newHealthForTest(
	t *testing.T, window, ttfb time.Duration, ratio float64, minCount int,
) (*upstreamHealth, *time.Time, *strings.Builder) {
	t.Helper()

	now := time.Now()
	logs := &strings.Builder{}

	health := newUpstreamHealth(window, ttfb, ratio, minCount)
	health.nowFn = func() time.Time { return now }
	health.logger = log.New(logs, "", 0)

	return health, &now, logs
}

// healthRow 从快照中取指定 (域, 模型) 行。
func healthRow(t *testing.T, snapshot UpstreamHealthSnapshot, realm, model string) UpstreamHealthRow {
	t.Helper()

	for _, row := range snapshot.Rows {
		if row.Realm == realm && row.Model == model {
			return row
		}
	}

	t.Fatalf("快照缺少 %s:%s: %+v", realm, model, snapshot.Rows)

	return UpstreamHealthRow{}
}

// TestUpstreamHealthTTFBThreshold TTFB 均值超阈值告警并标记劣化。
func TestUpstreamHealthTTFBThreshold(t *testing.T) {
	t.Parallel()

	health, _, logs := newHealthForTest(t, time.Minute, 100*time.Millisecond, 0.25, 3)

	for range 3 {
		health.observe("cn", "glm-5.2", 500*time.Millisecond, false)
	}

	if !strings.Contains(logs.String(), "上游劣化") {
		t.Fatalf("应打出劣化 WARN，got %q", logs.String())
	}

	assertTTFBHealthSnapshot(t, health.snapshot())
}

// assertTTFBHealthSnapshot 断言慢 TTFB 的模型行与全局行都标记劣化。
func assertTTFBHealthSnapshot(t *testing.T, snapshot UpstreamHealthSnapshot) {
	t.Helper()

	if !snapshot.Enabled || snapshot.Window != "1m0s" || snapshot.TTFBThresholdMs != 100 {
		t.Fatalf("snapshot=%+v", snapshot)
	}

	row := healthRow(t, snapshot, "cn", "glm-5.2")
	if !row.TTFBDegraded || !row.Degraded || row.AvgTTFBMs != 500 || row.TTFBSamples != 3 {
		t.Fatalf("row=%+v want ttfb degraded at 500ms", row)
	}

	global := healthRow(t, snapshot, "*", "*")
	if !global.Degraded || global.Attempts != 3 {
		t.Fatalf("global=%+v want aggregated degraded row", global)
	}
}

// TestUpstreamHealthBelowThresholdNoWarn 未超阈值不告警、不标记劣化。
func TestUpstreamHealthBelowThresholdNoWarn(t *testing.T) {
	t.Parallel()

	health, _, logs := newHealthForTest(t, time.Minute, 500*time.Millisecond, 0.25, 3)

	for range 3 {
		health.observe("cn", "glm-5.2", 100*time.Millisecond, false)
	}

	if logs.Len() != 0 {
		t.Fatalf("未超阈值不应告警，got %q", logs.String())
	}

	row := healthRow(t, health.snapshot(), "cn", "glm-5.2")
	if row.Degraded {
		t.Fatalf("row=%+v want not degraded", row)
	}
}

// TestUpstreamHealthErrorRatio 5xx 比例超阈值告警；等于阈值不算超。
func TestUpstreamHealthErrorRatio(t *testing.T) {
	t.Parallel()

	health, _, logs := newHealthForTest(t, time.Minute, 5*time.Second, 0.25, 4)

	for range 4 {
		health.observe("global", "glm-5.2", 0, false)
		health.markServer5xx("global", "glm-5.2")
	}

	if !strings.Contains(logs.String(), "上游劣化") {
		t.Fatalf("应打出劣化 WARN，got %q", logs.String())
	}

	row := healthRow(t, health.snapshot(), "global", "glm-5.2")
	if !row.ErrorDegraded || row.Server5xx != 4 || row.ErrorRatio != 1 {
		t.Fatalf("row=%+v want 4/4 5xx degraded", row)
	}

	atThreshold, _, quiet := newHealthForTest(t, time.Minute, 5*time.Second, 0.25, 4)
	for range 4 {
		atThreshold.observe("cn", "glm-5.2", 0, false)
	}

	atThreshold.markServer5xx("cn", "glm-5.2")

	if quiet.Len() != 0 {
		t.Fatalf("25%% 恰好等于阈值不应告警，got %q", quiet.String())
	}

	if row := healthRow(t, atThreshold.snapshot(), "cn", "glm-5.2"); row.Degraded {
		t.Fatalf("row=%+v want not degraded at threshold", row)
	}
}

// TestUpstreamHealthMinSamplesGuard 样本不足时即便全部 5xx 也不告警。
func TestUpstreamHealthMinSamplesGuard(t *testing.T) {
	t.Parallel()

	health, _, logs := newHealthForTest(t, time.Minute, 5*time.Second, 0.25, 10)

	health.observe("cn", "glm-5.2", 0, false)
	health.markServer5xx("cn", "glm-5.2")

	if logs.Len() != 0 {
		t.Fatalf("样本不足不应告警，got %q", logs.String())
	}

	if row := healthRow(t, health.snapshot(), "cn", "glm-5.2"); row.Degraded {
		t.Fatalf("row=%+v want not degraded", row)
	}
}

// TestUpstreamHealthWarnCooldown 同一窗口内只告警一次。
func TestUpstreamHealthWarnCooldown(t *testing.T) {
	t.Parallel()

	health, _, logs := newHealthForTest(t, time.Minute, 100*time.Millisecond, 0.25, 2)

	for range 6 {
		health.observe("cn", "glm-5.2", time.Second, false)
	}

	// 模型键与全局键各自最多告警一次（两条不同的 key）。
	if got := strings.Count(logs.String(), "上游劣化 cn:glm-5.2"); got != 1 {
		t.Fatalf("同一窗口内模型键应只告警一次，got %d: %q", got, logs.String())
	}

	if got := strings.Count(logs.String(), "上游劣化 全局"); got != 1 {
		t.Fatalf("同一窗口内全局键应只告警一次，got %d: %q", got, logs.String())
	}
}

// TestUpstreamHealthWindowExpiry 样本滑出窗口后不再计入。
func TestUpstreamHealthWindowExpiry(t *testing.T) {
	t.Parallel()

	health, now, logs := newHealthForTest(t, time.Minute, 100*time.Millisecond, 0.25, 2)

	health.observe("cn", "glm-5.2", time.Second, false)
	health.observe("cn", "glm-5.2", time.Second, false)

	if logs.Len() == 0 {
		t.Fatal("首个窗口应告警")
	}

	*now = now.Add(2 * time.Minute)

	logs.Reset()

	assertNoHealthSamples(t, health)
}

// assertNoHealthSamples 断言窗口滑过后快照为空。
func assertNoHealthSamples(t *testing.T, health *upstreamHealth) {
	t.Helper()

	if rows := health.snapshot().Rows; len(rows) != 0 {
		t.Fatalf("窗口滑过后应无样本: %+v", rows)
	}
}

// TestUpstreamHealthDisabled window<=0 时不统计、不告警。
func TestUpstreamHealthDisabled(t *testing.T) {
	t.Parallel()

	health, _, logs := newHealthForTest(t, 0, time.Second, 0.25, 1)

	health.observe("cn", "glm-5.2", time.Second, true)
	health.markServer5xx("cn", "glm-5.2")

	if logs.Len() != 0 {
		t.Fatalf("关闭时不应告警: %q", logs.String())
	}

	snapshot := health.snapshot()
	if snapshot.Enabled || len(snapshot.Rows) != 0 {
		t.Fatalf("snapshot=%+v want disabled", snapshot)
	}
}

// TestUpstreamHealthEndToEndWarnAndSelectionUnchanged 连续 5xx 触发 WARN 与指标变化，
// 但账号不受冷却、选号仍能拿到该账号（统计与轮转解耦）。
func TestUpstreamHealthEndToEndWarnAndSelectionUnchanged(t *testing.T) {
	t.Parallel()

	state := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		return http.StatusInternalServerError, `{"code":500,"msg":"boom"}`, false
	})
	handler := NewHandler(&Config{
		Pool: state, Upstream: up, MaxRotate: 1,
		UpstreamHealthWindow:     time.Minute,
		UpstreamHealthTTFB:       5 * time.Second,
		UpstreamHealth5xxRatio:   0.5,
		UpstreamHealthMinSamples: 2,
	})

	logs := &strings.Builder{}
	handler.upstreamHealth.logger = log.New(logs, "", 0)

	for range 2 {
		rec := postChat(t, handler, `{"model":"glm-5.2","messages":[]}`)
		if rec.Code == http.StatusOK {
			t.Fatalf("上游 5xx 不应回 200: %s", rec.Body)
		}
	}

	if !strings.Contains(logs.String(), "上游劣化") {
		t.Fatalf("连续 5xx 应告警，got %q", logs.String())
	}

	row := healthRow(t, handler.UpstreamHealthSnapshot(), "cn", "glm-5.2")
	if !row.ErrorDegraded || row.Server5xx < 2 {
		t.Fatalf("row=%+v want degraded with >=2 5xx", row)
	}

	if status, _ := state.Status("u1"); status.Cooling {
		t.Fatalf("劣化统计不应冷却账号: %+v", status)
	}

	if picked := state.PickExcludingForRealm(nil, "glm-5.2", "cn"); picked == nil {
		t.Fatal("劣化统计不应改变选号结果（仍应选到 u1）")
	}
}
