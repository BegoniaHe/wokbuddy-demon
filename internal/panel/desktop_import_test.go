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

	p := New(&Config{Version: "test"})
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/panel/api/import/desktop/scan", nil))

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
