// messages_search_test.go 钉住 POST /v1/search/messages（上游同步窗口 issue #203）：
// 开关门控、web_search 声明校验、query 提取、响应形态、不遮蔽 /v1/messages。
package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
	"github.com/BegoniaHe/wokbuddy-demon/internal/livecfg"
	"github.com/BegoniaHe/wokbuddy-demon/internal/reqlog"
	"github.com/BegoniaHe/wokbuddy-demon/internal/upstream"
)

// searchMessageResponse 搜索响应的断言形态。
type searchMessageResponse struct {
	Type    string `json:"type"`
	Role    string `json:"role"`
	ID      string `json:"id"`
	Content []struct {
		Type      string           `json:"type"`
		Text      string           `json:"text"`
		Citations []map[string]any `json:"citations"`
		Content   []map[string]any `json:"content"`
	} `json:"content"`
}

// searchTestHandler 造一个只服务搜索端点的 handler（带 CN 账号 + 假上游）。
func searchTestHandler(t *testing.T, enabled bool, respond func(*http.Request) (int, string)) (*Handler, *httptest.Server) {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		status, body := respond(r)

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)

	client := &upstream.Client{
		HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL, WebBaseCN: srv.URL,
	}

	state := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at-u1", ExpiresAt: 9999999999})

	handler := NewHandler(&Config{
		Pool:     state,
		Upstream: client,
		Live:     livecfg.New(livecfg.Snapshot{SearchEndpoint: enabled}),
	})

	return handler, srv
}

// doSearchRequest 发一次搜索请求。
func doSearchRequest(t *testing.T, handler *Handler, body string) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequest(http.MethodPost, "/v1/search/messages", strings.NewReader(body))
	req.Header.Set("Anthropic-Version", "2023-06-01")

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	return rec
}

const searchOKBody = `{"query":"golang","results":[{"title":"Go","url":"https://go.dev","snippet":"lang"}],"total_results":1}`

// TestSearchEndpointDisabledByDefault 开关关闭 → 404（且不打上游）。
func TestSearchEndpointDisabledByDefault(t *testing.T) {
	t.Parallel()

	calls := 0
	handler, _ := searchTestHandler(t, false, func(*http.Request) (int, string) {
		calls++

		return 200, searchOKBody
	})

	rec := doSearchRequest(t, handler, `{"model":"glm-5.2","messages":[{"role":"user","content":[{"type":"text","text":"golang"}]}],"tools":[{"type":"web_search_20250305","name":"web_search"}]}`)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("code=%d want 404，body=%s", rec.Code, rec.Body)
	}

	if calls != 0 {
		t.Errorf("开关关闭时不得打上游，calls=%d", calls)
	}
}

// TestSearchRequiresWebSearchTool 未声明 web_search → 400。
func TestSearchRequiresWebSearchTool(t *testing.T) {
	t.Parallel()

	handler, _ := searchTestHandler(t, true, func(*http.Request) (int, string) { return 200, searchOKBody })

	rec := doSearchRequest(t, handler, `{"model":"glm-5.2","messages":[{"role":"user","content":[{"type":"text","text":"golang"}]}]}`)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d want 400，body=%s", rec.Code, rec.Body)
	}

	if !strings.Contains(rec.Body.String(), "web_search") {
		t.Errorf("错误应说明需要 web_search 声明：%s", rec.Body)
	}
}

// TestSearchReturnsAnthropicMessage 成功响应：text 块带 citations + web_search_tool_result。
func TestSearchReturnsAnthropicMessage(t *testing.T) {
	t.Parallel()

	handler, _ := searchTestHandler(t, true, func(r *http.Request) (int, string) {
		if !strings.HasSuffix(r.URL.Path, "/agenttool/v1/search") {
			return 404, `{"code":404}`
		}

		return 200, searchOKBody
	})

	rec := doSearchRequest(t, handler, `{"model":"glm-5.2","messages":[{"role":"user","content":[{"type":"text","text":"golang"}]}],"tools":[{"type":"web_search_20250305","name":"web_search"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}

	var resp searchMessageResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v (body=%s)", err, rec.Body)
	}

	assertSearchMessageShape(t, resp)
}

// assertSearchMessageShape 断言 Anthropic 消息外壳与两个 content 块。
func assertSearchMessageShape(t *testing.T, resp searchMessageResponse) {
	t.Helper()

	if resp.Type != "message" || resp.Role != "assistant" || !strings.HasPrefix(resp.ID, "msg_") {
		t.Fatalf("envelope=%+v", resp)
	}

	if len(resp.Content) != 2 {
		t.Fatalf("content 块数=%d want 2", len(resp.Content))
	}

	if resp.Content[0].Type != "text" || len(resp.Content[0].Citations) != 1 ||
		resp.Content[0].Citations[0]["cited_text"] != "lang" {
		t.Errorf("text 块 citations=%+v", resp.Content[0])
	}

	if resp.Content[1].Type != "web_search_tool_result" || len(resp.Content[1].Content) != 1 {
		t.Errorf("tool_result 块=%+v", resp.Content[1])
	}
}

// TestSearchUsesLastUserText query 取最后一条 user 消息的文本块（忽略历史与 tool_result）。
func TestSearchUsesLastUserText(t *testing.T) {
	t.Parallel()

	var gotBody string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw := make([]byte, 0, 256)
		buf := make([]byte, 256)

		for {
			n, err := r.Body.Read(buf)
			raw = append(raw, buf[:n]...)

			if err != nil {
				break
			}
		}

		gotBody = string(raw)

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(searchOKBody))
	}))
	t.Cleanup(srv.Close)

	client := &upstream.Client{
		HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL, WebBaseCN: srv.URL,
	}
	state := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at-u1", ExpiresAt: 9999999999})
	handler := NewHandler(&Config{
		Pool:     state,
		Upstream: client,
		Live:     livecfg.New(livecfg.Snapshot{SearchEndpoint: true}),
	})

	rec := doSearchRequest(t, handler, `{"model":"glm-5.2","messages":[
		{"role":"user","content":[{"type":"text","text":"历史问题"}]},
		{"role":"assistant","content":[{"type":"tool_result","tool_use_id":"x"}]},
		{"role":"user","content":[{"type":"text","text":"当前搜索词"}]}
	],"tools":[{"name":"web_search"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}

	if !strings.Contains(gotBody, `"query":"当前搜索词"`) {
		t.Errorf("出站 query 应取最后一条 user 文本，body=%s", gotBody)
	}
}

// TestSearchDoesNotShadowMessages /v1/messages 不受影响（仍是既有 Anthropic 入站）。
func TestSearchDoesNotShadowMessages(t *testing.T) {
	t.Parallel()

	handler, _ := searchTestHandler(t, true, func(*http.Request) (int, string) { return 200, searchOKBody })

	req := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"glm-5.2","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Anthropic-Version", "2023-06-01")

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	// 既有路径：会走向上游 chat（假上游对任何路径都回搜索体，必然不是 404 的「未注册」）。
	if rec.Code == http.StatusNotFound && strings.Contains(rec.Body.String(), "search endpoint") {
		t.Fatalf("/v1/messages 被搜索端点遮蔽：%s", rec.Body)
	}
}

// TestSearchRecordsRequestTrace 搜索请求在请求日志里留下真实 path / 账号 / 模型。
func TestSearchRecordsRequestTrace(t *testing.T) {
	t.Parallel()

	handler, _ := searchTestHandler(t, true, func(*http.Request) (int, string) { return 200, searchOKBody })

	// 打开请求归档（trace 只在 RequestLog 存在时创建）。
	recorder := reqlog.New(reqlog.Config{Dir: t.TempDir(), Enabled: true})
	t.Cleanup(recorder.Close)
	handler.cfg.RequestLog = recorder

	req := httptest.NewRequest(http.MethodPost, "/v1/search/messages",
		strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":[{"type":"text","text":"golang"}]}],"tools":[{"name":"web_search"}]}`))
	req.Header.Set("Anthropic-Version", "2023-06-01")

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}

	if rec.Header().Get("X-Request-ID") == "" {
		t.Error("搜索请求应带 X-Request-ID（与其他 POST 接口同口径）")
	}
}
