// agentic_search.go 官方综述式检索（/agenttool/v1/agentic_search，issue #277）。
//
// 契约（官方 main/server.js，CN 5.8.3 / global 5.8.4 同构）：
//   - POST {chatBase}/agenttool/v1/agentic_search
//   - 请求体 {"query":..., "search_mode":2, "stream":true, "biz_via":"..."}
//   - SSE 事件 tool_call / tool_result / done；done 帧的 synthesis.content 是最终综述
//   - 整体超时官方 300s；本仓收紧到 120s，避免长时间占满在途名额
package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
)

const (
	agenticSearchPath    = "/agenttool/v1/agentic_search"
	agenticSearchMode    = 2
	agenticSearchTimeout = 120 * time.Second
	agenticSearchMaxBody = 4 << 20
)

var (
	// ErrAgenticSearchNoSynthesis SSE 流没有 done 帧或综述为空。
	ErrAgenticSearchNoSynthesis = errors.New("agentic search returned no synthesis")
	// ErrAgenticSearchTooLarge 响应超过上限。
	ErrAgenticSearchTooLarge = errors.New("agentic search response too large")
)

// AgenticUsage done 帧可能携带的用量（不同上游字段名不同，解析时归一）。
type AgenticUsage struct {
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	TotalTokens      int64 `json:"total_tokens"`
}

// AgenticOutcome 一次深检索的结果。
type AgenticOutcome struct {
	Query     string
	Synthesis string
	Usage     *AgenticUsage
}

// agenticRequestBody 出站请求体（字段顺序对齐官方 callBackendSse）。
type agenticRequestBody struct {
	Query      string `json:"query"`
	SearchMode int    `json:"search_mode"`
	Stream     bool   `json:"stream"`
	BizVia     string `json:"biz_via"`
}

// AgenticSearch 对指定账号执行一次综述式检索。只做单次上游调用，不选号/轮转。
// meta 与 snippet 搜索同口径：入站会话头族原样透传（上游按会话聚合搜索流量）。
//
//nolint:gocritic,funlen // ChatMeta 值传递与 SearchHeaders 同口径；单次调用 + 解析线性展开。
func (c *Client) AgenticSearch(
	ctx context.Context, account *auth.Auth, query, bizVia string, meta ChatMeta,
) (*AgenticOutcome, error) {
	query = strings.TrimSpace(query)
	if len([]rune(query)) < SearchMinQueryLen {
		return nil, &Error{
			Kind: ErrClient, Msg: fmt.Sprintf("search query must be at least %d characters", SearchMinQueryLen),
		}
	}

	if strings.TrimSpace(bizVia) == "" {
		bizVia = "public"
	}

	body, err := json.Marshal(agenticRequestBody{
		Query: query, SearchMode: agenticSearchMode, Stream: true, BizVia: strings.TrimSpace(bizVia),
	})
	if err != nil {
		return nil, fmt.Errorf("agentic search marshal: %w", err)
	}

	ctx, cancel := context.WithTimeout(ctx, agenticSearchTimeout)
	defer cancel()

	req, err := c.newAgentToolRequest(ctx, account, agenticSearchPath, body, surfaceAgenticSearch, meta)
	if err != nil {
		return nil, fmt.Errorf("agentic search request: %w", err)
	}

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("agentic search do: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, agenticSearchMaxBody+1))
	if err != nil {
		return nil, fmt.Errorf("agentic search read: %w", err)
	}

	if len(raw) > agenticSearchMaxBody {
		return nil, fmt.Errorf("%w: > %d bytes", ErrAgenticSearchTooLarge, agenticSearchMaxBody)
	}

	outcome, err := agenticOutcomeFromResponse(resp.StatusCode, raw)
	if err != nil {
		return nil, err
	}

	outcome.Query = query

	return outcome, nil
}

// agenticOutcomeFromResponse 处理状态码/业务错误/SSE，拆开降低主流程复杂度。
func agenticOutcomeFromResponse(status int, raw []byte) (*AgenticOutcome, error) {
	if status >= http.StatusBadRequest {
		return nil, &Error{
			Kind: Classify(status, string(raw)), Status: status,
			Msg: truncate(string(raw), 200), Code: businessCode(string(raw)),
		}
	}

	if outcome := agenticJSONError(status, raw); outcome != nil {
		return nil, outcome
	}

	return parseAgenticSSE(raw)
}

// agenticJSONError 识别 200 + JSON 业务错误（部分故障形态），避免当 SSE 解析后报
// “无综述”。非 JSON 业务错误返回 nil。
func agenticJSONError(status int, raw []byte) error {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return nil
	}

	var env apiEnvelope
	if json.Unmarshal(trimmed, &env) != nil {
		return nil //nolint:nilerr // 非 JSON 业务错误，交给 SSE 解析
	}

	if env.Code == 0 {
		return nil
	}

	kind := Classify(status, string(trimmed))
	if kind == ErrNone {
		kind = ErrClient
	}

	return &Error{Kind: kind, Status: status, Msg: truncate(env.Msg, 160), Code: businessCode(string(trimmed))}
}

// parseAgenticSSE 解析 SSE：兼容 LF / CRLF / CR，只取 done 帧的 synthesis.content。
func parseAgenticSSE(raw []byte) (*AgenticOutcome, error) {
	text := strings.ReplaceAll(string(raw), "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")

	for block := range strings.SplitSeq(text, "\n\n") {
		event, data := parseAgenticEvent(block)
		if event != "done" || data == "" || data == "[DONE]" {
			continue
		}

		var frame struct {
			Synthesis struct {
				Content string        `json:"content"`
				Usage   *AgenticUsage `json:"usage"`
			} `json:"synthesis"`
			Usage      *AgenticUsage `json:"usage"`
			TokenUsage *AgenticUsage `json:"token_usage"`
		}
		if json.Unmarshal([]byte(data), &frame) != nil {
			return nil, fmt.Errorf("agentic search parse done frame: %w", ErrAgenticSearchNoSynthesis)
		}

		content := strings.TrimSpace(frame.Synthesis.Content)
		if content == "" {
			return nil, ErrAgenticSearchNoSynthesis
		}

		usage := frame.Synthesis.Usage
		if usage == nil {
			usage = frame.Usage
		}

		if usage == nil {
			usage = frame.TokenUsage
		}

		return &AgenticOutcome{Synthesis: content, Usage: usage}, nil
	}

	return nil, ErrAgenticSearchNoSynthesis
}

// parseAgenticEvent 解析单个 SSE 事件块的 event 名与 data（多行 data 用 \n 拼接）。
func parseAgenticEvent(block string) (event, data string) {
	var lines []string

	for line := range strings.SplitSeq(block, "\n") {
		line = strings.TrimSuffix(line, "\r")
		if line == "" || strings.HasPrefix(line, ":") {
			continue
		}

		if rest, ok := strings.CutPrefix(line, "event:"); ok {
			event = strings.TrimSpace(rest)
			continue
		}

		if rest, ok := strings.CutPrefix(line, "data:"); ok {
			lines = append(lines, strings.TrimPrefix(rest, " "))
		}
	}

	return event, strings.Join(lines, "\n")
}
