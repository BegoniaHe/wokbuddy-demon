package panel

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BegoniaHe/wokbuddy-demon/internal/agentcfg"
)

func agentsPanel(t *testing.T, apiKey string) (*Panel, string) {
	t.Helper()
	home := t.TempDir()
	p := New(&Config{Version: "test", APIKey: apiKey, Agents: agentcfg.New(home, filepath.Join(home, "backups"))})

	return p, home
}

func TestAgentsApplyAndRestoreLoopback(t *testing.T) {
	t.Parallel()
	panel, home := agentsPanel(t, "k")
	body := `{"base_url":"http://127.0.0.1:7863","api_key":"sk-test","model":"glm-5.2"}`
	req := httptest.NewRequest(http.MethodPost, "/panel/api/agents/claude/apply", strings.NewReader(body))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Authorization", "Bearer k")

	rec := httptest.NewRecorder()
	panel.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("apply code=%d body=%s", rec.Code, rec.Body)
	}

	if strings.Contains(rec.Body.String(), "sk-test") {
		t.Error("response must not echo api key")
	}

	cfgPath := filepath.Join(home, ".claude", "settings.json")

	restore := httptest.NewRequest(http.MethodPost, "/panel/api/agents/claude/restore", strings.NewReader(`{}`))
	restore.RemoteAddr = "127.0.0.1:12345"
	restore.Header.Set("Authorization", "Bearer k")

	rec2 := httptest.NewRecorder()
	panel.ServeHTTP(rec2, restore)

	if rec2.Code != http.StatusOK {
		t.Fatalf("restore code=%d body=%s", rec2.Code, rec2.Body)
	}

	if _, err := os.Stat(cfgPath); !os.IsNotExist(err) {
		t.Error("created config should be removed on restore")
	}
}

func TestAgentsRejectNonLoopback(t *testing.T) {
	t.Parallel()
	p, _ := agentsPanel(t, "")
	req := httptest.NewRequest(http.MethodGet, "/panel/api/agents", nil)
	req.RemoteAddr = "192.0.2.10:1234"
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("code=%d want 403", rec.Code)
	}
}

// TestAgentsApplyRejectsMalformedJSON 畸形请求体返回 400，不得把尾随数据静默
// 忽略后仍写入客户端配置。
func TestAgentsApplyRejectsMalformedJSON(t *testing.T) {
	t.Parallel()
	panel, _ := agentsPanel(t, "")

	req := httptest.NewRequest(http.MethodPost, "/panel/api/agents/claude/apply",
		strings.NewReader(`{"base_url":"http://127.0.0.1:7863","api_key":"sk-test"} trailing`))
	req.RemoteAddr = "127.0.0.1:12345"

	rec := httptest.NewRecorder()
	panel.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d body=%s, want 400", rec.Code, rec.Body)
	}
}

// TestAgentsRestoreRejectsMalformedJSON 畸形请求体返回 400，不得把 force 当
// 默认 false 继续执行还原。
func TestAgentsRestoreRejectsMalformedJSON(t *testing.T) {
	t.Parallel()
	panel, _ := agentsPanel(t, "")

	req := httptest.NewRequest(http.MethodPost, "/panel/api/agents/claude/restore", strings.NewReader(`{"force":`))
	req.RemoteAddr = "127.0.0.1:12345"

	rec := httptest.NewRecorder()
	panel.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d body=%s, want 400", rec.Code, rec.Body)
	}

	trailing := httptest.NewRequest(http.MethodPost, "/panel/api/agents/claude/restore", strings.NewReader(`{"force":true} trailing`))
	trailing.RemoteAddr = "127.0.0.1:12345"

	rec = httptest.NewRecorder()
	panel.ServeHTTP(rec, trailing)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("trailing data: code=%d body=%s, want 400", rec.Code, rec.Body)
	}
}
