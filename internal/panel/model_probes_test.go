package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// model_probes 端点三态：文件存在 → 透传 + exists:true + updated_at；
// 未配置 / 文件缺失 → 空集（面板退化为无标注）；文件损坏 → 502。
// 契约字段（claimed/measured/verdict…）由前端消费，网关只透传不解析。
func TestModelProbesEndpoint(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	str := filepath.Join(dir, "output_probes.json")

	body := `{"version":1,"probes":{"cn:glm-5.2":{"claimed":131072,"measured":48000,` +
		`"verdict":"clamped","note":"钳制","tested_at":"2026-09-15 18:30:00","source":"probe_max_tokens.py"}}}`
	if err := os.WriteFile(str, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	// 1) 文件存在：透传 + exists + updated_at
	rec := getModelProbes(Config{Version: "test", APIKey: "test-key", ProbeFile: str})
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}

	var got struct {
		Probes    map[string]json.RawMessage `json:"probes"`
		Exists    bool                       `json:"exists"`
		UpdatedAt string                     `json:"updated_at"`
	}
	decodeJSONBytes(t, rec.Body.Bytes(), &got)

	if !got.Exists || len(got.Probes) != 1 || got.UpdatedAt == "" {
		t.Fatalf("exists=%v probes=%d updated_at=%q", got.Exists, len(got.Probes), got.UpdatedAt)
	}

	var rec1 struct {
		Verdict  string `json:"verdict"`
		Measured int64  `json:"measured"`
	}
	decodeJSONBytes(t, got.Probes["cn:glm-5.2"], &rec1)

	if rec1.Verdict != "clamped" || rec1.Measured != 48000 {
		t.Fatalf("probe passthrough = %+v", rec1)
	}
}

// TestModelProbesEndpointMissing 文件缺失 / 未配置都回空集 200（不是错误——无数据 = 无标注）。
// 注意每个 case 用全新结构体：向已非 nil 的 map 再次 Unmarshal 是合并不是替换。
func TestModelProbesEndpointMissing(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	rec := getModelProbes(Config{Version: "test", APIKey: "test-key", ProbeFile: filepath.Join(dir, "nope.json")})
	if rec.Code != http.StatusOK {
		t.Fatalf("missing file: code=%d want 200", rec.Code)
	}

	var gotEmpty struct {
		Probes map[string]json.RawMessage `json:"probes"`
		Exists bool                       `json:"exists"`
	}
	decodeJSONBytes(t, rec.Body.Bytes(), &gotEmpty)

	if gotEmpty.Exists || len(gotEmpty.Probes) != 0 {
		t.Fatalf("missing file: exists=%v probes=%d, want false/0", gotEmpty.Exists, len(gotEmpty.Probes))
	}

	// 未配置：同文件缺失
	rec = getModelProbes(Config{Version: "test", APIKey: "test-key"})
	if rec.Code != http.StatusOK {
		t.Fatalf("unconfigured: code=%d want 200", rec.Code)
	}
}

// TestModelProbesEndpointCorrupt 文件损坏回 502（让面板显示读取失败而不是静默空白）。
func TestModelProbesEndpointCorrupt(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	bad := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(bad, []byte(`{not json`), 0o600); err != nil {
		t.Fatal(err)
	}

	rec := getModelProbes(Config{Version: "test", APIKey: "test-key", ProbeFile: bad})
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("corrupt file: code=%d want 502", rec.Code)
	}
}

// getModelProbes 带鉴权请求 model_probes 端点。
func getModelProbes(cfg Config) *httptest.ResponseRecorder {
	p := New(&cfg)
	req := httptest.NewRequest(http.MethodGet, "/panel/api/model_probes", nil)
	req.Header.Set("Authorization", "Bearer test-key")

	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	return rec
}

// decodeJSONBytes 解析 JSON，失败即 Fatal。
func decodeJSONBytes(t *testing.T, data []byte, v any) {
	t.Helper()

	if err := json.Unmarshal(data, v); err != nil {
		t.Fatal(err)
	}
}
