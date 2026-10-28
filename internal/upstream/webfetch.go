// webfetch.go 官方服务端抓取（/agenttool/v1/webfetch，issue #276）。
//
// 契约（官方 cli/dist/codebuddy-lite-wb.mjs 的 fetchContentViaApi）：
//   - POST {chatBase}/agenttool/v1/webfetch
//   - 请求体 {"url","format":"markdown","prompt","timeout":秒,"max_length","model_name"?}
//   - 响应 {code,msg,url,content_type,content}；非文本资源不伪造正文
package upstream

import (
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
	webFetchPath         = "/agenttool/v1/webfetch"
	webFetchDefaultSecs  = 20
	webFetchMaxSecs      = 60
	webFetchDefaultMax   = 100_000
	webFetchMaxLengthCap = 1_000_000
	webFetchMaxBody      = 8 << 20
)

var (
	// ErrWebFetchEmptyContent 上游缺少 content 或 content 不是非空字符串。
	ErrWebFetchEmptyContent = errors.New("webfetch returned no readable content")
	// ErrWebFetchNonText 上游返回非文本资源。
	ErrWebFetchNonText = errors.New("webfetch returned non-text content")
	// ErrWebFetchTooLarge 响应超过上限。
	ErrWebFetchTooLarge = errors.New("webfetch response too large")
	// ErrWebFetchBadResponse 200 响应不是可解析的 JSON（协议/代理故障，换号不会变）。
	ErrWebFetchBadResponse = errors.New("webfetch returned malformed response")
)

// WebFetchRequest 一次服务端抓取的可选参数。URL 必填；其余空值按官方默认。
type WebFetchRequest struct {
	URL       string `json:"url"`
	Format    string `json:"format,omitempty"`
	Prompt    string `json:"prompt,omitempty"`
	Timeout   int    `json:"timeout,omitempty"` // 秒
	MaxLength int    `json:"max_length,omitempty"`
	ModelName string `json:"model_name,omitempty"`
}

// WebFetchOutcome 抓取结果（只保留客户端需要的字段）。
type WebFetchOutcome struct {
	URL         string
	ContentType string
	Content     string
}

// webFetchRequestBody 出站请求体（字段顺序对齐官方）。
type webFetchRequestBody struct {
	URL       string `json:"url"`
	Format    string `json:"format"`
	Prompt    string `json:"prompt"`
	Timeout   int    `json:"timeout"`
	MaxLength int    `json:"max_length"`
	ModelName string `json:"model_name,omitempty"`
}

// WebFetch 对指定账号执行一次官方服务端抓取。只做单次上游调用。
// meta 与 search / agentic_search 同口径：入站会话头族原样透传（官方客户端
// fetchContentViaApi 也会带 X-Conversation-Id / X-Conversation-Request-Id）。
//
//nolint:gocritic // 公开 API 值传递，调用方无需关心指针语义。
func (c *Client) WebFetch(
	ctx context.Context, account *auth.Auth, req WebFetchRequest, meta ChatMeta,
) (*WebFetchOutcome, error) {
	body, timeout, err := normalizeWebFetchRequest(&req)
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(ctx, time.Duration(timeout+10)*time.Second)
	defer cancel()

	httpReq, err := c.newAgentToolRequest(ctx, account, webFetchPath, body, surfaceWebFetch, meta)
	if err != nil {
		return nil, fmt.Errorf("webfetch request: %w", err)
	}

	resp, err := c.HTTP.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("webfetch do: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, webFetchMaxBody+1))
	if err != nil {
		return nil, fmt.Errorf("webfetch read: %w", err)
	}

	if len(raw) > webFetchMaxBody {
		return nil, fmt.Errorf("%w: > %d bytes", ErrWebFetchTooLarge, webFetchMaxBody)
	}

	return parseWebFetchResponse(resp.StatusCode, raw, req.URL)
}

// normalizeWebFetchRequest 归一默认值与上限，返回出站 body 与实际超时秒数。
func normalizeWebFetchRequest(req *WebFetchRequest) ([]byte, int, error) {
	url := strings.TrimSpace(req.URL)
	if url == "" {
		return nil, 0, &Error{Kind: ErrClient, Msg: "webfetch url is required"}
	}
	// 回写去空白后的 URL：上游未回传 url 时以它为回落值，不能把首尾空白带给客户端。
	req.URL = url

	format := strings.TrimSpace(req.Format)
	if format == "" {
		format = "markdown"
	}

	timeout := req.Timeout
	if timeout <= 0 {
		timeout = webFetchDefaultSecs
	}

	if timeout > webFetchMaxSecs {
		timeout = webFetchMaxSecs
	}

	maxLength := req.MaxLength
	if maxLength <= 0 {
		maxLength = webFetchDefaultMax
	}

	if maxLength > webFetchMaxLengthCap {
		maxLength = webFetchMaxLengthCap
	}

	body, err := json.Marshal(webFetchRequestBody{
		URL: url, Format: format, Prompt: strings.TrimSpace(req.Prompt),
		Timeout: timeout, MaxLength: maxLength, ModelName: strings.TrimSpace(req.ModelName),
	})
	if err != nil {
		return nil, 0, fmt.Errorf("webfetch marshal: %w", err)
	}

	return body, timeout, nil
}

// parseWebFetchResponse 校验状态/业务码/文本类型/正文，任一不满足都明确失败。
func parseWebFetchResponse(status int, raw []byte, fallbackURL string) (*WebFetchOutcome, error) {
	if status >= http.StatusBadRequest {
		return nil, &Error{
			Kind: Classify(status, string(raw)), Status: status,
			Msg: truncate(string(raw), 200), Code: businessCode(string(raw)),
		}
	}

	var parsed struct {
		Code        int             `json:"code"`
		Msg         string          `json:"msg"`
		URL         string          `json:"url"`
		ContentType string          `json:"content_type"`
		Content     json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrWebFetchBadResponse, err)
	}

	if parsed.Code != 0 {
		kind := Classify(status, string(raw))
		if kind == ErrNone {
			kind = ErrClient
		}

		return nil, &Error{
			Kind: kind, Status: status,
			Msg:  fmt.Sprintf("code=%d msg=%s", parsed.Code, truncate(parsed.Msg, 160)),
			Code: businessCode(string(raw)),
		}
	}

	if !isTextualContentType(parsed.ContentType) {
		return nil, ErrWebFetchNonText
	}

	var content string
	if err := json.Unmarshal(parsed.Content, &content); err != nil || strings.TrimSpace(content) == "" {
		return nil, ErrWebFetchEmptyContent
	}

	outURL := strings.TrimSpace(parsed.URL)
	if outURL == "" {
		outURL = strings.TrimSpace(fallbackURL)
	}

	return &WebFetchOutcome{URL: outURL, ContentType: parsed.ContentType, Content: content}, nil
}

// isTextualContentType 与官方 classifyResourceKind 同口径：image/video/audio 与
// 已知二进制类型视为非文本，其余（含空类型与 application/* 文本形态，如
// application/javascript、application/x-yaml）按文本处理。空类型按文本处理
// （上游个别响应不带该字段）。
func isTextualContentType(contentType string) bool {
	contentType = strings.ToLower(strings.TrimSpace(strings.Split(contentType, ";")[0]))
	if contentType == "" {
		return true
	}

	if strings.HasPrefix(contentType, "image/") || strings.HasPrefix(contentType, "video/") ||
		strings.HasPrefix(contentType, "audio/") {
		return false
	}

	switch contentType {
	case "application/octet-stream", "application/pdf", "application/zip",
		"application/gzip", "application/x-gzip", "application/x-tar",
		"application/x-7z-compressed", "application/x-rar-compressed", "application/vnd.rar",
		"application/vnd.openxmlformats-officedocument.wordprocessingml.document",
		"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
		"application/vnd.openxmlformats-officedocument.presentationml.presentation":
		return false
	}

	return true
}
