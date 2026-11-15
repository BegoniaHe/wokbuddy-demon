package panel

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestDesktopScanEndpoint 扫描接口只返回元数据，200 且不含 token 字段。
func TestDesktopScanEndpoint(t *testing.T) {
	t.Parallel()

	pnl := New(&Config{Version: "test"})
	req := httptest.NewRequest(http.MethodGet, "/panel/api/import/desktop/scan", nil)
	// 本机端点有回环护栏（与 agents* 同口径）：扫描会回本机绝对路径。
	req.RemoteAddr = "127.0.0.1:12345"

	rec := httptest.NewRecorder()
	pnl.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}

	body := rec.Body.String()
	if !strings.Contains(body, `"accounts"`) {
		t.Fatalf("body missing accounts: %s", body)
	}

	if strings.Contains(body, "accessToken") || strings.Contains(body, "refreshToken") {
		t.Fatalf("scan leaked token fields: %s", body)
	}
}
