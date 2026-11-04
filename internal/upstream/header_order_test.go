package upstream

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
)

// goStyleChatBlock 模拟 Go net/http 写出的 CLI chat 头块：Host/User-Agent/
// Content-Length 前置，其余字典序，Accept-Encoding 收尾。
func goStyleChatBlock() string {
	return "POST /v2/chat/completions HTTP/1.1\r\n" +
		"Host: www.workbuddy.cn\r\n" +
		"User-Agent: WorkBuddy/9.9.9 WorkBuddy/9.9.9 CLI/8.8.8\r\n" +
		"Content-Length: 2\r\n" +
		"Authorization: Bearer at\r\n" +
		"Accept: application/json\r\n" +
		"Accept-Encoding: gzip\r\n" +
		"Baggage: buddyId=,buddyName=\r\n" +
		"Content-Type: application/json\r\n" +
		"Traceparent: 00-aaaa-bbbb-01\r\n" +
		"X-Agent-Intent: craft\r\n" +
		"X-Agent-Purpose: conversation\r\n" +
		"X-Agent-Runtime: local-agent\r\n" +
		"X-Agent-Type: main\r\n" +
		"X-B3-Parentspanid: bbbb\r\n" +
		"X-B3-Sampled: 1\r\n" +
		"X-B3-Spanid: cccc\r\n" +
		"X-B3-Traceid: aaaa\r\n" +
		"X-Codebuddy-Request: 1\r\n" +
		"X-Conversation-Id: conv-id\r\n" +
		"X-Conversation-Message-Id: msg-id\r\n" +
		"X-Conversation-Request-Id: req-id\r\n" +
		"X-Device-Token: dev\r\n" +
		"X-Domain: www.workbuddy.cn\r\n" +
		"X-Forwarded-For: 1.2.3.4\r\n" +
		"X-Ide-Name: WorkBuddy\r\n" +
		"X-Ide-Type: WorkBuddy\r\n" +
		"X-Ide-Version: 9.9.9\r\n" +
		"X-Private-Data: false\r\n" +
		"X-Product: SaaS\r\n" +
		"X-Request-Id: msg-id\r\n" +
		"X-Requested-With: XMLHttpRequest\r\n" +
		"X-Root-Request-Id: req-id\r\n" +
		"X-Trace-Id: aaaa\r\n" +
		"X-User-Id: u1\r\n" +
		"X-Zzz-Custom: v\r\n" +
		"acp-connection-id: acp-id\r\n" +
		"b3: aaaa-cccc-1-bbbb\r\n" +
		"x-stainless-arch: x64\r\n" +
		"x-stainless-lang: js\r\n" +
		"x-stainless-os: Windows\r\n" +
		"x-stainless-package-version: 6.9.9\r\n" +
		"x-stainless-retry-count: 0\r\n" +
		"x-stainless-runtime: node\r\n" +
		"x-stainless-runtime-version: v22.9.9\r\n" +
		"\r\n"
}

// TestRewriteChatHeaderBlockGolden 头块按官方 42 头顺序 + 官方大小写输出：Accept
// 首头、stainless 紧随、身份头后置、尾部 Content-Length/Accept-Encoding/Host/
// Connection，未知头放在尾部之前。
func TestRewriteChatHeaderBlockGolden(t *testing.T) {
	t.Parallel()

	got, ok := rewriteChatHeaderBlock([]byte(goStyleChatBlock()))
	if !ok {
		t.Fatal("CLI chat 头块应可重写")
	}

	want := "POST /v2/chat/completions HTTP/1.1\r\n" +
		"Accept: application/json\r\n" +
		"Content-Type: application/json\r\n" +
		"X-Requested-With: XMLHttpRequest\r\n" +
		"x-stainless-arch: x64\r\n" +
		"x-stainless-lang: js\r\n" +
		"x-stainless-os: Windows\r\n" +
		"x-stainless-package-version: 6.9.9\r\n" +
		"x-stainless-retry-count: 0\r\n" +
		"x-stainless-runtime: node\r\n" +
		"x-stainless-runtime-version: v22.9.9\r\n" +
		"X-Conversation-ID: conv-id\r\n" +
		"X-Agent-Purpose: conversation\r\n" +
		"X-IDE-Type: WorkBuddy\r\n" +
		"X-IDE-Name: WorkBuddy\r\n" +
		"X-IDE-Version: 9.9.9\r\n" +
		"X-Agent-Intent: craft\r\n" +
		"X-Agent-Runtime: local-agent\r\n" +
		"X-Private-Data: false\r\n" +
		"acp-connection-id: acp-id\r\n" +
		"x-codebuddy-request: 1\r\n" +
		"X-Device-Token: dev\r\n" +
		"X-Request-ID: msg-id\r\n" +
		"X-Conversation-Message-ID: msg-id\r\n" +
		"X-Conversation-Request-ID: req-id\r\n" +
		"X-Root-Request-ID: req-id\r\n" +
		"X-Agent-Type: main\r\n" +
		"baggage: buddyId=,buddyName=\r\n" +
		"traceparent: 00-aaaa-bbbb-01\r\n" +
		"b3: aaaa-cccc-1-bbbb\r\n" +
		"X-B3-TraceId: aaaa\r\n" +
		"X-B3-ParentSpanId: bbbb\r\n" +
		"X-B3-SpanId: cccc\r\n" +
		"X-B3-Sampled: 1\r\n" +
		"X-Trace-ID: aaaa\r\n" +
		"X-Product: SaaS\r\n" +
		"User-Agent: WorkBuddy/9.9.9 WorkBuddy/9.9.9 CLI/8.8.8\r\n" +
		"Authorization: Bearer at\r\n" +
		"X-User-Id: u1\r\n" +
		"X-Domain: www.workbuddy.cn\r\n" +
		"X-Forwarded-For: 1.2.3.4\r\n" +
		"X-Zzz-Custom: v\r\n" +
		"Content-Length: 2\r\n" +
		"Accept-Encoding: gzip\r\n" +
		"Host: www.workbuddy.cn\r\n" +
		"Connection: keep-alive\r\n" +
		"\r\n"

	if string(got) != want {
		t.Errorf("重写结果不符\n got=%q\nwant=%q", got, want)
	}
}

// TestRewriteChatHeaderBlockPassThrough 非 CLI chat 头块（不同路径或缺
// x-stainless-lang 标记）必须原样透传。
func TestRewriteChatHeaderBlockPassThrough(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"其他路径": "POST /agenttool/v1/search HTTP/1.1\r\n" +
			"Host: x\r\nx-stainless-lang: js\r\n\r\n",
		"缺标记": "GET /v2/chat/completions HTTP/1.1\r\n" +
			"Host: x\r\nAccept: application/json\r\n\r\n",
	}

	for name, block := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if _, ok := rewriteChatHeaderBlock([]byte(block)); ok {
				t.Fatal("非 CLI chat 头块不应重写")
			}
		})
	}
}

// captureConn 记录写入字节的 net.Conn 替身。
type captureConn struct {
	net.Conn

	written bytes.Buffer
}

func (c *captureConn) Write(p []byte) (int, error) {
	written, err := c.written.Write(p)
	if err != nil {
		return written, fmt.Errorf("buffer write: %w", err)
	}

	return written, nil
}

// TestHeaderOrderConnRewritesEachBlockOnly 一次连接上的两个请求各自重写头块，
// body 与后续请求字节不变；头块跨多次 Write 也能正确拼装。
func TestHeaderOrderConnRewritesEachBlockOnly(t *testing.T) {
	t.Parallel()

	raw := &captureConn{}
	conn := &headerOrderConn{Conn: raw}

	first := goStyleChatBlock() + `{"a":1}`
	second := goStyleChatBlock() + `{"b":2}`

	// 第一个请求：头块与 body 一次写入。
	if _, err := conn.Write([]byte(first)); err != nil {
		t.Fatalf("write first: %v", err)
	}

	// 第二个请求：头块拆成两段写，body 单独写。
	split := len(second) / 2
	if _, err := conn.Write([]byte(second[:split])); err != nil {
		t.Fatalf("write second part1: %v", err)
	}

	if _, err := conn.Write([]byte(second[split:])); err != nil {
		t.Fatalf("write second part2: %v", err)
	}

	got := raw.written.String()
	if count := strings.Count(got, "POST /v2/chat/completions HTTP/1.1\r\nAccept: application/json\r\n"); count != 2 {
		t.Fatalf("重写次数=%d want 2\n%s", count, got)
	}

	if count := strings.Count(got, `{"a":1}`); count != 1 {
		t.Errorf("body1 出现 %d 次", count)
	}

	if count := strings.Count(got, `{"b":2}`); count != 1 {
		t.Errorf("body2 出现 %d 次", count)
	}

	if strings.Count(got, "Connection: keep-alive\r\n") != 2 {
		t.Errorf("每个请求都应有 Connection: keep-alive")
	}
}

// TestHeaderOrderConnPassesNonChat 同一连接上的非 chat 写入原样透传，之后的 chat
// 请求仍可重写。
func TestHeaderOrderConnPassesNonChat(t *testing.T) {
	t.Parallel()

	raw := &captureConn{}
	conn := &headerOrderConn{Conn: raw}

	nonChat := "POST /billing/meter/get-user-resource HTTP/1.1\r\nHost: x\r\n\r\n{}"

	if _, err := conn.Write([]byte(nonChat)); err != nil {
		t.Fatalf("write: %v", err)
	}

	if _, err := conn.Write([]byte(goStyleChatBlock())); err != nil {
		t.Fatalf("write chat: %v", err)
	}

	got := raw.written.String()
	if !strings.HasPrefix(got, nonChat) {
		t.Errorf("非 chat 字节被改写: %q", got[:min(len(got), 40)])
	}

	if !strings.Contains(got, "POST /v2/chat/completions HTTP/1.1\r\nAccept: application/json\r\n") {
		t.Error("后续 chat 请求未重写")
	}
}

// TestHeaderOrderConnFailOpenUnparseable 无法解析的头块必须原样回写，并关闭该连接
// 后续重写（fail-open）。
func TestHeaderOrderConnFailOpenUnparseable(t *testing.T) {
	t.Parallel()

	raw := &captureConn{}
	conn := &headerOrderConn{Conn: raw}

	broken := "POST /v2/chat/completions HTTP/1.1\r\nHost: x\r\nbad-line-without-colon\r\n\r\n"

	if _, err := conn.Write([]byte(broken)); err != nil {
		t.Fatalf("write broken: %v", err)
	}

	if got := raw.written.String(); got != broken {
		t.Fatalf("broken 未原样回写: %q", got)
	}

	// 之后的 CLI chat 写入不再重写。
	if _, err := conn.Write([]byte(goStyleChatBlock())); err != nil {
		t.Fatalf("write chat: %v", err)
	}

	if strings.Contains(raw.written.String(), "Connection: keep-alive") {
		t.Error("解析失败后不应继续重写该连接")
	}
}

// TestRewriteChatHeaderBlockOverLimitFailsOpen 完整但超过上限的头块必须原样透传
// （解析器自己也要守上限，不能只靠跨写缓冲那条路径）。
func TestRewriteChatHeaderBlockOverLimitFailsOpen(t *testing.T) {
	t.Parallel()

	huge := chatRequestLine + "X-Huge: " + strings.Repeat("a", chatHeaderBlockLimit) + "\r\n\r\n"

	if _, ok := rewriteChatHeaderBlock([]byte(huge)); ok {
		t.Fatal("超限头块不应重写")
	}
}

// zeroWriteConn 模拟违反 net.Conn 契约、写成功但返回 0 字节的连接。
type zeroWriteConn struct {
	net.Conn
}

func (zeroWriteConn) Write([]byte) (int, error) { return 0, nil }

// TestWriteAllRejectsZeroWrite 零写不得让 writeAll 无限自旋。
func TestWriteAllRejectsZeroWrite(t *testing.T) {
	t.Parallel()

	if err := writeAll(zeroWriteConn{}, []byte("x")); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("writeAll=%v want io.ErrShortWrite", err)
	}
}

// TestHeaderOrderConnOverLimitFailOpen 头块超过上限时原样回写并关闭重写。
func TestHeaderOrderConnOverLimitFailOpen(t *testing.T) {
	t.Parallel()

	raw := &captureConn{}
	conn := &headerOrderConn{Conn: raw}

	huge := chatRequestLine + "X-Huge: " + strings.Repeat("a", chatHeaderBlockLimit+1)

	if _, err := conn.Write([]byte(huge)); err != nil {
		t.Fatalf("write huge: %v", err)
	}

	if got := raw.written.String(); got != huge {
		t.Fatalf("超限头块未原样回写（len=%d want %d）", len(got), len(huge))
	}

	if !conn.disabled {
		t.Fatal("超限后应禁用该连接的重写")
	}
}

// TestHeaderOrderEndToEnd 真实 TLS 连接上两个 keep-alive chat 请求都被重写，body
// 逐字节不变。
func TestHeaderOrderEndToEnd(t *testing.T) {
	t.Parallel()

	var bodies []string

	addr, cert, rec := startRecordingTLSServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(raw))

		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))

	client := testFingerprintClient(t, addr, cert.Leaf)
	endpoint := "https://" + addr + "/v2/chat/completions"

	doChat(t, client, endpoint, `{"model":"x","messages":[]}`)
	doChat(t, client, endpoint, `{"model":"y","messages":[{"role":"user","content":"hi"}]}`)

	if len(bodies) != 2 {
		t.Fatalf("服务端收到 %d 个请求 want 2（应复用同一连接）", len(bodies))
	}

	got := string(rec.snapshot())
	if count := strings.Count(got, "POST /v2/chat/completions HTTP/1.1\r\nAccept: application/json\r\n"); count != 2 {
		t.Fatalf("重写次数=%d want 2\n---\n%s", count, got)
	}

	for _, body := range bodies {
		if !strings.Contains(got, body) {
			t.Errorf("明文缺少 body %q", body)
		}
	}

	if strings.Contains(got, "Host: "+addr+"\r\nUser-Agent:") {
		t.Error("Host 仍被写在最前面，头序未生效")
	}
}
