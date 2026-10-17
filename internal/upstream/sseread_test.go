package upstream

import (
	"bufio"
	"errors"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestReadSSELineNormalLines 正常行语义回归：带 '\n' 的行返回 nil err；
// 末行不带 '\n' 时返回该行 + io.EOF；空 reader 返回 "" + io.EOF。
func TestReadSSELineNormalLines(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		want    string
		wantEOF bool
	}{
		{"完整行", "data: hi\nrest\n", "data: hi\n", false},
		{"末行无换行", "data: hi", "data: hi", true},
		{"空 reader", "", "", true},
		{"CRLF", "data: hi\r\n", "data: hi\r\n", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			br := bufio.NewReaderSize(strings.NewReader(c.raw), 8)
			got, err := readSSELine(br)
			if got != c.want {
				t.Errorf("line=%q want %q", got, c.want)
			}
			if c.wantEOF && !errors.Is(err, io.EOF) {
				t.Errorf("err=%v want EOF", err)
			}
			if !c.wantEOF && err != nil {
				t.Errorf("err=%v want nil", err)
			}
		})
	}
}

// TestReadSSELineTooLong 单行超过 MaxSSELineBytes：返回 ErrSSELineTooLong，
// 且不分配超过上限的内存（实现按 chunk 检查，超限即停）。
func TestReadSSELineTooLong(t *testing.T) {
	// 用一个刚好超过上限的 reader：若实现按 ReadString 语义反复 concat，
	// 会在返回前构造出完整超长字符串；readSSELine 必须提前失败。
	huge := strings.Repeat("x", MaxSSELineBytes+1024)
	br := bufio.NewReaderSize(strings.NewReader(huge), 64*1024)
	_, err := readSSELine(br)
	if !errors.Is(err, ErrSSELineTooLong) {
		t.Fatalf("err=%v want ErrSSELineTooLong", err)
	}
}

// TestReadSSELineExactlyAtLimit 单行恰好等于上限（含 '\n'）不得误报：
// 上限是「超过才失败」，不是「达到就失败」。
func TestReadSSELineExactlyAtLimit(t *testing.T) {
	line := strings.Repeat("x", MaxSSELineBytes-1) + "\n"
	br := bufio.NewReaderSize(strings.NewReader(line), 64*1024)
	got, err := readSSELine(br)
	if err != nil {
		t.Fatalf("err=%v want nil (line length %d == limit)", err, len(line))
	}
	if len(got) != MaxSSELineBytes {
		t.Errorf("len=%d want %d", len(got), MaxSSELineBytes)
	}
}

// TestAggregateLineTooLong 非流式聚合：单行超限返回 ErrSSELineTooLong，
// 不返回任何响应体（不把部分内容当完整回答）。
func TestAggregateLineTooLong(t *testing.T) {
	huge := strings.Repeat("x", MaxSSELineBytes+512)
	raw := "data: " + huge + "\n\n"
	resp, err := Aggregate(strings.NewReader(raw))
	if !errors.Is(err, ErrSSELineTooLong) {
		t.Fatalf("err=%v want ErrSSELineTooLong (resp=%v)", err, resp)
	}
	if resp != nil {
		t.Errorf("resp=%v want nil", resp)
	}
}

// TestAggregateTotalTooLong 非流式聚合：多帧小行累计超过总量上限时返回
// ErrSSEAggregateTooLong，不返回残缺成功响应（issue #113）。
func TestAggregateTotalTooLong(t *testing.T) {
	// 每帧 1 MiB content：单行不超限，累计 33 帧超过 32 MiB。
	const frame = 1 << 20
	var sb strings.Builder
	for range 40 {
		sb.WriteString(`data: {"choices":[{"index":0,"delta":{"content":"`)
		sb.WriteString(strings.Repeat("y", frame))
		sb.WriteString(`"}}]}`)
		sb.WriteString("\n\n")
	}
	sb.WriteString("data: [DONE]\n\n")
	resp, err := Aggregate(strings.NewReader(sb.String()))
	if !errors.Is(err, ErrSSEAggregateTooLong) {
		t.Fatalf("err=%v want aggregate too long (resp!=nil: %v)", err, resp != nil)
	}
	if resp != nil {
		t.Errorf("聚合超限不得返回响应体")
	}
}

// TestStreamLineTooLong 流式：单行超限时写明确 error 帧 + 恰好一个 [DONE]，
// 返回 ErrSSELineTooLong（issue #113）；已写的正常帧不撤回。
func TestStreamLineTooLong(t *testing.T) {
	raw := "data: {\"id\":\"x1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"}}]}\n\n" +
		"data: " + strings.Repeat("z", MaxSSELineBytes+256) + "\n\n"
	rec := httptest.NewRecorder()
	err := StreamHint(rec, strings.NewReader(raw), nil)
	if !errors.Is(err, ErrSSELineTooLong) {
		t.Fatalf("err=%v want ErrSSELineTooLong", err)
	}
	body := rec.Body.String()
	if n := strings.Count(body, "data: [DONE]"); n != 1 {
		t.Errorf("[DONE] count=%d want 1: %q", n, body)
	}
	if !strings.Contains(body, "exceeded size limit") {
		t.Errorf("missing size-limit error frame: %q", body)
	}
	if !strings.Contains(body, "ok") {
		t.Errorf("已写正常帧被撤回: %q", body)
	}
}

// TestAggregateLargeButLegalStream 回归护栏：大量小帧（总量接近上限内）正常
// 聚合成功——上限不误伤合法大模型输出（issue #113 Non-goals）。
func TestAggregateLargeButLegalStream(t *testing.T) {
	// 4000 帧 × 4 KiB ≈ 16 MiB < 32 MiB 上限。
	const frame, n = 4096, 4000
	var sb strings.Builder
	for range n {
		sb.WriteString(`data: {"choices":[{"index":0,"delta":{"content":"`)
		sb.WriteString(strings.Repeat("a", frame))
		sb.WriteString(`"}}]}`)
		sb.WriteString("\n\n")
	}
	sb.WriteString("data: [DONE]\n\n")
	resp, err := Aggregate(strings.NewReader(sb.String()))
	if err != nil {
		t.Fatalf("err=%v want nil (大量小帧是合法形态)", err)
	}
	msg := resp["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	if got := len(msg["content"].(string)); got != frame*n {
		t.Errorf("content len=%d want %d", got, frame*n)
	}
}

// TestStreamLineTooLongKeepalivePath keepalive 读取路径（异步逐行）同样受单行
// 上限保护：超限写明确 error 帧 + [DONE]，返回 ErrSSELineTooLong（issue #113）。
func TestStreamLineTooLongKeepalivePath(t *testing.T) {
	raw := "data: " + strings.Repeat("z", MaxSSELineBytes+256) + "\n\n"
	rec := httptest.NewRecorder()
	err := StreamHint(rec, strings.NewReader(raw), nil, WithKeepaliveInterval(time.Hour))
	if !errors.Is(err, ErrSSELineTooLong) {
		t.Fatalf("err=%v want ErrSSELineTooLong", err)
	}
	body := rec.Body.String()
	if n := strings.Count(body, "data: [DONE]"); n != 1 {
		t.Errorf("[DONE] count=%d want 1", n)
	}
	if !strings.Contains(body, "exceeded size limit") {
		t.Errorf("missing size-limit error frame: %q", body)
	}
}
