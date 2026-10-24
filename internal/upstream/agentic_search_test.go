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

// TestAgenticSearchRequestShape 路径/请求体/Accept 对齐官方。
//
//nolint:cyclop // 逐字段校验出站契约，拆开反而难读。
func TestAgenticSearchRequestShape(t *testing.T) {
	t.Parallel()

	var gotPath, gotBody, gotAccept, gotIntent, gotConvID, gotConvReqID, gotTraceID string

	client := testClient(func(r *http.Request) (*http.Response, error) {
		gotPath = r.URL.Path
		gotAccept = r.Header.Get("Accept")
		gotIntent = r.Header.Get("X-Agent-Intent")
		gotConvID = r.Header.Get("X-Conversation-Id")
		gotConvReqID = r.Header.Get("X-Conversation-Request-Id")
		gotTraceID = r.Header.Get("X-Trace-Id")
		raw, _ := io.ReadAll(r.Body)
		gotBody = string(raw)

		return jsonResp(200, "event: done\ndata: {\"synthesis\":{\"content\":\"综述\",\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":4,\"total_tokens\":7}}}\n\n"), nil
	})

	meta := ChatMeta{ConversationID: "conv-1", ConversationRequestID: "req-1", TraceID: "trace-1"}

	out, err := client.AgenticSearch(context.Background(), &auth.Auth{UID: "u1", AccessToken: "at"}, "研究 go", "internal", meta)
	if err != nil {
		t.Fatalf("agentic search: %v", err)
	}

	if gotPath != "/agenttool/v1/agentic_search" {
		t.Fatalf("path=%s", gotPath)
	}

	if gotBody != `{"query":"研究 go","search_mode":2,"stream":true,"biz_via":"internal"}` {
		t.Fatalf("body=%s", gotBody)
	}

	if gotAccept != "text/event-stream" || gotIntent != "craft" {
		t.Fatalf("accept=%q intent=%q", gotAccept, gotIntent)
	}

	// 会话头族与 snippet 搜索同口径：入站值必须原样到达上游。
	if gotConvID != "conv-1" || gotConvReqID != "req-1" || gotTraceID != "trace-1" {
		t.Fatalf("conversation headers=%q/%q/%q", gotConvID, gotConvReqID, gotTraceID)
	}

	if out.Synthesis != "综述" || out.Usage == nil || out.Usage.TotalTokens != 7 {
		t.Fatalf("outcome=%+v", out)
	}
}

// TestAgenticSearchLineEndings LF / CRLF / CR 都能解析 done 帧。
func TestAgenticSearchLineEndings(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"LF":   "event: tool_call\ndata: {}\n\nevent: done\ndata: {\"synthesis\":{\"content\":\"ok\"}}\n\n",
		"CRLF": "event: tool_call\r\ndata: {}\r\n\r\nevent: done\r\ndata: {\"synthesis\":{\"content\":\"ok\"}}\r\n\r\n",
		"CR":   "event: tool_call\rdata: {}\r\revent: done\rdata: {\"synthesis\":{\"content\":\"ok\"}}\r\r",
	}

	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			out, err := parseAgenticSSE([]byte(body))
			if err != nil {
				t.Fatalf("parse: %v", err)
			}

			if out.Synthesis != "ok" {
				t.Fatalf("synthesis=%q", out.Synthesis)
			}
		})
	}
}

// TestAgenticSearchNoDoneFails 无 done / 空综述都不是成功。
func TestAgenticSearchNoDoneFails(t *testing.T) {
	t.Parallel()

	for _, body := range []string{
		"",
		"event: tool_result\ndata: {\"status\":\"OK\"}\n\n",
		"event: done\ndata: {\"synthesis\":{\"content\":\"\"}}\n\n",
	} {
		if _, err := parseAgenticSSE([]byte(body)); !errors.Is(err, ErrAgenticSearchNoSynthesis) {
			t.Fatalf("body=%q err=%v want ErrAgenticSearchNoSynthesis", body, err)
		}
	}
}

// TestAgenticSearchBusinessError 200 + JSON 业务错误按 *Error 返回，不当空综述。
func TestAgenticSearchBusinessError(t *testing.T) {
	t.Parallel()

	client := testClient(func(*http.Request) (*http.Response, error) {
		return jsonResp(200, `{"code":15001,"msg":"search quota exhausted"}`), nil
	})

	_, err := client.AgenticSearch(context.Background(), &auth.Auth{UID: "u1", AccessToken: "at"}, "go", "public", ChatMeta{})

	var upErr *Error
	if !asError(err, &upErr) || upErr.Kind != ErrSoftRate {
		t.Fatalf("err=%v want *Error ErrSoftRate", err)
	}

	if strings.Contains(err.Error(), "done") {
		t.Fatalf("business error must not be reported as missing synthesis: %v", err)
	}
}

// TestAgenticSearchShortQueryRejected 空/过短 query 本地拒绝，不打上游。
func TestAgenticSearchShortQueryRejected(t *testing.T) {
	t.Parallel()

	calls := 0
	client := testClient(func(*http.Request) (*http.Response, error) {
		calls++
		return jsonResp(200, "event: done\ndata: {\"synthesis\":{\"content\":\"ok\"}}\n\n"), nil
	})

	_, err := client.AgenticSearch(context.Background(), &auth.Auth{UID: "u1", AccessToken: "at"}, " a ", "public", ChatMeta{})

	var upErr *Error
	if !asError(err, &upErr) || upErr.Kind != ErrClient {
		t.Fatalf("err=%v want *Error ErrClient", err)
	}

	if calls != 0 {
		t.Fatalf("calls=%d want 0", calls)
	}
}
