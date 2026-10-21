// messages_search_test.go 钉住 POST /v1/search/messages（上游同步窗口 issue #203）：
// 开关门控、web_search 声明校验、query 提取、响应形态、不遮蔽 /v1/messages。
package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
