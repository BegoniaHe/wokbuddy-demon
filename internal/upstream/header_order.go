// header_order.go 在 TLS 之上重写 CLI chat 的明文请求头条目顺序与大小写。
//
// Go 的 net/http 固定先写 Host/User-Agent/Content-Length，再按字典序写其余头，
// 最后由 Transport 追加 Accept-Encoding；官方 Node 客户端按 SDK Headers 字典序 +
// 插入序 + 固定尾部写出。Header.Set 的调用顺序无法改变线上字节，所以在连接层
// 拦截并重排整个头块（#457）。
//
// 作用范围刻意最小：只有 `POST /v2/chat/completions` 且头块带 `x-stainless-lang`
// 标记的 CLI chat 请求会被重写；其他请求原样透传。解析失败一律回写原始字节并
// 关闭本连接的后续重写（fail-open，宁可不重排也不能破坏请求）。
package upstream

import (
	"bytes"
	"net"
	"strings"
)

// chatRequestLine 官方 CLI chat 的请求行前缀；同时作为「该连接上的下一个请求是
// CLI chat」的识别标记。
const chatRequestLine = "POST /v2/chat/completions HTTP/1.1\r\n"

// chatHeaderBlockLimit 单个头块的上限。超过即视为解析失败（fail-open），避免
// 异常连接把内存拖大。
const chatHeaderBlockLimit = 64 << 10

// chatHeaderOrder 官方 2026-11-04 抓包的 42 头中，出现在 Content-Length 之前的
// 40 个头，按线上顺序排列。大小写逐字抄自抓包（X-Conversation-ID /
// X-Request-ID / acp-connection-id / traceparent 等）。
var chatHeaderOrder = []string{
	"Accept",
	"Content-Type",
	"X-Requested-With",
	"x-stainless-arch",
	"x-stainless-lang",
	"x-stainless-os",
	"x-stainless-package-version",
	"x-stainless-retry-count",
	"x-stainless-runtime",
	"x-stainless-runtime-version",
	"X-Conversation-ID",
	"X-Agent-Purpose",
	"X-IDE-Type",
	"X-IDE-Name",
	"X-IDE-Version",
	"X-Agent-Intent",
	"X-Agent-Runtime",
	"X-Private-Data",
	"acp-connection-id",
	"x-codebuddy-request",
	"X-Device-Token",
	"X-Request-ID",
	"X-Conversation-Message-ID",
	"X-Conversation-Request-ID",
	"X-Root-Request-ID",
	"X-Agent-Type",
	"baggage",
	"Content-Encoding",
	"traceparent",
	"b3",
	"X-B3-TraceId",
	"X-B3-ParentSpanId",
	"X-B3-SpanId",
	"X-B3-Sampled",
	"X-Trace-ID",
	"X-Product",
	"User-Agent",
	"Authorization",
	"X-User-Id",
	"X-Domain",
}

// chatHeaderOptionalOrder 抓包未覆盖、但本仓确实会发的 chat 头。官方位置未知，
// 统一放在已捕获 40 头之后、Content-Length 之前，并保持原有相对顺序。
var chatHeaderOptionalOrder = []string{
	"X-Parent-Conversation-ID",
	"X-Compaction",
	"X-Context-Compacted",
	"X-Wb-Uid",
	"X-Expert-Id",
	"X-Enterprise-Id",
	"X-Tenant-Id",
	"X-Auth-Method",
	"X-Id-Source",
	"X-Userinfo",
	"X-Department-Info",
	"X-Forwarded-For",
	"X-Real-IP",
	"X-Client-Ip",
}

// chatHeaderTailOrder Node 在用户头之后补的固定尾部：Content-Length /
// Accept-Encoding 由 axios 写出，Host / Connection 由 Node http 写出。
var chatHeaderTailOrder = []string{
	"Content-Length",
	"Accept-Encoding",
	"Host",
	"Connection",
}

// chatHeaderCasing 小写头名 → 官方线上大小写。未列出的头保持原始大小写。
func chatHeaderCasing() map[string]string {
	casing := make(map[string]string, len(chatHeaderOrder)+len(chatHeaderOptionalOrder)+len(chatHeaderTailOrder))
	for _, name := range chatHeaderOrder {
		casing[strings.ToLower(name)] = name
	}

	for _, name := range chatHeaderOptionalOrder {
		casing[strings.ToLower(name)] = name
	}

	for _, name := range chatHeaderTailOrder {
		casing[strings.ToLower(name)] = name
	}

	return casing
}

// headerLine 头块里的一行：original 保留原始头名，value 为冒号后的值（去前导空格）。
type headerLine struct {
	name  string
	value string
}

// parsedChatHeader 解析后的 CLI chat 头块：requestLine 与按头名归并的原始行。
type parsedChatHeader struct {
	requestLine string
	headers     map[string][]headerLine
	order       []string
}

// parseChatHeaderBlock 解析完整头块（含结尾空行）；ok=false 表示不是 CLI chat
// 或无法解析，调用方必须回写原始字节。
func parseChatHeaderBlock(block []byte) (parsedChatHeader, bool) {
	if !bytes.HasPrefix(block, []byte(chatRequestLine)) || !bytes.HasSuffix(block, []byte("\r\n\r\n")) {
		return parsedChatHeader{}, false
	}

	lines := strings.Split(strings.TrimSuffix(string(block), "\r\n\r\n"), "\r\n")
	if len(lines) == 0 {
		return parsedChatHeader{}, false
	}

	parsed := parsedChatHeader{
		requestLine: lines[0],
		headers:     make(map[string][]headerLine, len(lines)-1),
		order:       make([]string, 0, len(lines)-1),
	}

	for _, line := range lines[1:] {
		name, value, ok := strings.Cut(line, ":")
		if !ok || name == "" {
			return parsedChatHeader{}, false
		}

		key := strings.ToLower(name)
		if _, seen := parsed.headers[key]; !seen {
			parsed.order = append(parsed.order, key)
		}

		line := headerLine{name: name, value: strings.TrimPrefix(value, " ")}
		parsed.headers[key] = append(parsed.headers[key], line)
	}

	if len(parsed.headers["x-stainless-lang"]) == 0 {
		return parsedChatHeader{}, false
	}

	return parsed, true
}

// writeHeaderGroup 按官方大小写写出一个头名的全部行；同一头名多行时保持原顺序。
func writeHeaderGroup(
	out *strings.Builder, key string, headers map[string][]headerLine,
	casing map[string]string, emitted map[string]bool,
) {
	if emitted[key] || len(headers[key]) == 0 {
		return
	}

	emitted[key] = true

	name := casing[key]
	if name == "" {
		name = headers[key][0].name
	}

	for _, line := range headers[key] {
		out.WriteString(name)
		out.WriteString(": ")
		out.WriteString(line.value)
		out.WriteString("\r\n")
	}
}

func writeHeaderGroups(
	out *strings.Builder, parsed parsedChatHeader, casing map[string]string,
	names []string, emitted map[string]bool,
) {
	for _, name := range names {
		writeHeaderGroup(out, strings.ToLower(name), parsed.headers, casing, emitted)
	}
}

// rewriteChatHeaderBlock 重排一个完整头块（含结尾空行）。ok=false 表示该块不是
// CLI chat 或无法解析，调用方必须回写原始字节。
func rewriteChatHeaderBlock(block []byte) ([]byte, bool) {
	parsed, ok := parseChatHeaderBlock(block)
	if !ok {
		return nil, false
	}

	casing := chatHeaderCasing()
	known := chatHeaderKeySet()
	emitted := make(map[string]bool, len(parsed.headers))

	var out strings.Builder
	out.Grow(len(block) + 64)
	out.WriteString(parsed.requestLine)
	out.WriteString("\r\n")

	writeHeaderGroups(&out, parsed, casing, chatHeaderOrder, emitted)
	writeHeaderGroups(&out, parsed, casing, chatHeaderOptionalOrder, emitted)

	// 未在已知清单里的头（例如自定义透传头）放在尾部之前，保持 Go 写出时
	// 的原始相对顺序。
	for _, key := range parsed.order {
		if known[key] || isTailHeader(key) {
			continue
		}

		writeHeaderGroup(&out, key, parsed.headers, casing, emitted)
	}

	writeHeaderGroups(&out, parsed, casing, chatHeaderTailOrder, emitted)

	// 官方 keep-alive 连接带 `Connection: keep-alive`；Go 在 HTTP/1.1 复用连接上
	// 不发该头。只有原请求没有显式 Connection（如 close）时才补。
	if !emitted["connection"] {
		out.WriteString("Connection: keep-alive\r\n")
	}

	out.WriteString("\r\n")

	// 必须至少包含原始块里的全部头；否则说明解析漏项，按失败处理。
	if len(emitted) < len(parsed.headers) {
		return nil, false
	}

	return []byte(out.String()), true
}

// chatHeaderKeySet 已捕获 + 本仓可选的头名（小写）；尾部四头由 isTailHeader 处理。
func chatHeaderKeySet() map[string]bool {
	casing := chatHeaderCasing()

	known := make(map[string]bool, len(casing))
	for key := range casing {
		known[key] = true
	}

	return known
}

func isTailHeader(key string) bool {
	for _, name := range chatHeaderTailOrder {
		if strings.EqualFold(name, key) {
			return true
		}
	}

	return false
}

// headerOrderConn 在 TLS 连接上重写 CLI chat 头块。http.Transport 的写循环单协程
// 调用 Write，故状态机无需加锁。
type headerOrderConn struct {
	net.Conn

	buf      []byte
	buffered bool
	disabled bool
}

// Write 缓冲/重写头块，body 与后续请求原样透传。
func (c *headerOrderConn) Write(payload []byte) (int, error) {
	if c.disabled || len(payload) == 0 {
		return c.Conn.Write(payload)
	}

	if !c.buffered {
		// 只在一次写入的起点识别新请求：Transport 在发完上一个请求前不会写下一个，
		// body 的首字节是 JSON 的 '{'（或 chunked 长度行），不会命中该前缀。
		if !bytes.HasPrefix(payload, []byte(chatRequestLine)) {
			return c.Conn.Write(payload)
		}

		c.buf = append(c.buf[:0], payload...)
		c.buffered = true
	} else {
		c.buf = append(c.buf, payload...)
	}

	end := bytes.Index(c.buf, []byte("\r\n\r\n"))
	if end < 0 {
		return c.bufferIncomplete(payload)
	}

	return c.flushHeaderBlock(payload, end)
}

// bufferIncomplete 头块尚未写完：继续缓冲；超过上限时回写已缓冲字节并停用重写。
func (c *headerOrderConn) bufferIncomplete(payload []byte) (int, error) {
	if len(c.buf) <= chatHeaderBlockLimit {
		return len(payload), nil
	}

	err := writeAll(c.Conn, c.buf)

	c.buf = nil
	c.buffered = false
	c.disabled = true

	if err != nil {
		return 0, err
	}

	return len(payload), nil
}

// flushHeaderBlock 头块已完整：重写后写出，并把同一次 Write 里的 body 原样跟上。
func (c *headerOrderConn) flushHeaderBlock(payload []byte, end int) (int, error) {
	block := c.buf[:end+4]
	rest := c.buf[end+4:]

	out := block
	if rewritten, ok := rewriteChatHeaderBlock(block); ok {
		out = rewritten
	} else {
		c.disabled = true
	}

	c.buf = nil
	c.buffered = false

	if err := writeAll(c.Conn, out); err != nil {
		return 0, err
	}

	if len(rest) > 0 {
		if err := writeAll(c.Conn, rest); err != nil {
			return 0, err
		}
	}

	return len(payload), nil
}

// writeAll 完整写出所有字节（net.Conn.Write 允许短写）。
func writeAll(conn net.Conn, data []byte) error {
	for len(data) > 0 {
		written, err := conn.Write(data)
		if err != nil {
			return err
		}

		data = data[written:]
	}

	return nil
}
