// tools_search_test.go 钉住 POST /v1/tools/search（DeepSeek 官方形状，issue #608）：
// 开关门控、入参校验、results[] 契约、上游失败沿用非对话端点处置。
package server

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"unicode/utf8"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
	"github.com/BegoniaHe/wokbuddy-demon/internal/jsonx"
	"github.com/BegoniaHe/wokbuddy-demon/internal/livecfg"
	"github.com/BegoniaHe/wokbuddy-demon/internal/upstream"
)

const toolsSearchPath = "/v1/tools/search"

// newToolsSearchTestHandler 造一个只服务 /v1/tools/search 的 handler（单 CN 账号 + 假上游）。
func newToolsSearchTestHandler(
	t *testing.T, enabled bool, respond func(*http.Request) (*http.Response, error),
) *Handler {
	t.Helper()

	return NewHandler(&Config{
		Pool: testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: &upstream.Client{
			HTTP:          &http.Client{Transport: roundTripFunc(respond)},
			ChatBaseCN:    "https://fake.example",
			BillingBaseCN: "https://fake.example",
		},
		Live: livecfg.New(livecfg.Snapshot{SearchEndpoint: enabled}),
	})
}

func postToolsSearch(t *testing.T, handler *Handler, body string) *httptest.ResponseRecorder {
	t.Helper()

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, toolsSearchPath, strings.NewReader(body)))

	return rec
}

// toolsSearchDecoded 断言形态：results 解成 map，键齐全性由测试显式检查。
type toolsSearchDecoded struct {
	Results []map[string]any `json:"results"`
}

// toolsSearchResultKeys DeepSeek 契约里每条结果必须出现的全部键（值可为 null）。
var toolsSearchResultKeys = []string{
	"rerank_score", "title", "url", "snippet", "summary",
	"content", "site_name", "site_icon", "date", "authority",
}

// assertToolsSearchResultKeys 断言每条结果都带全部键（不缺键、不多键）。
func assertToolsSearchResultKeys(t *testing.T, result map[string]any) {
	t.Helper()

	if len(result) != len(toolsSearchResultKeys) {
		t.Fatalf("result keys=%v want %v", result, toolsSearchResultKeys)
	}

	for _, key := range toolsSearchResultKeys {
		if _, ok := result[key]; !ok {
			t.Fatalf("missing key %q in %v", key, result)
		}
	}
}

// TestToolsSearchDisabledIs403NoUpstream 开关关闭 → 403，且不出网。
func TestToolsSearchDisabledIs403NoUpstream(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32

	handler := newToolsSearchTestHandler(t, false, func(*http.Request) (*http.Response, error) {
		calls.Add(1)

		return jsonResponse(http.StatusOK, searchOKBody), nil
	})

	rec := postToolsSearch(t, handler, `{"query":"golang","count":5}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("code=%d want 403，body=%s", rec.Code, rec.Body)
	}

	if calls.Load() != 0 {
		t.Fatalf("开关关闭时不得打上游，calls=%d", calls.Load())
	}
}

// TestToolsSearchRejectsNonCNAPIKey 命名密钥绑定 global：搜索只走 CN 账号，入口 400
// 且不消耗 CN 额度（与 /v1/search/messages、/v1/tools/fetch 同口径）。
func TestToolsSearchRejectsNonCNAPIKey(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32

	handler := newToolsSearchTestHandler(t, true, func(*http.Request) (*http.Response, error) {
		calls.Add(1)

		return jsonResponse(http.StatusOK, searchOKBody), nil
	})

	req := httptest.NewRequest(http.MethodPost, toolsSearchPath, strings.NewReader(`{"query":"golang"}`))
	req = req.WithContext(context.WithValue(req.Context(), keyUsageKey{}, &keyUsage{id: "k1", realm: "global"}))
	rec := httptest.NewRecorder()

	handler.toolsSearchEndpointHandler(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d want 400，body=%s", rec.Code, rec.Body)
	}

	if calls.Load() != 0 {
		t.Fatalf("calls=%d want 0", calls.Load())
	}
}

// TestToolsSearchReturnsDeepSeekShape 正常结果：十个契约键齐全，缺失值补空串 /
// null / 契约默认（rerank_score=0、authority=-1），正文恒空串。
//
//nolint:cyclop // 逐键断言契约字段。
func TestToolsSearchReturnsDeepSeekShape(t *testing.T) {
	t.Parallel()

	var gotPath string

	var gotBody []byte

	handler := newToolsSearchTestHandler(t, true, func(r *http.Request) (*http.Response, error) {
		gotPath = r.URL.Path
		gotBody, _ = io.ReadAll(r.Body)

		return jsonResponse(http.StatusOK, `{"query":"golang","results":[
			{"title":"Go","url":"https://go.dev","snippet":"lang"},
			{"url":"https://tip.golang.org"}
		],"total_results":2}`), nil
	})

	rec := postToolsSearch(t, handler, `{"query":"golang"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}

	if gotPath != "/agenttool/v1/search" {
		t.Fatalf("upstream path=%q", gotPath)
	}

	var outbound struct {
		Query      string `json:"query"`
		MaxResults int    `json:"max_results"`
	}
	if err := jsonx.Unmarshal(gotBody, &outbound); err != nil {
		t.Fatalf("upstream body=%s err=%v", gotBody, err)
	}

	// count 缺省 = 10（DeepSeek 默认值；上游按 ClampSearchMaxResults 收敛到上限 10）。
	if outbound.Query != "golang" || outbound.MaxResults != 10 {
		t.Fatalf("upstream body=%s", gotBody)
	}

	var resp toolsSearchDecoded
	if err := jsonx.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("body=%s err=%v", rec.Body, err)
	}

	if len(resp.Results) != 2 {
		t.Fatalf("results=%v", resp.Results)
	}

	for _, result := range resp.Results {
		assertToolsSearchResultKeys(t, result)
	}

	first := resp.Results[0]
	if first["title"] != "Go" || first["url"] != "https://go.dev" || first["snippet"] != "lang" {
		t.Fatalf("first=%v", first)
	}

	if first["summary"] != "" || first["content"] != "" {
		t.Fatalf("summary/content 缺失必须补空串：%v", first)
	}

	if first["site_name"] != nil || first["site_icon"] != nil || first["date"] != nil {
		t.Fatalf("上游无对应能力时 site_name/site_icon/date 必须为 null：%v", first)
	}

	if first["rerank_score"] != float64(0) || first["authority"] != float64(-1) {
		t.Fatalf("契约默认 rerank_score=0 / authority=-1：%v", first)
	}

	// 缺 title 的条目不拿 URL 顶替（DeepSeek 契约要求字符串，空串即缺失原样）。
	if second := resp.Results[1]; second["title"] != "" || second["url"] != "https://tip.golang.org" {
		t.Fatalf("second=%v", second)
	}
}

// TestToolsSearchEmptyResults 空结果是 {"results":[]}，不是 null。
func TestToolsSearchEmptyResults(t *testing.T) {
	t.Parallel()

	handler := newToolsSearchTestHandler(t, true, func(*http.Request) (*http.Response, error) {
		return jsonResponse(http.StatusOK, `{"query":"nope","results":[],"total_results":0}`), nil
	})

	rec := postToolsSearch(t, handler, `{"query":"nope"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}

	if got := strings.TrimSpace(rec.Body.String()); got != `{"results":[]}` {
		t.Fatalf("body=%s want {\"results\":[]}", got)
	}
}

// TestToolsSearchRejectsInvalidQuery query 必填且 1..128 字符：非法一律 400 且不出网。
func TestToolsSearchRejectsInvalidQuery(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32

	handler := newToolsSearchTestHandler(t, true, func(*http.Request) (*http.Response, error) {
		calls.Add(1)

		return jsonResponse(http.StatusOK, searchOKBody), nil
	})

	// 按字符（rune）计上限：129 个汉字同样是超长。
	tooLong := strings.Repeat("查", 129)

	for _, body := range []string{
		`{}`,
		`{"query":""}`,
		`{"query":"   "}`,
		`{"query":"a"}`,
		`{"query":` + strconvQuote(tooLong) + `}`,
	} {
		rec := postToolsSearch(t, handler, body)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("body=%s code=%d want 400，resp=%s", body, rec.Code, rec.Body)
		}

		if !strings.Contains(rec.Body.String(), `"error"`) {
			t.Fatalf("错误形状应为 OpenAI 信封：%s", rec.Body)
		}
	}

	// 边界值可接受：2 字符（上游下限）与 128 字符（契约上限）。
	for _, body := range []string{
		`{"query":"ab"}`,
		`{"query":` + strconvQuote(strings.Repeat("查", 128)) + `}`,
	} {
		if rec := postToolsSearch(t, handler, body); rec.Code != http.StatusOK {
			t.Fatalf("body=%s code=%d want 200，resp=%s", body, rec.Code, rec.Body)
		}
	}

	if calls.Load() != 2 {
		t.Fatalf("calls=%d want 2（只有边界合法值出网）", calls.Load())
	}
}

// TestToolsSearchCountRange count 1..50，缺省 10；越界 400 且不出网。
func TestToolsSearchCountRange(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32

	var gotBody []byte

	handler := newToolsSearchTestHandler(t, true, func(r *http.Request) (*http.Response, error) {
		calls.Add(1)

		gotBody, _ = io.ReadAll(r.Body)

		return jsonResponse(http.StatusOK, searchOKBody), nil
	})

	for _, body := range []string{
		`{"query":"golang","count":0}`,
		`{"query":"golang","count":-1}`,
		`{"query":"golang","count":51}`,
	} {
		rec := postToolsSearch(t, handler, body)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("body=%s code=%d want 400，resp=%s", body, rec.Code, rec.Body)
		}
	}

	if calls.Load() != 0 {
		t.Fatalf("越界 count 不得出网，calls=%d", calls.Load())
	}

	if rec := postToolsSearch(t, handler, `{"query":"golang","count":5}`); rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}

	var outbound struct {
		MaxResults int `json:"max_results"`
	}
	if err := jsonx.Unmarshal(gotBody, &outbound); err != nil {
		t.Fatalf("upstream body=%s err=%v", gotBody, err)
	}

	if outbound.MaxResults != 5 {
		t.Fatalf("max_results=%d want 5", outbound.MaxResults)
	}
}

// TestToolsSearchNeedContentDoesNotFabricate need_content=true 不报错、不伪造正文：
// content 恒空串（要正文走既有 deep 语义 /v1/search/messages 的 deep=true）。
func TestToolsSearchNeedContentDoesNotFabricate(t *testing.T) {
	t.Parallel()

	handler := newToolsSearchTestHandler(t, true, func(*http.Request) (*http.Response, error) {
		return jsonResponse(http.StatusOK, `{"query":"golang","results":[
			{"title":"Go","url":"https://go.dev","snippet":"lang"}
		],"total_results":1}`), nil
	})

	rec := postToolsSearch(t, handler,
		`{"query":"golang","count":3,"need_content":true,"ori_question":"原始问题","localization":"zh"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}

	var resp toolsSearchDecoded
	if err := jsonx.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("body=%s err=%v", rec.Body, err)
	}

	if len(resp.Results) != 1 {
		t.Fatalf("results=%v", resp.Results)
	}

	assertToolsSearchResultKeys(t, resp.Results[0])

	if resp.Results[0]["content"] != "" {
		t.Fatalf("need_content=true 不得拿 snippet 冒充正文：%v", resp.Results[0])
	}
}

// TestToolsSearchUpstreamFailureStatus 上游可换号类失败按既有非对话端点处置：
// 单账号 → 轮转耗尽后把最后一次的类别状态码透给客户端。
func TestToolsSearchUpstreamFailureStatus(t *testing.T) {
	t.Parallel()

	handler := newToolsSearchTestHandler(t, true, func(*http.Request) (*http.Response, error) {
		return jsonResponse(http.StatusTooManyRequests, `{"code":429,"msg":"rate limited"}`), nil
	})

	rec := postToolsSearch(t, handler, `{"query":"golang"}`)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("code=%d want 429，body=%s", rec.Code, rec.Body)
	}
}

// TestToolsSearchRotatesUntilExhausted 可换号失败按 MaxRotate 有界轮转，
// 全部失败后回 503（而不是把 5xx 当 200 或透出上游原文的空状态）。
func TestToolsSearchRotatesUntilExhausted(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32

	handler := NewHandler(&Config{
		Pool: testPoolWith(
			&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999},
			&auth.Auth{UID: "u2", AccessToken: "at2", ExpiresAt: 9999999999},
		),
		Upstream: &upstream.Client{
			HTTP: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				calls.Add(1)

				return jsonResponse(http.StatusInternalServerError, `{"code":500,"msg":"boom"}`), nil
			})},
			ChatBaseCN:    "https://fake.example",
			BillingBaseCN: "https://fake.example",
		},
		Live:      livecfg.New(livecfg.Snapshot{SearchEndpoint: true}),
		MaxRotate: 3,
	})

	rec := postToolsSearch(t, handler, `{"query":"golang"}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code=%d want 503，body=%s", rec.Code, rec.Body)
	}

	if got := calls.Load(); got != 2 {
		t.Fatalf("calls=%d want 2（每个账号各试一次）", got)
	}
}

// TestToolsSearchRequestLevelFailFast 请求级失败（上游业务码 15003）不换号：
// 同一请求换号必然同样失败，白烧额度只会放大上游请求量。
func TestToolsSearchRequestLevelFailFast(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32

	handler := NewHandler(&Config{
		Pool: testPoolWith(
			&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999},
			&auth.Auth{UID: "u2", AccessToken: "at2", ExpiresAt: 9999999999},
		),
		Upstream: &upstream.Client{
			HTTP: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				calls.Add(1)

				return jsonResponse(http.StatusBadRequest,
					`{"code":15003,"msg":"unsupported search type"}`), nil
			})},
			ChatBaseCN:    "https://fake.example",
			BillingBaseCN: "https://fake.example",
		},
		Live:      livecfg.New(livecfg.Snapshot{SearchEndpoint: true}),
		MaxRotate: 3,
	})

	rec := postToolsSearch(t, handler, `{"query":"golang"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d want 400，body=%s", rec.Code, rec.Body)
	}

	if got := calls.Load(); got != 1 {
		t.Fatalf("calls=%d want 1（请求级失败不换号）", got)
	}
}

// TestToolsSearchNoHealthyAccountIs503 池里没有可服务账号 → 503（不是 502）。
func TestToolsSearchNoHealthyAccountIs503(t *testing.T) {
	t.Parallel()

	handler := NewHandler(&Config{
		Pool: testPoolWith(),
		Upstream: &upstream.Client{
			HTTP: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				return nil, context.Canceled
			})},
			ChatBaseCN:    "https://fake.example",
			BillingBaseCN: "https://fake.example",
		},
		Live: livecfg.New(livecfg.Snapshot{SearchEndpoint: true}),
	})

	rec := postToolsSearch(t, handler, `{"query":"golang"}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code=%d want 503，body=%s", rec.Code, rec.Body)
	}
}

// TestToolsSearchQueryMaxLenIs128 上半部分测试用的常量自检：确保限制按 rune 计。
func TestToolsSearchQueryMaxLenIs128(t *testing.T) {
	t.Parallel()

	if got := utf8.RuneCountInString(strings.Repeat("查", toolsSearchQueryMaxLen)); got != 128 {
		t.Fatalf("rune count=%d want 128", got)
	}
}
