package upstream

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
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

	out, err := client.Search(context.Background(), &auth.Auth{AccessToken: "at", UID: "u1"}, "golang", 0, ChatMeta{})
	if err != nil {
		t.Fatalf("search: %v", err)
	}

	assertSearchRequestShape(t, gotPath, gotBody, gotAccept, gotContentType, gotIntent, gotRuntime)

	if len(out.Results) != 1 || out.Results[0].URL != "https://go.dev" || out.TotalResults != 1 {
		t.Errorf("outcome=%+v", out)
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

	_, err := client.Search(context.Background(), &auth.Auth{AccessToken: "at", UID: "u1"}, " a ", 0, ChatMeta{})
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

	_, err := client.Search(context.Background(), &auth.Auth{AccessToken: "at", UID: "u1"}, "golang", 0, ChatMeta{})
	if err == nil {
		t.Fatal("want error")
	}

	var upErr *Error
	if !asError(err, &upErr) || upErr.Kind == ErrNone {
		t.Fatalf("err=%v want 带 Kind 的业务错误", err)
	}
}

// TestSearchHTTPErrorClassified 非 2xx 按既有 Classify 分类（不引入第二套判定）。
func TestSearchHTTPErrorClassified(t *testing.T) {
	t.Parallel()

	client := testClient(func(*http.Request) (*http.Response, error) {
		return jsonResp(http.StatusTooManyRequests, `{"code":429,"msg":"rate limited"}`), nil
	})

	_, err := client.Search(context.Background(), &auth.Auth{AccessToken: "at", UID: "u1"}, "golang", 0, ChatMeta{})

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

	out, err := client.Search(context.Background(), &auth.Auth{AccessToken: "at", UID: "u1"}, "golang", 0, ChatMeta{})
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
