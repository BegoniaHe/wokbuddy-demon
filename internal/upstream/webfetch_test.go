package upstream

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
)

// TestWebFetchRequestShape 路径、请求体默认值与响应解析。
func TestWebFetchRequestShape(t *testing.T) {
	t.Parallel()

	var gotPath, gotBody, gotAccept string

	var gotHeaders http.Header

	client := testClient(func(r *http.Request) (*http.Response, error) {
		gotPath = r.URL.Path
		gotAccept = r.Header.Get("Accept")
		gotHeaders = r.Header.Clone()
		raw, _ := io.ReadAll(r.Body)
		gotBody = string(raw)

		return jsonResp(200, `{"code":0,"msg":"OK","url":"https://example.com/a","content_type":"text/markdown","content":"# hi"}`), nil
	})

	out, err := client.WebFetch(context.Background(), &auth.Auth{UID: "u1", AccessToken: "at"}, WebFetchRequest{
		URL: "https://example.com/a", Prompt: "summary",
	}, ChatMeta{ConversationID: "conv-1", ConversationRequestID: "req-1", TraceID: "trace-1"})
	if err != nil {
		t.Fatalf("webfetch: %v", err)
	}

	if gotPath != "/agenttool/v1/webfetch" {
		t.Fatalf("path=%s", gotPath)
	}

	want := `{"url":"https://example.com/a","format":"markdown","prompt":"summary","timeout":20,"max_length":100000}`
	if gotBody != want {
		t.Fatalf("body=%s want %s", gotBody, want)
	}

	if gotAccept != "application/json" {
		t.Fatalf("accept=%q", gotAccept)
	}

	// 与 search / agentic_search 同口径：入站会话头族必须原样到达上游。
	assertHeaderValue(t, gotHeaders, "X-Conversation-Id", "conv-1")
	assertHeaderValue(t, gotHeaders, "X-Conversation-Request-Id", "req-1")
	assertHeaderValue(t, gotHeaders, "X-Trace-Id", "trace-1")
	// 官方 5.9.3 的 webfetch 头构造带 X-Wb-Uid，取值与 X-User-Id 同源（issue #346）。
	assertHeaderValue(t, gotHeaders, "X-Wb-Uid", "u1")

	if out.Content != "# hi" || out.URL != "https://example.com/a" || out.ContentType != "text/markdown" {
		t.Fatalf("outcome=%+v", out)
	}
}

// TestWebFetchTrimsURLFallback 请求 URL 首尾空白在出站前去掉；上游未回传 url 时
// 回落值也必须是干净形态（不能让空白进入客户端响应）。
func TestWebFetchTrimsURLFallback(t *testing.T) {
	t.Parallel()

	var gotBody string

	client := testClient(func(r *http.Request) (*http.Response, error) {
		raw, _ := io.ReadAll(r.Body)
		gotBody = string(raw)

		return jsonResp(200, `{"code":0,"content_type":"text/plain","content":"ok"}`), nil
	})

	out, err := client.WebFetch(context.Background(), &auth.Auth{UID: "u1", AccessToken: "at"}, WebFetchRequest{
		URL: "  https://example.com/a  ",
	}, ChatMeta{})
	if err != nil {
		t.Fatalf("webfetch: %v", err)
	}

	if !strings.Contains(gotBody, `"url":"https://example.com/a"`) {
		t.Fatalf("body=%s", gotBody)
	}

	if out.URL != "https://example.com/a" {
		t.Fatalf("outcome url=%q", out.URL)
	}
}

// TestWebFetchMissingContentFails 缺 content / 非字符串 / 空串都明确失败，不伪造正文。
func TestWebFetchMissingContentFails(t *testing.T) {
	t.Parallel()

	cases := []string{
		`{"code":0,"content_type":"text/html"}`,
		`{"code":0,"content_type":"text/html","content":null}`,
		`{"code":0,"content_type":"text/html","content":123}`,
		`{"code":0,"content_type":"text/html","content":"   "}`,
	}

	for _, body := range cases {
		client := testClient(func(*http.Request) (*http.Response, error) {
			return jsonResp(200, body), nil
		})

		_, err := client.WebFetch(context.Background(), &auth.Auth{UID: "u1", AccessToken: "at"}, WebFetchRequest{URL: "https://example.com"}, ChatMeta{})
		if !errors.Is(err, ErrWebFetchEmptyContent) {
			t.Fatalf("body=%s err=%v want ErrWebFetchEmptyContent", body, err)
		}
	}
}

// TestWebFetchNonTextFails 非文本资源返回明确错误。
func TestWebFetchNonTextFails(t *testing.T) {
	t.Parallel()

	client := testClient(func(*http.Request) (*http.Response, error) {
		return jsonResp(200, `{"code":0,"content_type":"image/png","content":"AAAA"}`), nil
	})

	_, err := client.WebFetch(context.Background(), &auth.Auth{UID: "u1", AccessToken: "at"}, WebFetchRequest{URL: "https://example.com/a.png"}, ChatMeta{})
	if !errors.Is(err, ErrWebFetchNonText) {
		t.Fatalf("err=%v want ErrWebFetchNonText", err)
	}
}

// TestWebFetchBusinessError 200 + code != 0 按 *Error 返回。
func TestWebFetchBusinessError(t *testing.T) {
	t.Parallel()

	client := testClient(func(*http.Request) (*http.Response, error) {
		return jsonResp(200, `{"code":15001,"msg":"quota exhausted"}`), nil
	})

	_, err := client.WebFetch(context.Background(), &auth.Auth{UID: "u1", AccessToken: "at"}, WebFetchRequest{URL: "https://example.com"}, ChatMeta{})

	var upErr *Error
	if !asError(err, &upErr) || upErr.Kind != ErrSoftRate {
		t.Fatalf("err=%v want *Error ErrSoftRate", err)
	}
}

// TestWebFetchAcceptsTextishApplicationTypes 与官方 classifyResourceKind 同口径：
// application/javascript / yaml 等文本形态不能被误判为非文本（只有 image/video/
// audio 与已知二进制类型才拒绝）。
func TestWebFetchAcceptsTextishApplicationTypes(t *testing.T) {
	t.Parallel()

	for _, contentType := range []string{
		"application/javascript", "application/x-yaml", "application/rss+xml",
		"application/ld+json", "text/plain; charset=utf-8",
	} {
		client := testClient(func(*http.Request) (*http.Response, error) {
			return jsonResp(200, `{"code":0,"content_type":"`+contentType+`","content":"ok"}`), nil
		})

		if _, err := client.WebFetch(context.Background(), &auth.Auth{UID: "u1", AccessToken: "at"},
			WebFetchRequest{URL: "https://example.com/a"}, ChatMeta{}); err != nil {
			t.Fatalf("content_type=%s err=%v want text", contentType, err)
		}
	}

	for _, contentType := range []string{"image/png", "video/mp4", "audio/mpeg", "application/pdf"} {
		client := testClient(func(*http.Request) (*http.Response, error) {
			return jsonResp(200, `{"code":0,"content_type":"`+contentType+`","content":"AA"}`), nil
		})

		if _, err := client.WebFetch(context.Background(), &auth.Auth{UID: "u1", AccessToken: "at"},
			WebFetchRequest{URL: "https://example.com/a"}, ChatMeta{}); !errors.Is(err, ErrWebFetchNonText) {
			t.Fatalf("content_type=%s err=%v want ErrWebFetchNonText", contentType, err)
		}
	}
}

// TestWebFetchMalformedResponseSentinel 200 + 非 JSON 必须带哨兵，供 service 层
// 判定为确定性失败（不换号重试）。
func TestWebFetchMalformedResponseSentinel(t *testing.T) {
	t.Parallel()

	client := testClient(func(*http.Request) (*http.Response, error) {
		return jsonResp(200, `<html>bad gateway</html>`), nil
	})

	_, err := client.WebFetch(context.Background(), &auth.Auth{UID: "u1", AccessToken: "at"},
		WebFetchRequest{URL: "https://example.com/a"}, ChatMeta{})
	if !errors.Is(err, ErrWebFetchBadResponse) {
		t.Fatalf("err=%v want ErrWebFetchBadResponse", err)
	}
}

// assertHeaderValue 断言出站请求头等于期望值。
func assertHeaderValue(t *testing.T, header http.Header, name, want string) {
	t.Helper()

	if got := header.Get(name); got != want {
		t.Errorf("%s=%q want %q", name, got, want)
	}
}

// TestWbUidOnlyOnWebFetch X-Wb-Uid 只对 webfetch 发（官方仅 webfetch /
// knowledge_search 发）：search / agentic_search / images 的同源头族都不带，
// 账号 uid 为空时也不注入空头（issue #346）。
func TestWbUidOnlyOnWebFetch(t *testing.T) {
	t.Parallel()

	account := &auth.Auth{UID: "u1", AccessToken: "at"}

	for _, headerCase := range []struct {
		name string
		call func(req *http.Request)
	}{
		{"search", func(req *http.Request) {
			New().applyHeaders(surfaceSearch, req, account, "", ChatMeta{})
		}},
		{"images", func(req *http.Request) { New().ImagesHeaders(req, account, "", ChatMeta{}) }},
	} {
		t.Run(headerCase.name, func(t *testing.T) {
			t.Parallel()

			req, err := http.NewRequest(http.MethodPost, "https://example.invalid/x", http.NoBody)
			if err != nil {
				t.Fatal(err)
			}

			headerCase.call(req)

			if got := req.Header.Get("X-Wb-Uid"); got != "" {
				t.Fatalf("%s X-Wb-Uid=%q want empty", headerCase.name, got)
			}
		})
	}

	t.Run("agentic_search", func(t *testing.T) {
		t.Parallel()

		var got string

		client := testClient(func(r *http.Request) (*http.Response, error) {
			got = r.Header.Get("X-Wb-Uid")

			return jsonResp(200, "event: done\ndata: {\"synthesis\":{\"content\":\"x\"}}\n\n"), nil
		})

		if _, err := client.AgenticSearch(
			context.Background(), account, "研究 go", "internal", ChatMeta{},
		); err != nil {
			t.Fatalf("agentic search: %v", err)
		}

		if got != "" {
			t.Fatalf("agentic_search X-Wb-Uid=%q want empty", got)
		}
	})

	t.Run("webfetch 空 uid 不注入空头", func(t *testing.T) {
		t.Parallel()

		var present bool

		client := testClient(func(r *http.Request) (*http.Response, error) {
			_, present = r.Header["X-Wb-Uid"]

			return jsonResp(200, `{"code":0,"content_type":"text/plain","content":"ok"}`), nil
		})

		if _, err := client.WebFetch(
			context.Background(), &auth.Auth{AccessToken: "at"},
			WebFetchRequest{URL: "https://example.com"}, ChatMeta{},
		); err != nil {
			t.Fatalf("webfetch: %v", err)
		}

		if present {
			t.Fatal("uid 为空时不应注入 X-Wb-Uid")
		}
	})
}
