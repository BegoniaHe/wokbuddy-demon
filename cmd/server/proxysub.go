// proxysub.go 代理订阅导入（issue #490）：把订阅文本解析成现成的 proxies 列表。
//
// 支持三种输入：
//   - 纯 URL 列表（每行一条，http/https/socks5/socks5h 与四段式 host:port:user:pass）；
//   - base64 包裹的同类列表（常见订阅格式）；
//   - Clash / KM YAML 的 proxies: 段（只取 http/https/socks5 型条目）。
//
// 只做「文本 → []string」，最终校验复用 outbound.ParseProxy；不引入第二套代理模型，
// 也不引入 YAML 依赖——clash 只解析本项目需要的最小子集（items + key: value）。
// 非法条目单独跳过并记原因，不整批失败；订阅拉取/解析失败保留上一次成功结果。
package main

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/BegoniaHe/wokbuddy-demon/internal/outbound"
)

// errProxySubFailed 订阅拉取失败的静态哨兵（err113：错误串里含脱敏 URL 等动态内容）。
var errProxySubFailed = errors.New("proxy subscription failed")

const (
	// proxySubFetchTimeout 单个订阅的拉取上限：启动与配置热应用都会经过这里，
	// 不能因为一个订阅源挂住整条装配路径。
	proxySubFetchTimeout = 10 * time.Second
	// proxySubMaxBytes 订阅文本大小上限（超出按失败处理，避免把内存撑爆）。
	proxySubMaxBytes = 4 << 20
)

// proxySubFetcher 拉取订阅文本（测试注入用）。
var proxySubFetcher = fetchProxySubscriptionText

// fetchProxySubscriptionText 拉取订阅原文。错误信息只用脱敏后的 URL：订阅 URL 常
// 带 token（路径或 query），不能进日志。
func fetchProxySubscriptionText(rawURL string) (string, error) {
	masked := maskProxySubURL(rawURL)

	ctx, cancel := context.WithTimeout(context.Background(), proxySubFetchTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, http.NoBody)
	if err != nil {
		return "", fmt.Errorf("%w: URL 非法: %s", errProxySubFailed, masked)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("%w: 请求失败: %s: %s", errProxySubFailed, masked, maskedHTTPError(err))
	}
	defer resp.Body.Close()

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return "", fmt.Errorf("%w: 返回 %d: %s", errProxySubFailed, resp.StatusCode, masked)
	}

	raw, err := io.ReadAll(io.LimitReader(resp.Body, proxySubMaxBytes+1))
	if err != nil {
		return "", fmt.Errorf("%w: 读取失败: %s", errProxySubFailed, masked)
	}

	if len(raw) > proxySubMaxBytes {
		return "", fmt.Errorf("%w: 超过 %d 字节: %s", errProxySubFailed, proxySubMaxBytes, masked)
	}

	return string(raw), nil
}

// maskedHTTPError 抽出 *url.Error 的底层原因（url.Error 自带含 token 的 URL）。
func maskedHTTPError(err error) string {
	var urlErr *url.Error
	if errors.As(err, &urlErr) && urlErr.Err != nil {
		return urlErr.Err.Error()
	}

	return "请求失败"
}

// maskProxySubURL 订阅 URL 的打码口径：只留 scheme://host，丢掉可能带 token 的
// 路径与 query（与 outbound.Proxy.Masked 同一思路）。
func maskProxySubURL(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "[invalid proxy subscription url]"
	}

	return u.Scheme + "://" + u.Host
}

// maskProxyEntry 单个代理条目的打码口径：同样只留 scheme://host[:port]。
func maskProxyEntry(raw string) string {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Scheme == "" {
		return "[unparsable proxy entry]"
	}

	if parsed.Host == "" {
		return parsed.Scheme + "://"
	}

	return parsed.Scheme + "://" + parsed.Host
}

// proxySubscriptionCache 上一次成功解析结果（按订阅 URL）。刷新失败时沿用它，
// 保证「拉不到 ≠ 清空现有代理」。
var proxySubscriptionCache = struct {
	mu   sync.Mutex
	last map[string][]string
}{last: map[string][]string{}}

func cachedProxySubscription(sub string) []string {
	proxySubscriptionCache.mu.Lock()
	defer proxySubscriptionCache.mu.Unlock()

	return append([]string(nil), proxySubscriptionCache.last[sub]...)
}

func storeProxySubscription(sub string, proxies []string) {
	proxySubscriptionCache.mu.Lock()
	defer proxySubscriptionCache.mu.Unlock()

	proxySubscriptionCache.last[sub] = append([]string(nil), proxies...)
}

// resolveProxySubscriptions 展开订阅为代理 URL 列表并合并。单个订阅失败/解析为空
// 保留上一次成功结果；解析时的非法条目只记 WARN，不影响其余条目。
func resolveProxySubscriptions(subs []string) []string {
	var out []string

	for _, sub := range subs {
		text, err := proxySubFetcher(sub)
		if err != nil {
			cached := cachedProxySubscription(sub)
			log.Printf("WARN: [server] 代理订阅不可用（沿用上次结果 %d 条）: %v", len(cached), err)
			out = append(out, cached...)

			continue
		}

		proxies, skipped := parseProxySubscription(text)
		if len(proxies) == 0 {
			cached := cachedProxySubscription(sub)
			log.Printf("WARN: [server] 代理订阅未解析出可用代理（沿用上次结果 %d 条）: %s（跳过 %d 条）",
				len(cached), maskProxySubURL(sub), len(skipped))
			out = append(out, cached...)

			continue
		}

		for _, reason := range skipped {
			log.Printf("WARN: [server] 代理订阅条目跳过: %s", reason)
		}

		storeProxySubscription(sub, proxies)
		log.Printf("INFO: [server] 代理订阅已解析: %s（%d 个代理，跳过 %d 条）",
			maskProxySubURL(sub), len(proxies), len(skipped))
		out = append(out, proxies...)
	}

	return out
}

// parseProxySubscription 解析订阅文本：先尝试 base64 解包，再按 Clash YAML 或纯
// URL 列表分派。skipped 是逐条跳过原因（已脱敏）。
func parseProxySubscription(text string) (proxies, skipped []string) {
	text = decodeProxySubscription(text)
	if isClashConfig(text) {
		return parseClashProxies(text)
	}

	return parseProxyList(text)
}

// decodeProxySubscription 识别 base64 包裹的订阅：明文含 scheme 或 proxies: 时
// 原样返回；否则按常见 base64 变体解一次，解出的文本仍须含代理特征才采用。
func decodeProxySubscription(text string) string {
	if strings.Contains(text, "://") || isClashConfig(text) {
		return text
	}

	compact := strings.Join(strings.Fields(text), "")
	if compact == "" {
		return text
	}

	for _, encoding := range []*base64.Encoding{
		base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding,
	} {
		decoded, err := encoding.DecodeString(compact)
		if err != nil {
			continue
		}

		if candidate := strings.TrimSpace(string(decoded)); strings.Contains(candidate, "://") ||
			isClashConfig(candidate) {
			return candidate
		}
	}

	return text
}

// isClashConfig 报告文本是否含顶层 proxies: 段。
func isClashConfig(text string) bool {
	for line := range strings.SplitSeq(text, "\n") {
		if strings.TrimSpace(line) == "proxies:" {
			return true
		}
	}

	return false
}

// parseProxyList 解析「每行一条」的代理列表。空行与 # / // 注释跳过。
func parseProxyList(text string) (proxies, skipped []string) {
	for line := range strings.SplitSeq(text, "\n") {
		entry := strings.TrimSpace(line)
		if entry == "" || strings.HasPrefix(entry, "#") || strings.HasPrefix(entry, "//") {
			continue
		}

		if _, err := outbound.ParseProxy(entry); err != nil {
			skipped = append(skipped, fmt.Sprintf("%s: %v", maskProxyEntry(entry), err))

			continue
		}

		proxies = append(proxies, entry)
	}

	return proxies, skipped
}

// parseClashProxies 解析 Clash / KM YAML 的 proxies: 段。只支持 http/https/socks5，
// 其余类型（ss / vmess / trojan / hysteria…）单独跳过并给原因。
func parseClashProxies(text string) (proxies, skipped []string) {
	for _, item := range splitClashProxyItems(text) {
		entry, reason := clashProxyToURL(item)
		if reason != "" {
			skipped = append(skipped, reason)

			continue
		}

		if _, err := outbound.ParseProxy(entry); err != nil {
			skipped = append(skipped, fmt.Sprintf("%s: %v", maskProxyEntry(entry), err))

			continue
		}

		proxies = append(proxies, entry)
	}

	return proxies, skipped
}

// splitClashProxyItems 抽取 proxies: 段里的条目（每项一个 key/value 表）。只做
// 行级最小解析：列表项 `- ` 起新项，后续 `key: value` 归属当前项；顶层其它键结束。
//
//nolint:cyclop,funlen,gocognit,gocyclo // 行级状态机：段起止、缩进、列表项与键值线性展开。
func splitClashProxyItems(text string) []map[string]string {
	var (
		items     []map[string]string
		current   map[string]string
		inProxies bool
	)

	flush := func() {
		if current != nil {
			items = append(items, current)
			current = nil
		}
	}

	for line := range strings.SplitSeq(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}

		indent := len(line) - len(strings.TrimLeft(line, " \t"))

		if !inProxies {
			if trimmed == "proxies:" {
				inProxies = true
			}

			continue
		}

		// 顶层其它键（未缩进、非列表项）=> proxies 段结束。
		if indent == 0 && !strings.HasPrefix(trimmed, "-") {
			break
		}

		if strings.HasPrefix(trimmed, "-") {
			flush()

			current = map[string]string{}
			parseClashInlineItem(strings.TrimSpace(strings.TrimPrefix(trimmed, "-")), current)

			continue
		}

		if current == nil {
			continue
		}

		if key, value, ok := splitClashKeyValue(trimmed); ok {
			current[key] = value
		}
	}

	flush()

	return items
}

// parseClashInlineItem 处理 `- {name: x, type: http, ...}` 行内形态；块形态传入空串。
func parseClashInlineItem(rest string, item map[string]string) {
	rest = strings.TrimSpace(rest)
	if !strings.HasPrefix(rest, "{") {
		if key, value, ok := splitClashKeyValue(rest); ok {
			item[key] = value
		}

		return
	}

	rest = strings.TrimSuffix(strings.TrimPrefix(rest, "{"), "}")

	for pair := range strings.SplitSeq(rest, ",") {
		if key, value, ok := splitClashKeyValue(pair); ok {
			item[key] = value
		}
	}
}

// splitClashKeyValue 拆分一行 `key: value`，去掉两端引号。
func splitClashKeyValue(line string) (string, string, bool) {
	key, value, ok := strings.Cut(line, ":")
	if !ok {
		return "", "", false
	}

	key = strings.TrimSpace(key)
	value = strings.Trim(strings.TrimSpace(value), `"'`)

	if key == "" || value == "" {
		return "", "", false
	}

	return strings.ToLower(key), value, true
}

// clashProxyToURL 把一条 Clash 代理映射成 outbound.ParseProxy 认得的 URL。
// 返回非空 reason 表示跳过。
func clashProxyToURL(item map[string]string) (string, string) {
	kind := strings.ToLower(item["type"])
	server := item["server"]
	port := item["port"]
	name := item["name"]

	if server == "" || port == "" {
		return "", fmt.Sprintf("Clash 条目 %q: 缺少 server/port 已跳过", name)
	}

	switch kind {
	case "http", "https", "socks5", "socks5h":
	default:
		return "", fmt.Sprintf("Clash 条目 %q: 不支持的代理类型 %q 已跳过", name, kind)
	}

	u := url.URL{Scheme: kind, Host: net.JoinHostPort(server, port)}
	if user := item["username"]; user != "" {
		u.User = url.UserPassword(user, item["password"])
	}

	return u.String(), ""
}
