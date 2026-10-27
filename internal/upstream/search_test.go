package upstream

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
)

// TestSearchRequestShape 请求形状：路径、方法、字段顺序、必要头。
func TestSearchRequestShape(t *testing.T) {
	t.Parallel()

	var gotBody string

	var gotPath, gotAccept, gotContentType, gotIntent, gotRuntime string

	client := testClient(func(r *http.Request) (*http.Response, error) {
		gotPath = r.URL.Path
		gotAccept = r.Header.Get("Accept")
		gotContentType = r.Header.Get("Content-Type")
		gotIntent = r.Header.Get("X-Agent-Intent")
		gotRuntime = r.Header.Get("X-Agent-Runtime")
		raw, _ := io.ReadAll(r.Body)
		gotBody = string(raw)

		return jsonResp(200, `{"query":"golang","type":"text2text","provider":"bing","results":[`+
			`{"title":"Go","url":"https://go.dev","snippet":"lang"}],"total_results":1}`), nil
	})

	out, err := client.Search(context.Background(), &auth.Auth{AccessToken: "at", UID: "u1"}, "golang", SearchOptions{}, ChatMeta{})
	if err != nil {
		t.Fatalf("search: %v", err)
	}

	assertSearchRequestShape(t, gotPath, gotBody, gotAccept, gotContentType, gotIntent, gotRuntime)

	if len(out.Results) != 1 || out.Results[0].URL != "https://go.dev" || out.TotalResults != 1 {
		t.Errorf("outcome=%+v", out)
	}
}

// TestSearchUsesNormalizedChatBase chat base 允许 host / host/v2 /
// host/v2/chat/completions 三种配置形态（与 chat 同口径）：搜索端点必须先归一，
// 否则会拼出 /v2/agenttool/v1/search 之类的错误路径。
func TestSearchUsesNormalizedChatBase(t *testing.T) {
	t.Parallel()

	for _, base := range []string{
		"https://srv.example",
		"https://srv.example/v2",
		"https://srv.example/v2/chat/completions",
	} {
		var gotHost, gotPath string

		client := testClient(func(r *http.Request) (*http.Response, error) {
			gotHost, gotPath = r.URL.Host, r.URL.Path

			return jsonResp(200, `{"query":"q","results":[]}`), nil
		})
		client.ChatBaseCN = base

		if _, err := client.Search(context.Background(), &auth.Auth{AccessToken: "at", UID: "u1"}, "golang", SearchOptions{}, ChatMeta{}); err != nil {
			t.Fatalf("base=%q search: %v", base, err)
		}

		if gotHost != "srv.example" || gotPath != searchPath {
			t.Errorf("base=%q -> host=%q path=%q want srv.example %s", base, gotHost, gotPath, searchPath)
		}
	}
}

// assertSearchRequestShape 断言路径 / body 形状 / 三个覆盖头。
func assertSearchRequestShape(t *testing.T, path, body, accept, contentType, intent, runtime string) {
	t.Helper()

	if path != "/agenttool/v1/search" {
		t.Errorf("path=%s want /agenttool/v1/search", path)
	}

	if body != `{"query":"golang","type":"text2text","max_results":5}` {
		t.Errorf("body=%s want 默认 5 条且字段顺序 query/type/max_results", body)
	}

	if accept != "application/json" || !strings.Contains(contentType, "charset=UTF-8") {
		t.Errorf("accept=%q content-type=%q", accept, contentType)
	}

	if intent != "craft" || runtime != "other" {
		t.Errorf("agent intent/runtime=%q/%q want craft/other", intent, runtime)
	}
}

// TestSearchHeadersReuseChatFingerprint 防回归：search 仍复用 chat 头族。
// 这是刻意取舍（同一账号避免多客户端指纹），不是遗漏；改动前先看 UPSTREAM.md §六。
func TestSearchHeadersReuseChatFingerprint(t *testing.T) {
	t.Parallel()

	client := newProfileClient(t)
	req := httptest.NewRequest(http.MethodPost, "https://upstream.test/agenttool/v1/search", nil)
	client.SearchHeaders(req, &auth.Auth{AccessToken: "at", UID: "u1"}, "", ChatMeta{})

	for _, name := range []string{"X-Agent-Intent", "X-Private-Data", "X-Agent-Runtime"} {
		if got := req.Header.Get(name); got == "" {
			t.Errorf("search 缺少 chat 头族代表字段 %s（刻意统一指纹，勿当遗漏删除）", name)
		}
	}

	if got := req.Header.Get("Accept"); got != "application/json" {
		t.Errorf("Accept=%q want application/json", got)
	}

	if got := req.Header.Get("Content-Type"); !strings.Contains(got, "charset=UTF-8") {
		t.Errorf("Content-Type=%q want charset=UTF-8", got)
	}
}

// TestSearchMaxResultsClamp 条数收敛：<=0 回落默认、超上限截断。
func TestSearchMaxResultsClamp(t *testing.T) {
	t.Parallel()

	cases := map[int]int{0: SearchDefaultMaxResults, -3: SearchDefaultMaxResults, 3: 3, 99: SearchMaxResultsCap}
	for in, want := range cases {
		if got := ClampSearchMaxResults(in); got != want {
			t.Errorf("ClampSearchMaxResults(%d)=%d want %d", in, got, want)
		}
	}
}

// TestSearchShortQueryRejected 过短 query 在本地即拒绝（不打上游）。
func TestSearchShortQueryRejected(t *testing.T) {
	t.Parallel()

	calls := 0
	client := testClient(func(*http.Request) (*http.Response, error) {
		calls++

		return jsonResp(200, `{}`), nil
	})

	_, err := client.Search(context.Background(), &auth.Auth{AccessToken: "at", UID: "u1"}, " a ", SearchOptions{}, ChatMeta{})
	if err == nil {
		t.Fatal("want error for short query")
	}

	var upErr *Error
	if !asError(err, &upErr) || upErr.Kind != ErrClient {
		t.Fatalf("err=%v want *Error{Kind:ErrClient}", err)
	}

	if calls != 0 {
		t.Errorf("过短 query 不应打上游，calls=%d", calls)
	}
}

// TestSearchBusinessCodeError 2xx + code != 0 归业务错误（ErrClient 兜底）。
func TestSearchBusinessCodeError(t *testing.T) {
	t.Parallel()

	client := testClient(func(*http.Request) (*http.Response, error) {
		return jsonResp(200, `{"code":15003,"msg":"unsupported search type"}`), nil
	})

	_, err := client.Search(context.Background(), &auth.Auth{AccessToken: "at", UID: "u1"}, "golang", SearchOptions{}, ChatMeta{})
	if err == nil {
		t.Fatal("want error")
	}

	var upErr *Error
	if !asError(err, &upErr) || upErr.Kind == ErrNone {
		t.Fatalf("err=%v want 带 Kind 的业务错误", err)
	}
}

// TestSearchBusinessCodeClassifiedFromBody 2xx + 业务码必须按完整 body 分类：
// 15001（搜索额度耗尽）只在 code 字段里，只传 msg 会误降级成 ErrClient，
// 让账号逃过软限流、把额度耗尽的号反复选中。
func TestSearchBusinessCodeClassifiedFromBody(t *testing.T) {
	t.Parallel()

	client := testClient(func(*http.Request) (*http.Response, error) {
		return jsonResp(200, `{"code":15001,"msg":"search quota exhausted"}`), nil
	})

	_, err := client.Search(context.Background(), &auth.Auth{AccessToken: "at", UID: "u1"}, "golang", SearchOptions{}, ChatMeta{})

	var upErr *Error
	if !asError(err, &upErr) || upErr.Kind != ErrSoftRate {
		t.Fatalf("err=%v want ErrSoftRate（15001 账号级额度）", err)
	}
}

// TestSearchHTTPErrorClassified 非 2xx 按既有 Classify 分类（不引入第二套判定）。
func TestSearchHTTPErrorClassified(t *testing.T) {
	t.Parallel()

	client := testClient(func(*http.Request) (*http.Response, error) {
		return jsonResp(http.StatusTooManyRequests, `{"code":429,"msg":"rate limited"}`), nil
	})

	_, err := client.Search(context.Background(), &auth.Auth{AccessToken: "at", UID: "u1"}, "golang", SearchOptions{}, ChatMeta{})

	var upErr *Error
	if !asError(err, &upErr) || upErr.Kind != ErrSoftRate {
		t.Fatalf("err=%v want ErrSoftRate", err)
	}
}

// TestSearchParsesFlatResponse 扁平响应（无 data 包裹）解析正确，results 恒为数组。
func TestSearchParsesFlatResponse(t *testing.T) {
	t.Parallel()

	client := testClient(func(*http.Request) (*http.Response, error) {
		return jsonResp(200, `{"query":"q","results":[{"title":"t","url":"u"},{"title":"t2","url":"u2"}]}`), nil
	})

	out, err := client.Search(context.Background(), &auth.Auth{AccessToken: "at", UID: "u1"}, "golang", SearchOptions{}, ChatMeta{})
	if err != nil {
		t.Fatalf("search: %v", err)
	}

	if len(out.Results) != 2 || out.TotalResults != 2 {
		t.Fatalf("outcome=%+v want 2 条且 total 兜底为 2", out)
	}

	raw, _ := json.Marshal(out)
	if !strings.Contains(string(raw), `"results":[`) {
		t.Errorf("results 必须序列化为数组: %s", raw)
	}
}

// TestSearchRequestOptions 官方可选参数透传：blocked 拼 -site:、allowed/freshness
// 独立字段；空数组不出现字段（issue #278）。
func TestSearchRequestOptions(t *testing.T) {
	t.Parallel()

	var gotBody string

	client := testClient(func(r *http.Request) (*http.Response, error) {
		raw, _ := io.ReadAll(r.Body)
		gotBody = string(raw)

		return jsonResp(200, `{"query":"q","results":[]}`), nil
	})

	_, err := client.Search(context.Background(), &auth.Auth{AccessToken: "at", UID: "u1"}, "golang", SearchOptions{
		MaxResults:     99,
		AllowedDomains: []string{"go.dev"},
		BlockedDomains: []string{"spam.example"},
		Freshness:      "d7",
	}, ChatMeta{})
	if err != nil {
		t.Fatalf("search: %v", err)
	}

	want := `{"query":"golang (-site:spam.example)","type":"text2text","max_results":10,"allowed_domains":["go.dev"],"freshness":"d7"}`
	if gotBody != want {
		t.Fatalf("body=%s want %s", gotBody, want)
	}

	// 空参数与现状一致：不出现 allowed_domains/freshness。
	_, err = client.Search(context.Background(), &auth.Auth{AccessToken: "at", UID: "u1"}, "golang", SearchOptions{}, ChatMeta{})
	if err != nil {
		t.Fatalf("search: %v", err)
	}

	if gotBody != `{"query":"golang","type":"text2text","max_results":5}` {
		t.Fatalf("empty options body=%s", gotBody)
	}
}

// TestSearchOptionsValidation 域名/freshness 合法性与上限。
func TestSearchOptionsValidation(t *testing.T) {
	t.Parallel()

	for _, domain := range []string{"example.com", "a-b.example.com", "sub_domain.example"} {
		if !ValidSearchDomain(domain) {
			t.Errorf("ValidSearchDomain(%q)=false", domain)
		}
	}

	for _, domain := range []string{"", "https://example.com", "example.com/x", "a b.com", "u@example.com", ".example.com", "example.com."} {
		if ValidSearchDomain(domain) {
			t.Errorf("ValidSearchDomain(%q)=true", domain)
		}
	}

	for _, freshness := range []string{"", "d", "d1", "d30", "m", "m12", "y", "y5"} {
		if !ValidSearchFreshness(freshness) {
			t.Errorf("ValidSearchFreshness(%q)=false", freshness)
		}
	}

	for _, freshness := range []string{"d31", "m13", "y6", "0d", "d1x"} {
		if ValidSearchFreshness(freshness) {
			t.Errorf("ValidSearchFreshness(%q)=true", freshness)
		}
	}
}
