package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
	"github.com/BegoniaHe/wokbuddy-demon/internal/livecfg"
	"github.com/BegoniaHe/wokbuddy-demon/internal/upstream"
)

// TestAnthropicWebSearchExecutesServerTool 声明 web_search_20250305 时执行官方搜索，
// 回发 server_tool_use + web_search_tool_result，且不把服务端工具透传给 chat（issue #256）。
//
//nolint:cyclop // 端到端假上游分支。
func TestAnthropicWebSearchExecutesServerTool(t *testing.T) {
	t.Parallel()

	var searchCalls, chatCalls atomic.Int32

	var chatBody []byte

	client := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			switch {
			case strings.Contains(r.URL.Path, "/agenttool/v1/search"):
				searchCalls.Add(1)

				return jsonResponse(http.StatusOK, `{"query":"weather","results":[{"title":"Weather","url":"https://weather.example","snippet":"sunny"}],"total_results":1}`), nil
			case strings.Contains(r.URL.Path, "/v2/chat/completions"):
				chatCalls.Add(1)

				chatBody, _ = io.ReadAll(r.Body)

				return jsonResponse(http.StatusOK,
					"data: {\"id\":\"c1\",\"model\":\"glm-5.2\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"sunny\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"), nil
			default:
				return jsonResponse(http.StatusNotFound, `{"code":404}`), nil
			}
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}

	handler := NewHandler(&Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: client,
		Live:     livecfg.New(livecfg.Snapshot{SearchEndpoint: true}),
	})

	body := `{"model":"glm-5.2","max_tokens":64,"stream":false,
		"messages":[{"role":"user","content":"what is the weather"}],
		"tools":[{"type":"web_search_20250305","name":"web_search"}]}`
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body)))

	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}

	if searchCalls.Load() != 1 || chatCalls.Load() != 1 {
		t.Fatalf("search=%d chat=%d", searchCalls.Load(), chatCalls.Load())
	}

	out := rec.Body.String()
	for _, want := range []string{
		`"type":"server_tool_use"`, `"name":"web_search"`,
		`"type":"web_search_tool_result"`, `"url":"https://weather.example"`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("response missing %q: %s", want, out)
		}
	}

	if strings.Contains(string(chatBody), "web_search_20250305") {
		t.Fatalf("server tool must not be forwarded to chat: %s", chatBody)
	}

	if !strings.Contains(string(chatBody), "https://weather.example") {
		t.Fatalf("search results must be injected into chat context: %s", chatBody)
	}
}

// TestAnthropicNoWebSearchZeroOverhead 未声明 web_search 时不发起搜索。
func TestAnthropicNoWebSearchZeroOverhead(t *testing.T) {
	t.Parallel()

	var searchCalls atomic.Int32

	client := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if strings.Contains(r.URL.Path, "/agenttool/v1/search") {
				searchCalls.Add(1)
			}

			return jsonResponse(http.StatusOK,
				"data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"), nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}

	handler := NewHandler(&Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: client,
		Live:     livecfg.New(livecfg.Snapshot{SearchEndpoint: true}),
	})

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"glm-5.2","max_tokens":64,"stream":false,"messages":[{"role":"user","content":"hi"}]}`)))

	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}

	if searchCalls.Load() != 0 {
		t.Fatalf("search calls=%d want 0", searchCalls.Load())
	}
}

// TestAnthropicWebSearchFailureDegrades 搜索失败时仍完成对话，并回发错误结果块。
func TestAnthropicWebSearchFailureDegrades(t *testing.T) {
	t.Parallel()

	client := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if strings.Contains(r.URL.Path, "/agenttool/v1/search") {
				return jsonResponse(http.StatusInternalServerError, `{"code":500,"msg":"boom"}`), nil
			}

			return jsonResponse(http.StatusOK,
				"data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"), nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}

	handler := NewHandler(&Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: client,
		Live:     livecfg.New(livecfg.Snapshot{SearchEndpoint: true}),
	})

	body := `{"model":"glm-5.2","max_tokens":64,"stream":false,
		"messages":[{"role":"user","content":"what is the weather"}],
		"tools":[{"type":"web_search_20250305","name":"web_search"}]}`
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body)))

	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}

	var parsed struct {
		Content []struct {
			Type    string `json:"type"`
			Content any    `json:"content"`
		} `json:"content"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &parsed); err != nil {
		t.Fatal(err)
	}

	for _, block := range parsed.Content {
		if block.Type != "web_search_tool_result" {
			continue
		}

		// Anthropic 契约：搜索失败时 content 是单个错误对象，不是数组。
		if _, ok := block.Content.(map[string]any); !ok {
			t.Fatalf("web_search_tool_result error content must be an object, got %T: %s", block.Content, rec.Body)
		}

		return
	}

	t.Fatalf("response missing web_search_tool_result: %s", rec.Body)
}

// TestAnthropicWebSearchPassesOfficialFilters 服务端 web_search 的工具声明参数
// 透传到官方搜索请求体（issue #278 / #256）。
func TestAnthropicWebSearchPassesOfficialFilters(t *testing.T) {
	t.Parallel()

	var searchBody []byte

	client := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			switch {
			case strings.Contains(r.URL.Path, "/agenttool/v1/search"):
				searchBody, _ = io.ReadAll(r.Body)

				return jsonResponse(http.StatusOK, `{"query":"q","results":[]}`), nil
			default:
				return jsonResponse(http.StatusOK,
					"data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"), nil
			}
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}

	handler := NewHandler(&Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: client,
		Live:     livecfg.New(livecfg.Snapshot{SearchEndpoint: true}),
	})

	body := `{"model":"glm-5.2","max_tokens":64,"stream":false,
		"messages":[{"role":"user","content":"what is the weather"}],
		"tools":[{"type":"web_search_20250305","name":"web_search","max_results":3,
		"allowed_domains":["weather.example"],"blocked_domains":["spam.example"],"freshness":"d2"}]}`
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body)))

	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}

	for _, want := range []string{
		`"query":"what is the weather (-site:spam.example)"`,
		`"max_results":3`, `"allowed_domains":["weather.example"]`, `"freshness":"d2"`,
	} {
		if !strings.Contains(string(searchBody), want) {
			t.Errorf("search body=%s want 含 %s", searchBody, want)
		}
	}
}
