// tools_search.go POST /v1/tools/search：DeepSeek 官方文档形状的联网搜索出口
// （issue #608）。受 features.search_endpoint 门控（默认关，热生效）。
//
// 与 /v1/search/messages 的关系：同一个上游能力（/agenttool/v1/search）、同一套
// 取号/换号/失败处置/用量记账（runSearch → runAgentTool），差别只在入站与回程：
// 这里是 DeepSeek 的裸 JSON 契约，那边是 Anthropic Messages 契约。
//
// need_content / ori_question / localization 上游没有对应能力，按 DeepSeek 文档
// 「未知字段忽略」静默忽略：不报错、不伪造正文（要正文走 /v1/search/messages 的
// deep=true 综述式检索）。
package server

import (
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/BegoniaHe/wokbuddy-demon/internal/jsonx"
	"github.com/BegoniaHe/wokbuddy-demon/internal/reqlog"
	"github.com/BegoniaHe/wokbuddy-demon/internal/upstream"
)

const (
	// toolsSearchQueryMaxLen 契约的 query 长度上限（按字符计，不是字节）。
	toolsSearchQueryMaxLen = 128

	// toolsSearchCountDefault count 缺省值。
	toolsSearchCountDefault = 10

	// toolsSearchCountMax count 上限。上游实际最多返回 10 条（ClampSearchMaxResults
	// 在出站时收敛），中间值不做额外收紧：契约允许 50，截断是上游行为。
	toolsSearchCountMax = 50

	// toolsSearchUsageModel 用量账本里的模型维度：本端点入站没有 model 字段
	// （与 webfetch 用固定维度的口径一致）。
	toolsSearchUsageModel = "search"

	// toolsSearchMaxBody 本端点的请求体上限：入站只有一个 query 与 count，64KiB 足够。
	toolsSearchMaxBody = 64 << 10
)

// toolsSearchRequestBody 入站请求体。字段名对齐 DeepSeek 契约；未声明的字段
// （need_content / ori_question / localization）解码时直接忽略。
type toolsSearchRequestBody struct {
	Query string `json:"query"`
	// Count 用指针区分「未传」（缺省 10）与「显式 0」（越界，400）。
	Count *int `json:"count"`
}

// toolsSearchResult 契约的 results[] 条目：十个键必须全部出现（客户端按固定结构读），
// 无对应能力的给契约默认值而不是省键。site_name / site_icon / date 可为 null。
type toolsSearchResult struct {
	RerankScore float64 `json:"rerank_score"`
	Title       string  `json:"title"`
	URL         string  `json:"url"`
	Snippet     string  `json:"snippet"`
	Summary     string  `json:"summary"`
	Content     string  `json:"content"`
	SiteName    *string `json:"site_name"`
	SiteIcon    *string `json:"site_icon"`
	Date        *string `json:"date"`
	Authority   int     `json:"authority"`
}

// toolsSearchResponse 契约的回程信封。
type toolsSearchResponse struct {
	Results []toolsSearchResult `json:"results"`
}

// toolsSearchEndpointHandler /v1/tools/search 入口（鉴权由 withClientAuth 负责）。
func (h *Handler) toolsSearchEndpointHandler(w http.ResponseWriter, r *http.Request) {
	if !h.loadLive().SearchEndpoint {
		writeOpenAIError(w, http.StatusForbidden, "forbidden",
			"search endpoint is disabled; enable features.search_endpoint to use POST /v1/tools/search")

		return
	}

	// 命名密钥绑定出口是运维侧的显式约束：搜索只放行 CN 域选号，绑定 global 的 key
	// 不能在未经授权的情况下消耗 CN 账号额度（与 /v1/search/messages 同口径）。
	if keyRealm := keyRealmFrom(r.Context()); keyRealm != "" && keyRealm != searchRealm {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request",
			"search endpoint only supports the cn realm; this API key is bound to "+keyRealm)

		return
	}

	query, count, ok := readToolsSearchRequest(w, r)
	if !ok {
		return
	}

	// 会话头族在轮转外构造一次：同一次搜索的换号重试必须复用同一聚合主键。
	meta := searchMetaFromRequest(r)
	started := time.Now()

	outcome, uid, fail := h.runSearch(r.Context(), query, upstream.SearchOptions{MaxResults: count}, &meta)
	h.recordSearchUsage(r, uid, toolsSearchUsageModel, started, nil, fail == nil)

	if fail != nil {
		// 调用方已断开：连接已关，写响应体没有意义，也不能记成上游失败。
		if fail.status == clientClosedRequestStatus {
			noteSearchTrace(r, h, uid, toolsSearchUsageModel, reqlog.OutcomeInterrupted)

			return
		}

		noteSearchTrace(r, h, uid, toolsSearchUsageModel, reqlog.OutcomeHTTPError)
		writeOpenAIErrorHint(w, fail.status, toolsSearchErrorCode(fail.status), fail.msg, searchHint(fail))

		return
	}

	noteSearchTrace(r, h, uid, toolsSearchUsageModel, reqlog.OutcomeSuccess)
	writeJSON(w, http.StatusOK, buildToolsSearchResponse(outcome))
}

// readToolsSearchRequest 读 body 并按契约取出 query / count；失败已写出响应。
func readToolsSearchRequest(w http.ResponseWriter, r *http.Request) (string, int, bool) {
	limitRequestBody(w, r, toolsSearchMaxBody)

	body, err := io.ReadAll(r.Body)
	if err != nil {
		if isBodyTooLarge(err) {
			writeOpenAIError(w, http.StatusRequestEntityTooLarge, "payload_too_large", "request body too large")
		} else {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "read body failed")
		}

		return "", 0, false
	}

	query, count, err := normalizeToolsSearchRequest(body)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", err.Error())

		return "", 0, false
	}

	return query, count, true
}

// normalizeToolsSearchRequest 解码并校验 query / count：越界一律 400，不出网。
func normalizeToolsSearchRequest(body []byte) (string, int, error) {
	var req toolsSearchRequestBody
	if jsonx.Unmarshal(body, &req) != nil {
		return "", 0, errToolsSearchInvalidJSON
	}

	query := strings.TrimSpace(req.Query)

	// 下限取上游口径（upstream.SearchMinQueryLen）：单字符 query 上游一定拒，入口
	// 先拦能省一次账号额度，且错误信息比上游原文更直白。
	switch runes := utf8.RuneCountInString(query); {
	case runes == 0:
		return "", 0, errToolsSearchQueryRequired
	case runes < upstream.SearchMinQueryLen:
		return "", 0, errToolsSearchQueryTooShort
	case runes > toolsSearchQueryMaxLen:
		return "", 0, errToolsSearchQueryTooLong
	}

	count := toolsSearchCountDefault
	if req.Count != nil {
		count = *req.Count
		if count < 1 || count > toolsSearchCountMax {
			return "", 0, errToolsSearchCountRange
		}
	}

	return query, count, nil
}

// buildToolsSearchResponse 把上游扁平结果归一成 DeepSeek 契约：
//   - title / url / snippet 原样透出（缺失即空串；不拿 URL 顶替 title——契约要求
//     字符串，空串就是「上游没给」）；
//   - summary 上游没有独立摘要字段 → 空串；
//   - content 上游 snippet 搜索不返回正文 → 恒空串（need_content 已静默忽略，
//     绝不拿 snippet 冒充正文）；
//   - site_name / site_icon：上游响应里的 site / favicon 未在网关结果结构里声明
//     （语义未经实测确认），按契约给 null；
//   - date 上游无日期字段 → null；
//   - rerank_score 无重排分 → 0；authority 契约里「无权威度」用 -1。
func buildToolsSearchResponse(outcome *upstream.SearchOutcome) toolsSearchResponse {
	resp := toolsSearchResponse{Results: make([]toolsSearchResult, 0, len(outcome.Results))}

	for _, result := range outcome.Results {
		resp.Results = append(resp.Results, toolsSearchResult{
			Title:     result.Title,
			URL:       result.URL,
			Snippet:   result.Snippet,
			Authority: -1,
		})
	}

	return resp
}

// toolsSearchErrorCode 把终端状态码映射成错误 code。DeepSeek 客户端不消费该字段，
// 只要求与其它端点同形、且不含上游内部细节。
func toolsSearchErrorCode(status int) string {
	switch status {
	case http.StatusBadRequest:
		return "invalid_request"
	case http.StatusTooManyRequests:
		return "rate_limit_exceeded"
	case http.StatusForbidden:
		return "forbidden"
	case http.StatusServiceUnavailable:
		return "service_unavailable"
	default:
		return "upstream_search_failed"
	}
}

// 入站校验错误（面向调用方，英文 + OpenAI 错误信封：本端点是裸 JSON 契约）。
var (
	errToolsSearchInvalidJSON   = errors.New("invalid JSON body")
	errToolsSearchQueryRequired = errors.New("query is required")
	errToolsSearchQueryTooShort = errors.New(
		"query must be at least " + strconv.Itoa(upstream.SearchMinQueryLen) + " characters")
	errToolsSearchQueryTooLong = errors.New(
		"query too long (max " + strconv.Itoa(toolsSearchQueryMaxLen) + " characters)")
	errToolsSearchCountRange = errors.New(
		"count must be between 1 and " + strconv.Itoa(toolsSearchCountMax))
)
