// messages_search_test.go 钉住 POST /v1/search/messages（上游同步窗口 issue #203）：
// 开关门控、web_search 声明校验、query 提取、响应形态、不遮蔽 /v1/messages。
package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/BegoniaHe/wokbuddy-demon/internal/apikey"
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
		ToolUseID string           `json:"tool_use_id"`
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
//
//nolint:cyclop // 逐字段校验响应契约，拆开反而难读。
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

	// Anthropic 的 web_search_tool_result 必须带 tool_use_id（每次调用唯一标识）。
	if !strings.HasPrefix(resp.Content[1].ToolUseID, "srvtoolu_") || len(resp.Content[1].ToolUseID) <= len("srvtoolu_") {
		t.Errorf("tool_use_id=%q want srvtoolu_ 前缀且非空", resp.Content[1].ToolUseID)
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

// TestSearchStripsOfficialQueryPrefix 官方搜索客户端（DSH / 官方 CLI）会把固定指令
// 前缀一起送来（`Perform a web search for the query: <query>`）：前缀必须剥掉，
// 否则会把指令本身当搜索词打给上游。大小写差异也按同口径处理。
func TestSearchStripsOfficialQueryPrefix(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		content string
		want    string
	}{
		{
			name:    "块数组 + 标准前缀",
			content: `[{"type":"text","text":"Perform a web search for the query: golang"}]`,
			want:    `"query":"golang"`,
		},
		{
			name:    "纯字符串 + 大小写差异",
			content: `"perform a WEB SEARCH for the query: 中文查询"`,
			want:    `"query":"中文查询"`,
		},
		{
			name:    "无前缀原样保留",
			content: `[{"type":"text","text":"golang"}]`,
			want:    `"query":"golang"`,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
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

			body := `{"model":"glm-5.2","messages":[{"role":"user","content":` + testCase.content + `}],"tools":[{"name":"web_search"}]}`
			if rec := doSearchRequest(t, handler, body); rec.Code != http.StatusOK {
				t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
			}

			if !strings.Contains(gotBody, testCase.want) {
				t.Errorf("出站 query 应剥离官方前缀，body=%s", gotBody)
			}

			if strings.Contains(gotBody, "Perform a web search") || strings.Contains(gotBody, "perform a WEB SEARCH") {
				t.Errorf("固定指令前缀不得进入上游 query，body=%s", gotBody)
			}
		})
	}
}

// TestSearchResultTitleFallsBackToURL 结果无 title 时用 URL 兜底：Anthropic 客户端
// 按 title 渲染结果行，空标题会显示成空白条目；tool_use_id 每次响应都必须唯一。
func TestSearchResultTitleFallsBackToURL(t *testing.T) {
	t.Parallel()

	msg := buildSearchMessage("m", "q", &upstream.SearchOutcome{
		Results: []upstream.SearchResult{{URL: "https://example.com/a"}},
	})

	content, ok := msg["content"].([]map[string]any)
	if !ok || len(content) != 2 {
		t.Fatalf("content=%v", msg["content"])
	}

	citations, _ := content[0]["citations"].([]map[string]any)
	if len(citations) != 1 || citations[0]["title"] != "https://example.com/a" {
		t.Errorf("citation title=%v want URL 兜底", citations)
	}

	results, _ := content[1]["content"].([]map[string]any)
	if len(results) != 1 || results[0]["title"] != "https://example.com/a" {
		t.Errorf("result title=%v want URL 兜底", results)
	}

	id, _ := content[1]["tool_use_id"].(string)
	if !strings.HasPrefix(id, "srvtoolu_") {
		t.Errorf("tool_use_id=%q want srvtoolu_ 前缀", id)
	}
}

// TestSearchForwardsSessionHeaders 入站会话头族必须透传给上游（上游按会话聚合
// 搜索流量）：同一次搜索的换号重试复用同一聚合主键，客户端缺 request ID 时本地生成。
func TestSearchForwardsSessionHeaders(t *testing.T) {
	t.Parallel()

	var got http.Header

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()

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

	req := httptest.NewRequest(http.MethodPost, "/v1/search/messages",
		strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"golang"}],"tools":[{"name":"web_search"}]}`))
	req.Header.Set("Anthropic-Version", "2023-06-01")
	req.Header.Set("X-Conversation-Id", "conv-1")
	req.Header.Set("X-Conversation-Request-Id", "req-1")
	req.Header.Set("X-Trace-Id", "trace-1")

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}

	if got.Get("X-Conversation-Id") != "conv-1" {
		t.Errorf("X-Conversation-Id=%q want conv-1", got.Get("X-Conversation-Id"))
	}

	if got.Get("X-Conversation-Request-Id") != "req-1" {
		t.Errorf("X-Conversation-Request-Id=%q want req-1", got.Get("X-Conversation-Request-Id"))
	}

	if got.Get("X-Trace-Id") != "trace-1" {
		t.Errorf("X-Trace-Id=%q want trace-1", got.Get("X-Trace-Id"))
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

// TestSearchMaxResultsPassthrough 请求体 / tools 里的 max_results 透传并收敛。
func TestSearchMaxResultsPassthrough(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		body string
		want string
	}{
		{
			name: "请求体顶层 max_results",
			body: `{"model":"m","max_results":3,"messages":[{"role":"user","content":[{"type":"text","text":"go"}]}],"tools":[{"name":"web_search"}]}`,
			want: `"max_results":3`,
		},
		{
			name: "tools 里的 max_results",
			body: `{"model":"m","messages":[{"role":"user","content":[{"type":"text","text":"go"}]}],"tools":[{"name":"web_search","max_results":7}]}`,
			want: `"max_results":7`,
		},
		{
			name: "超上限收敛到 10",
			body: `{"model":"m","max_results":99,"messages":[{"role":"user","content":[{"type":"text","text":"go"}]}],"tools":[{"name":"web_search"}]}`,
			want: `"max_results":10`,
		},
		{
			name: "缺省用默认 5",
			body: `{"model":"m","messages":[{"role":"user","content":[{"type":"text","text":"go"}]}],"tools":[{"name":"web_search"}]}`,
			want: `"max_results":5`,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
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

			client := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL, WebBaseCN: srv.URL}
			state := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at-u1", ExpiresAt: 9999999999})
			handler := NewHandler(&Config{
				Pool: state, Upstream: client,
				Live: livecfg.New(livecfg.Snapshot{SearchEndpoint: true}),
			})

			if rec := doSearchRequest(t, handler, testCase.body); rec.Code != http.StatusOK {
				t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
			}

			if !strings.Contains(gotBody, testCase.want) {
				t.Errorf("出站 body=%s want 含 %s", gotBody, testCase.want)
			}
		})
	}
}

// TestSearchRejectsTooLongQuery 超长搜索词在本地拒绝（不打上游）。
func TestSearchRejectsTooLongQuery(t *testing.T) {
	t.Parallel()

	calls := 0
	handler, _ := searchTestHandler(t, true, func(*http.Request) (int, string) {
		calls++

		return 200, searchOKBody
	})

	long := strings.Repeat("搜", searchQueryMaxLen+1)
	rec := doSearchRequest(t, handler, `{"model":"m","messages":[{"role":"user","content":[{"type":"text","text":"`+long+`"}]}],"tools":[{"name":"web_search"}]}`)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d want 400", rec.Code)
	}

	if calls != 0 {
		t.Errorf("超长搜索词不应打上游，calls=%d", calls)
	}
}

// TestSearchUsageIsEstimated 响应 usage 是估算值（非恒 0），供客户端预算。
func TestSearchUsageIsEstimated(t *testing.T) {
	t.Parallel()

	handler, _ := searchTestHandler(t, true, func(*http.Request) (int, string) { return 200, searchOKBody })

	rec := doSearchRequest(t, handler, `{"model":"m","messages":[{"role":"user","content":[{"type":"text","text":"golang search"}]}],"tools":[{"name":"web_search"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}

	var resp struct {
		Usage struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if resp.Usage.InputTokens < 1 || resp.Usage.OutputTokens < 1 {
		t.Errorf("usage=%+v want 估算值均 >= 1", resp.Usage)
	}
}

// TestSearchRetryableExhaustReturns429 可重试错误（429）耗尽轮转后必须回 429 +
// upstream 原文：曾经这一路返回零值 failure，写到 WriteHeader(0) 直接 panic
// （客户端只看到空 200）。
func TestSearchRetryableExhaustReturns429(t *testing.T) {
	t.Parallel()

	handler, _ := searchTestHandler(t, true, func(*http.Request) (int, string) {
		return http.StatusTooManyRequests, `{"code":429,"msg":"rate limited"}`
	})

	rec := doSearchRequest(t, handler, `{"model":"glm-5.2","messages":[{"role":"user","content":[{"type":"text","text":"golang"}]}],"tools":[{"name":"web_search"}]}`)

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("code=%d want 429，body=%s", rec.Code, rec.Body)
	}

	if !strings.Contains(rec.Body.String(), "rate_limit_error") || !strings.Contains(rec.Body.String(), "rate limited") {
		t.Errorf("错误信封应带 rate_limit_error 与上游原文：%s", rec.Body)
	}
}

// TestSearchAcceptsStringContent Anthropic Messages 允许 content 是纯字符串：
// 这种形态此前被解析成「invalid JSON body」，属误报。
func TestSearchAcceptsStringContent(t *testing.T) {
	t.Parallel()

	handler, _ := searchTestHandler(t, true, func(*http.Request) (int, string) {
		return http.StatusOK, searchOKBody
	})

	rec := doSearchRequest(t, handler, `{"model":"glm-5.2","messages":[{"role":"user","content":"golang 用法"}],"tools":[{"name":"web_search"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d want 200，body=%s", rec.Code, rec.Body)
	}
}

// TestSearchRejectsGlobalBoundKey 搜索只放行 CN 域：绑定 global 出口的命名密钥
// 不得消耗 CN 账号额度（出口绑定是运维侧的显式约束）。
func TestSearchRejectsGlobalBoundKey(t *testing.T) {
	t.Parallel()

	calls := 0

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(searchOKBody))
	}))
	t.Cleanup(srv.Close)

	client := &upstream.Client{
		HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL, WebBaseCN: srv.URL,
	}
	state := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at-u1", ExpiresAt: 9999999999})

	store := apikey.New("")

	_, raw, err := store.Create("global-key")
	if err != nil {
		t.Fatalf("create key: %v", err)
	}

	if _, err := store.SetRealm(store.List()[0].ID, "global"); err != nil {
		t.Fatalf("set realm: %v", err)
	}

	handler := NewHandler(&Config{
		Pool:     state,
		Upstream: client,
		APIKey:   "secret",
		Keys:     store,
		Live:     livecfg.New(livecfg.Snapshot{SearchEndpoint: true}),
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/search/messages",
		strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":[{"type":"text","text":"golang"}]}],"tools":[{"name":"web_search"}]}`))
	req.Header.Set("Anthropic-Version", "2023-06-01")
	req.Header.Set("Authorization", "Bearer "+raw)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "cn realm") {
		t.Fatalf("code=%d body=%s want 400 且说明 cn realm 限制", rec.Code, rec.Body)
	}

	if calls != 0 {
		t.Errorf("global 绑定 key 不得打 CN 上游，calls=%d", calls)
	}
}

// TestSearchRejectsSingleRuneQuery 搜索词长度按字符计：单个 CJK 字符是 3 字节，
// 不能因为字节数 >= 2 就放过（上游 zod 按字符数要求至少 2）。
func TestSearchRejectsSingleRuneQuery(t *testing.T) {
	t.Parallel()

	calls := 0
	handler, _ := searchTestHandler(t, true, func(*http.Request) (int, string) {
		calls++

		return 200, searchOKBody
	})

	rec := doSearchRequest(t, handler, `{"model":"m","messages":[{"role":"user","content":[{"type":"text","text":"搜"}]}],"tools":[{"name":"web_search"}]}`)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d want 400，body=%s", rec.Code, rec.Body)
	}

	if calls != 0 {
		t.Errorf("过短搜索词不应打上游，calls=%d", calls)
	}
}

// TestSearchClientErrorIsTerminal 未知 4xx（ErrClient）是请求级错误：应回 400
// 且不再逐号轮转、不把一次坏请求记成账号失败。
func TestSearchClientErrorIsTerminal(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"code":1,"msg":"bad request"}`))
	}))
	t.Cleanup(srv.Close)

	client := &upstream.Client{
		HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL, WebBaseCN: srv.URL,
	}
	state := testPoolWith(
		&auth.Auth{UID: "u1", AccessToken: "at-u1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "u2", AccessToken: "at-u2", ExpiresAt: 9999999999},
	)
	handler := NewHandler(&Config{
		Pool:     state,
		Upstream: client,
		Live:     livecfg.New(livecfg.Snapshot{SearchEndpoint: true}),
	})

	rec := doSearchRequest(t, handler, `{"model":"glm-5.2","messages":[{"role":"user","content":[{"type":"text","text":"golang"}]}],"tools":[{"name":"web_search"}]}`)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d want 400，body=%s", rec.Code, rec.Body)
	}

	if got := calls.Load(); got != 1 {
		t.Errorf("请求级 4xx 不应逐号轮转，calls=%d", got)
	}

	for _, uid := range []string{"u1", "u2"} {
		if st := statusOf(t, state, uid); st.ErrTotal != 0 || st.Cooling {
			t.Errorf("请求级 4xx 不应罚账号 %s: %+v", uid, st)
		}
	}
}

// TestSearch401RefreshesAndReplaysOnce 上游 401 → 同号 refresh 后重放一次（与 chat
// 同口径）：不把可恢复的 token 过期降级成请求级 400，也不惩罚账号。
//
//nolint:cyclop // 覆盖 401 → refresh → 重放 全链路，拆开反而难读。
func TestSearch401RefreshesAndReplaysOnce(t *testing.T) {
	t.Parallel()

	var searchCalls, refreshCalls atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		if strings.HasSuffix(r.URL.Path, "/v2/plugin/auth/token/refresh") {
			refreshCalls.Add(1)

			_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{"accessToken":"at-new","refreshToken":"rt-new","expiresIn":3600}}`))

			return
		}

		if searchCalls.Add(1) == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"code":401,"msg":"unauthorized"}`))

			return
		}

		_, _ = w.Write([]byte(searchOKBody))
	}))
	t.Cleanup(srv.Close)

	client := &upstream.Client{
		HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL, WebBaseCN: srv.URL,
	}
	account := &auth.Auth{
		UID: "u1", AccessToken: "at-old", RefreshToken: "rt-old", ExpiresAt: 9999999999,
		FilePath: filepath.Join(t.TempDir(), "u1.json"),
	}
	state := testPoolWith(account)
	handler := NewHandler(&Config{
		Pool:     state,
		Upstream: client,
		Live:     livecfg.New(livecfg.Snapshot{SearchEndpoint: true}),
	})

	rec := doSearchRequest(t, handler, `{"model":"glm-5.2","messages":[{"role":"user","content":"golang"}],"tools":[{"type":"web_search_20250305","name":"web_search"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d want 200（401 应 refresh 后重放），body=%s", rec.Code, rec.Body)
	}

	if got := searchCalls.Load(); got != 2 {
		t.Errorf("search calls=%d want 2（原请求 + 重放）", got)
	}

	if got := refreshCalls.Load(); got != 1 {
		t.Errorf("refresh calls=%d want 1", got)
	}

	if account.AccessToken != "at-new" || account.RefreshToken != "rt-new" {
		t.Errorf("token 未更新: access=%q refresh=%q", account.AccessToken, account.RefreshToken)
	}

	if st := statusOf(t, state, "u1"); st.Cooling || st.Disabled || st.ErrTotal != 0 {
		t.Errorf("401 refresh 成功不应惩罚账号: %+v", st)
	}
}

// TestSearch401AfterRefreshIsRetryable 重放后仍 401：按账号级可换号处理（不是
// 请求级 400）；单号池轮转耗尽后返回 5xx，而不是把 token 故障说成客户端问题。
func TestSearch401AfterRefreshIsRetryable(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		if strings.HasSuffix(r.URL.Path, "/v2/plugin/auth/token/refresh") {
			_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{"accessToken":"at-new","refreshToken":"rt-new","expiresIn":3600}}`))

			return
		}

		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"code":401,"msg":"unauthorized"}`))
	}))
	t.Cleanup(srv.Close)

	client := &upstream.Client{
		HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL, WebBaseCN: srv.URL,
	}
	account := &auth.Auth{
		UID: "u1", AccessToken: "at-old", RefreshToken: "rt-old", ExpiresAt: 9999999999,
		FilePath: filepath.Join(t.TempDir(), "u1.json"),
	}
	state := testPoolWith(account)
	handler := NewHandler(&Config{
		Pool:     state,
		Upstream: client,
		Live:     livecfg.New(livecfg.Snapshot{SearchEndpoint: true}),
	})

	rec := doSearchRequest(t, handler, `{"model":"glm-5.2","messages":[{"role":"user","content":"golang"}],"tools":[{"type":"web_search_20250305","name":"web_search"}]}`)
	if rec.Code < 500 {
		t.Fatalf("code=%d want 5xx（401 不是请求级 400），body=%s", rec.Code, rec.Body)
	}

	if strings.Contains(rec.Body.String(), "bad_params") {
		t.Errorf("401 不应映射成 bad_params: %s", rec.Body)
	}
}

// TestSearchRealmPrefix 搜索端点只走 CN：显式 global: 前缀必须拒绝（否则客户端
// 以为在查国际版，实际消耗 CN 额度）；cn: 前缀剥掉后按裸名回显。
func TestSearchRealmPrefix(t *testing.T) {
	t.Parallel()

	calls := 0
	handler, _ := searchTestHandler(t, true, func(*http.Request) (int, string) {
		calls++

		return http.StatusOK, searchOKBody
	})

	rec := doSearchRequest(t, handler, `{"model":"global:glm-5.2","messages":[{"role":"user","content":"golang"}],"tools":[{"name":"web_search"}]}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("global 前缀 code=%d want 400，body=%s", rec.Code, rec.Body)
	}

	if calls != 0 {
		t.Errorf("global 前缀不得打上游，calls=%d", calls)
	}

	rec = doSearchRequest(t, handler, `{"model":"cn:glm-5.2","messages":[{"role":"user","content":"golang"}],"tools":[{"name":"web_search"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("cn 前缀 code=%d want 200，body=%s", rec.Code, rec.Body)
	}

	if !strings.Contains(rec.Body.String(), `"model":"glm-5.2"`) {
		t.Errorf("响应 model 应剥掉 cn: 前缀：%s", rec.Body)
	}

	if calls != 1 {
		t.Errorf("cn 前缀应打上游一次，calls=%d", calls)
	}
}
