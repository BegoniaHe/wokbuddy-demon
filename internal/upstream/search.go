// search.go 联网搜索接口：/agenttool/v1/search。
//
// 上游契约（对齐官方客户端 WebSearchTool，实测 2026-10）：
//   - POST {chatBase}/agenttool/v1/search，超时 20s
//   - 请求体 {"query":..., "type":"text2text", "max_results":N}
//   - 响应为**扁平结构**（非 {code,msg,data} 信封）：
//     {query, type, provider, results[{title,url,snippet,site?,favicon?}],
//     total_results, response_time_ms}
//   - 业务错误才带 code/msg（如 code=15003 unsupported search type/provider），
//     且通常同时伴随 HTTP 4xx
//
// 为什么不能复用 doJSON：doJSON 解的是 {code,msg,data} 统一信封并只返回 data；
// 搜索成功响应没有 data 包裹，直接套信封会拿到空 data。故单独实现 doSearch，
// 但错误分类与 doJSON 保持同一口径（复用 Classify/Error），不引入第二套判定。
package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
)

const (
	// searchPath 搜索端点路径。官方 CLI 同名常量 WEB_SEARCH_API_PATH。
	// 与 chat 共用同一 endpoint（官方 product.endpoint 同时承载 /v2/chat/completions
	// 与 /agenttool/v1/search），故 base 复用 chatBase。
	searchPath = "/agenttool/v1/search"

	// searchTimeout 搜索请求整体超时，对齐官方 WEB_SEARCH_TIMEOUT_MS（2e4）。
	// 必须显式设置：搜索是同步 JSON 请求（非流式），上游 hang 时若无超时会一直占用
	// 在途租约，把账号池名额耗光。
	searchTimeout = 20 * time.Second

	// SearchDefaultMaxResults 默认返回条数 = 官方硬编码值（buildRequestBody 固定 5）。
	SearchDefaultMaxResults = 5

	// SearchMaxResultsCap 服务端实际返回上限（实测约 10）。
	// 请求值超过该上限不会报错、也不会多返回，只是被服务端截断——故在入口收敛，
	// 避免把无效配置透传给上游并给调用方「配了就该生效」的错觉。
	SearchMaxResultsCap = 10

	// SearchMinQueryLen 官方 zod schema 的 query.min(2)。
	SearchMinQueryLen = 2

	// SearchMaxDomains 单个请求允许的域名条目上限（防御性：只影响 query 体积）。
	SearchMaxDomains = 20

	// SearchMaxDomainLen 单个域名 host 长度上限（DNS 规范 253）。
	SearchMaxDomainLen = 253
)

// SearchResult 单条搜索结果。
// site / favicon 是上游可选字段（官方客户端同样按「有才带」处理）。
type SearchResult struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Snippet string `json:"snippet,omitempty"`
	Site    string `json:"site,omitempty"`
	Favicon string `json:"favicon,omitempty"`
}

// SearchOutcome 一次搜索的完整结果，字段名与上游响应一致，便于直接透出。
type SearchOutcome struct {
	Query          string         `json:"query"`
	Results        []SearchResult `json:"results"`
	TotalResults   int            `json:"total_results"`
	Provider       string         `json:"provider,omitempty"`
	ResponseTimeMs int64          `json:"response_time_ms,omitempty"`
}

// agenttool 四路的头族差异（Accept / Content-Type / X-Wb-Uid）在 headers.go 的
// chatFamilyHeaders 里按 surface 集中表达：search 与 webfetch 是非流式 JSON
// （Content-Type 带 charset），agentic_search 走 SSE（Accept: text/event-stream），
// 只有 webfetch 带 X-Wb-Uid。
//
// 为什么整体复用 chat 头族而不是照抄官方精简头清单：官方 CLI 的搜索头就是它同一个
// 客户端的通用头（chat 与搜索共用同一 UA 与会话头族）。本仓库出站身份是桌面端，若
// 搜索另发一套精简头，上游会看到「同账号同时段两种客户端指纹」，反而更像异常。
// 决策记录见 `internal/upstream/UPSTREAM.md` §六。

// ClampSearchMaxResults 把调用方给的条数收敛到 [1, SearchMaxResultsCap]；
// <=0 视为未指定，回落官方默认值。
func ClampSearchMaxResults(n int) int {
	if n <= 0 {
		return SearchDefaultMaxResults
	}

	if n > SearchMaxResultsCap {
		return SearchMaxResultsCap
	}

	return n
}

// SearchOptions 一次搜索的可选参数（字段名对齐官方 WebSearchTool 工具声明）。
type SearchOptions struct {
	MaxResults     int
	AllowedDomains []string
	BlockedDomains []string
	Freshness      string
}

// searchRequestBody 出站搜索请求体。
//
// 用 struct 而不是 map：Go 的 map 序列化会按键名排序，而官方 buildRequestBody 的
// 字段顺序是 query → type → max_results → allowed_domains → freshness。JSON 语义上
// 顺序无关，但保持与真实客户端同形便于抓包比对与可能的 body 指纹风控。
type searchRequestBody struct {
	Query          string   `json:"query"`
	Type           string   `json:"type"`
	MaxResults     int      `json:"max_results"`
	AllowedDomains []string `json:"allowed_domains,omitempty"`
	Freshness      string   `json:"freshness,omitempty"`
}

// searchFreshnessRE 官方工具 schema 的 freshness 正则（d1-d30 / m1-m12 / y1-y5）。
var searchFreshnessRE = regexp.MustCompile(`^(?:d(?:[1-9]|[12]\d|30)?|m(?:[1-9]|1[012])?|y[1-5]?)$`)

// searchDomainRE 域名 host 白名单：字母/数字/点/连字符/下划线，首尾非分隔符。
var searchDomainRE = regexp.MustCompile(`^[A-Za-z0-9_](?:[A-Za-z0-9._-]*[A-Za-z0-9_])?$`)

// ValidSearchFreshness 报告 freshness 是否合法（空串 = 未指定，合法）。
func ValidSearchFreshness(freshness string) bool {
	freshness = strings.TrimSpace(freshness)
	return freshness == "" || searchFreshnessRE.MatchString(freshness)
}

// ValidSearchDomain 报告域名是否只包含 host（拒绝协议/路径/端口/userinfo/空白/超长）。
func ValidSearchDomain(domain string) bool {
	domain = strings.TrimSpace(domain)
	if domain == "" || len(domain) > SearchMaxDomainLen {
		return false
	}

	if strings.ContainsAny(domain, "/\\?#@%") || strings.Contains(domain, "://") {
		return false
	}

	return searchDomainRE.MatchString(domain)
}

// normalizeSearchOptions 去空白并生成官方请求体字段；blocked_domains 拼进 query
// （官方形态：" (" + "-site:a -site:b" + ")"）。
func normalizeSearchOptions(query string, opts SearchOptions) searchRequestBody {
	allowed := cleanSearchDomains(opts.AllowedDomains)
	blocked := cleanSearchDomains(opts.BlockedDomains)

	if len(blocked) > 0 {
		var suffix strings.Builder
		suffix.WriteString(" (")

		for i, domain := range blocked {
			if i > 0 {
				suffix.WriteString(" ")
			}

			suffix.WriteString("-site:")
			suffix.WriteString(domain)
		}

		suffix.WriteString(")")
		query += suffix.String()
	}

	return searchRequestBody{
		Query:          query,
		Type:           "text2text",
		MaxResults:     ClampSearchMaxResults(opts.MaxResults),
		AllowedDomains: allowed,
		Freshness:      strings.TrimSpace(opts.Freshness),
	}
}

// cleanSearchDomains 去空白、去重并保持输入顺序；空项丢弃。
func cleanSearchDomains(domains []string) []string {
	if len(domains) == 0 {
		return nil
	}

	out := make([]string, 0, len(domains))
	seen := make(map[string]bool, len(domains))

	for _, domain := range domains {
		domain = strings.TrimSpace(domain)
		if domain == "" || seen[domain] {
			continue
		}

		seen[domain] = true
		out = append(out, domain)
	}

	if len(out) == 0 {
		return nil
	}

	return out
}

// Search 对指定账号执行一次联网搜索。
//
// 只做「一次上游调用 + 结果解析」，不涉及选号/轮转/冷却——那些属于 handler 的职责
// （与 chat 一致：upstream 层不感知账号池）。
//
// 返回的 error 在 HTTP 非 2xx 或业务 code != 0 时是 *Error（带 Kind，供 handler
// 决定是否罚号），其余（序列化/读体失败）是普通 error（传输层故障不罚号）。
//
//nolint:gocritic // ChatMeta 值传递与 ChatHeaders 保持同一口径（公开 API 简单）。
func (c *Client) Search(
	ctx context.Context, account *auth.Auth, query string, opts SearchOptions, meta ChatMeta,
) (*SearchOutcome, error) {
	query = strings.TrimSpace(query)
	if utf8.RuneCountInString(query) < SearchMinQueryLen {
		return nil, &Error{
			Kind: ErrClient, Msg: fmt.Sprintf("search query must be at least %d characters", SearchMinQueryLen),
		}
	}

	if !ValidSearchFreshness(opts.Freshness) {
		return nil, &Error{Kind: ErrClient, Msg: "invalid search freshness"}
	}

	body, err := json.Marshal(normalizeSearchOptions(query, opts))
	if err != nil {
		return nil, fmt.Errorf("search marshal: %w", err)
	}

	ctx, cancel := context.WithTimeout(ctx, searchTimeout)
	defer cancel()

	req, err := c.newAgentToolRequest(ctx, account, searchPath, body, surfaceSearch, meta)
	if err != nil {
		return nil, fmt.Errorf("search request: %w", err)
	}

	return c.doSearch(account, req)
}

// newAgentToolRequest 构造 agenttool 系请求（search / webfetch / agentic_search）：
// base 先归一，头族按 surface 由 applyHeaders 统一构造（差异见 chatFamilyHeaders）。
func (c *Client) newAgentToolRequest(
	//nolint:gocritic // ChatMeta 值传递与 ChatHeaders 保持同一口径。
	ctx context.Context, account *auth.Auth, path string, body []byte, srf surface, meta ChatMeta,
) (*http.Request, error) {
	req, err := http.NewRequestWithContext(
		ctx, http.MethodPost, normalizeChatBase(c.chatBase(account))+path, bytes.NewReader(body),
	)
	if err != nil {
		return nil, fmt.Errorf("new agenttool request: %w", err)
	}

	c.applyHeaders(srf, req, account, "", meta)

	return req, nil
}

// doSearch 发搜索请求并解析扁平响应。
//
// 错误处理与 doJSON 同口径：
//   - HTTP >= 400 → Classify 出 Kind，包成 *Error
//   - 2xx 但 code != 0 → 业务错误（Classify 判不出时兜底 ErrClient）
//   - 读体失败 → 普通 error（半截 body 不参与账号惩罚）
func (c *Client) doSearch(account *auth.Auth, req *http.Request) (*SearchOutcome, error) {
	resp, err := c.HTTPForAccount(account).Do(req)
	if err != nil {
		return nil, fmt.Errorf("search do: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}

	if resp.StatusCode >= http.StatusBadRequest {
		kind := Classify(resp.StatusCode, string(raw))

		return nil, &Error{Kind: kind, Status: resp.StatusCode, Msg: truncate(string(raw), 200)}
	}

	// 成功响应没有 data 包裹：code/msg 与结果字段同级，用嵌入结构一次性解出。
	var parsed struct {
		SearchOutcome

		Code int    `json:"code"`
		Msg  string `json:"msg"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("parse failed: %w (body: %s)", err, truncate(string(raw), 120))
	}

	if parsed.Code != 0 {
		// 用完整 body 分类：业务码（如 15001 搜索额度耗尽）只在 code 字段里，
		// 只传 msg 会丢失分类信息，把它降级成通用 ErrClient。
		kind := Classify(resp.StatusCode, string(raw))
		if kind == ErrNone {
			kind = ErrClient
		}

		return nil, &Error{
			Kind: kind, Status: resp.StatusCode,
			Msg: fmt.Sprintf("code=%d msg=%s", parsed.Code, truncate(parsed.Msg, 160)),
		}
	}

	out := parsed.SearchOutcome
	// results 恒输出为数组（nil 会让 JSON 变成 null，调用方还要判空）。
	if out.Results == nil {
		out.Results = []SearchResult{}
	}
	// 上游个别情况下 total_results 缺省，用实际条数兜底，避免「0 条却有条目」的矛盾。
	if out.TotalResults == 0 && len(out.Results) > 0 {
		out.TotalResults = len(out.Results)
	}

	return &out, nil
}
