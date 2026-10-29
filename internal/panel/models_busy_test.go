package panel

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// modelsJSON 构造 fake 上游 JSON 响应。
func modelsJSON(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

// modelsResp 解析 /panel/api/models 成功响应体。
type modelsResp struct {
	OK          bool             `json:"ok"`
	Models      []map[string]any `json:"models"`
	FetchErrors []string         `json:"fetch_errors"`
	BusyErrors  []string         `json:"busy_errors"`
}

// getModels 打一次模型页接口，返回记录器与（200 时的）解析结果。
func getModels(t *testing.T, p *Panel) (*httptest.ResponseRecorder, modelsResp) {
	t.Helper()

	rec := httptest.NewRecorder()
	p.models(rec, httptest.NewRequest(http.MethodGet, "/panel/api/models", nil))

	var resp modelsResp
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode models resp: %v", err)
		}
	}

	return rec, resp
}

// TestModelsBusyErrorsDetached 繁忙度观测失败不再污染目录错误面（issue #401）：
// 目录正常时 fetch_errors 为空，繁忙度明细另走 busy_errors，条目只是少一列 busy。
func TestModelsBusyErrorsDetached(t *testing.T) {
	t.Parallel()

	panel, _ := newProbePanel(t, func(req *http.Request) (*http.Response, error) {
		switch {
		case strings.HasSuffix(req.URL.Path, "/v3/config"):
			return modelsJSON(http.StatusOK, `{"code":0,"data":{"models":[
				{"id":"glm-5.2","maxOutputTokens":8192}
			],"agents":[{"name":"cli","models":["glm-5.2"]}]}}`), nil
		case strings.HasSuffix(req.URL.Path, "/v2/chat/busy"):
			return modelsJSON(http.StatusInternalServerError, `{"code":500,"msg":"boom"}`), nil
		default:
			return modelsJSON(http.StatusInternalServerError, `{"code":1,"msg":"no agent"}`), nil
		}
	})

	rec, resp := getModels(t, panel)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d want 200", rec.Code)
	}

	if len(resp.Models) != 1 {
		t.Fatalf("models=%v want 1 条", resp.Models)
	}

	if len(resp.FetchErrors) != 0 {
		t.Fatalf("fetch_errors=%v want 空（繁忙度失败不属于目录错误）", resp.FetchErrors)
	}

	if len(resp.BusyErrors) != 1 || !strings.Contains(resp.BusyErrors[0], "cn: busy: ") {
		t.Fatalf("busy_errors=%v want 一条 cn: busy:", resp.BusyErrors)
	}

	if _, ok := resp.Models[0]["busy"]; ok {
		t.Fatalf("观测失败时不应写 busy 列: %v", resp.Models[0])
	}
}

// TestModelsCatalogErrorOnly 目录全失败时 502 文案只讲目录，不掺繁忙度条目。
func TestModelsCatalogErrorOnly(t *testing.T) {
	t.Parallel()

	panel, _ := newProbePanel(t, func(*http.Request) (*http.Response, error) {
		return modelsJSON(http.StatusInternalServerError, `{"code":1,"msg":"boom"}`), nil
	})

	rec, resp := getModels(t, panel)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("code=%d want 502", rec.Code)
	}

	if len(resp.Models) != 0 {
		t.Fatalf("目录全失败不应返回条目: %v", resp.Models)
	}

	body := rec.Body.String()
	if !strings.Contains(body, "fetch models:") {
		t.Fatalf("502 文案应指明目录失败: %s", body)
	}

	if strings.Contains(body, "busy:") {
		t.Fatalf("502 文案不应包含繁忙度明细: %s", body)
	}
}
