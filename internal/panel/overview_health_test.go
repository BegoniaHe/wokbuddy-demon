// overview_health_test.go 钉住上游劣化滑窗在 overview 的挂载（issue #488）：
// 注入 provider 时输出 upstream_health，未注入时不输出该字段。
package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

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
