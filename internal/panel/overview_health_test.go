// overview_health_test.go 钉住上游劣化滑窗在 overview 的挂载（issue #488）：
// 注入 provider 时输出 upstream_health，未注入时不输出该字段。
package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
	"github.com/BegoniaHe/wokbuddy-demon/internal/pool"
)

// getOverview 带凭据读取 overview 并解出 JSON。
func getOverview(t *testing.T, panel *Panel) map[string]any {
	t.Helper()

	req := httptest.NewRequest(http.MethodGet, "/panel/api/overview", nil)
	req.Header.Set("Authorization", "Bearer k")

	recorder := httptest.NewRecorder()
	panel.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", recorder.Code, recorder.Body)
	}

	var out map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode overview: %v", err)
	}

	return out
}

// TestOverviewIncludesUpstreamHealth 注入 provider 时 overview 带 upstream_health。
func TestOverviewIncludesUpstreamHealth(t *testing.T) {
	t.Parallel()

	panel := New(&Config{Version: "test", APIKey: "k", Pool: pool.New("")})
	panel.SetUpstreamHealth(func() any {
		return map[string]any{"enabled": true, "window": "5m"}
	})

	out := getOverview(t, panel)

	health, ok := out["upstream_health"].(map[string]any)
	if !ok {
		t.Fatalf("overview 缺 upstream_health: %+v", out)
	}

	if health["enabled"] != true || health["window"] != "5m" {
		t.Fatalf("upstream_health=%+v", health)
	}
}

// TestOverviewOmitsUpstreamHealthWithoutProvider 未注入时不输出该字段。
func TestOverviewOmitsUpstreamHealthWithoutProvider(t *testing.T) {
	t.Parallel()

	panel := New(&Config{Version: "test", APIKey: "k", Pool: pool.New("")})

	if _, ok := getOverview(t, panel)["upstream_health"]; ok {
		t.Fatal("未注入 provider 时不应输出 upstream_health")
	}
}

// TestOverviewExposesPaidUserTriState overview 的 accounts 行透出付费三态（issue #529）：
// true / false / null 分别对应付费 / 免费 / 未知。
func TestOverviewExposesPaidUserTriState(t *testing.T) {
	t.Parallel()

	testPool := pool.New("")
	t.Cleanup(testPool.Close)
	testPool.Add(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})

	panel := New(&Config{Version: "test", APIKey: "k", Pool: testPool})

	row := overviewAccountRow(t, getOverview(t, panel))
	if row["paid_user"] != nil {
		t.Fatalf("未观测应透出 null, got %v", row["paid_user"])
	}

	paid := true
	testPool.SetPaidStatus("u1", &paid)

	row = overviewAccountRow(t, getOverview(t, panel))
	if row["paid_user"] != true {
		t.Fatalf("付费号应透出 true, got %v", row["paid_user"])
	}
}

// overviewAccountRow 取 overview.accounts 的唯一一行。
func overviewAccountRow(t *testing.T, overview map[string]any) map[string]any {
	t.Helper()

	accounts, ok := overview["accounts"].([]any)
	if !ok || len(accounts) != 1 {
		t.Fatalf("accounts=%v want 1 行", overview["accounts"])
	}

	row, ok := accounts[0].(map[string]any)
	if !ok {
		t.Fatalf("accounts[0]=%T want map", accounts[0])
	}

	return row
}
