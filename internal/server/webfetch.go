// webfetch.go POST /v1/tools/fetch：官方 /agenttool/v1/webfetch 的 DSH 形态出口
// （issue #276）。默认关闭（features.web_fetch_endpoint）。
//
// 安全边界：
//   - 只允许 http/https，拒绝 userinfo、空 host、私网/回环/链路本地/云元数据/CGNAT
//     等特殊地址；目标域名逐 IP 校验（复用 localwebtools.go 的校验链）。
//   - 客户端只收到通用错误；provider 原文、完整 URL、token 不进入响应/日志。
//   - 上游返回的最终 URL 再校验一次（provider 侧重定向后也不允许落到内网）。
package server

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
	"github.com/BegoniaHe/wokbuddy-demon/internal/logfmt"
	"github.com/BegoniaHe/wokbuddy-demon/internal/reqlog"
	"github.com/BegoniaHe/wokbuddy-demon/internal/upstream"
)

const webFetchEndpointMaxBody = 64 << 10

// webFetchEndpointHandler /v1/tools/fetch 入口（鉴权由 withClientAuth 负责）。
func (h *Handler) webFetchEndpointHandler(w http.ResponseWriter, r *http.Request) {
	if !h.loadLive().WebFetchEndpoint {
		writeOpenAIError(w, http.StatusNotFound, "not_found",
			"fetch endpoint is disabled; enable features.web_fetch_endpoint to use POST /v1/tools/fetch")

		return
	}

	if keyRealm := keyRealmFrom(r.Context()); keyRealm != "" && keyRealm != searchRealm {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request",
			"fetch endpoint only supports the cn realm; this API key is bound to "+keyRealm)

		return
	}

	req, ok := h.readWebFetchRequest(w, r)
	if !ok {
		return
	}

	// 会话头族在轮转外构造一次：同一次抓取的换号重试必须复用同一聚合主键。
	meta := searchMetaFromRequest(r)

	started := time.Now()
	usageModel := webFetchUsageModel(&req)

	outcome, uid, fail := runAgentTool(r.Context(), h, "webfetch", max(h.cfg.MaxRotate+1, 1), true,
		func(ctx context.Context, acct *auth.Auth) (*upstream.WebFetchOutcome, error) {
			return h.cfg.Upstream.WebFetch(ctx, acct, req, meta)
		})
	if fail != nil {
		h.recordSearchUsage(r, uid, usageModel, started, nil, false)
		noteWebFetchTrace(r, h, uid, reqlog.OutcomeHTTPError)

		status, code := webFetchFailure(fail)
		writeOpenAIError(w, status, code, "upstream fetch failed")

		return
	}

	// provider 侧重定向后的最终 URL 再校验：不允许落入内网。
	if err := validateWebURL(r.Context(), outcome.URL); err != nil {
		h.recordSearchUsage(r, uid, usageModel, started, nil, false)
		log.Printf("WARN: [server] webfetch rejected final url acct=%s", logfmt.UID8(uid))
		noteWebFetchTrace(r, h, uid, reqlog.OutcomeHTTPError)
		writeOpenAIError(w, http.StatusBadGateway, "upstream_fetch_failed", "upstream fetch failed")

		return
	}

	h.recordSearchUsage(r, uid, usageModel, started, nil, true)
	noteWebFetchTrace(r, h, uid, reqlog.OutcomeSuccess)

	writeJSON(w, http.StatusOK, map[string]any{
		"title":        webFetchTitle(outcome.Content, outcome.URL),
		"content":      outcome.Content,
		"url":          outcome.URL,
		"content_type": outcome.ContentType,
	})
}

// readWebFetchRequest 读取 body 并做 SSRF 校验；失败已写出响应。
func (h *Handler) readWebFetchRequest(w http.ResponseWriter, r *http.Request) (upstream.WebFetchRequest, bool) {
	var req upstream.WebFetchRequest

	limitRequestBody(w, r, webFetchEndpointMaxBody)

	body, err := io.ReadAll(r.Body)
	if err != nil {
		if isBodyTooLarge(err) {
			writeOpenAIError(w, http.StatusRequestEntityTooLarge, "payload_too_large", "request body too large")
			return req, false
		}

		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "read body failed")

		return req, false
	}

	if json.Unmarshal(body, &req) != nil || strings.TrimSpace(req.URL) == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "url is required")

		return req, false
	}

	// SSRF：复用本地 web 工具的校验链（scheme + userinfo + 逐 IP）。
	if err := validateWebURL(r.Context(), req.URL); err != nil {
		log.Printf("WARN: [server] webfetch rejected target (ssrf policy)")
		writeOpenAIError(w, http.StatusBadRequest, "invalid_url", "invalid url")

		return req, false
	}

	return req, true
}

// noteWebFetchTrace 把抓取结果写进请求日志（trace 由 ServeHTTP 在 tracedPath 创建）。
func noteWebFetchTrace(r *http.Request, h *Handler, uid, outcome string) {
	if trace := requestTraceFrom(r); trace != nil {
		trace.noteResult(logfmt.Label(uid, accountNickname(h, uid)), "webfetch", outcome)
	}
}

// webFetchFailure 把内部失败映射成客户端通用形态：不泄露 provider 原文/URL。
func webFetchFailure(fail *searchFailure) (int, string) {
	switch fail.status {
	case http.StatusBadRequest:
		return http.StatusBadRequest, "invalid_request"
	case http.StatusTooManyRequests:
		return http.StatusTooManyRequests, "rate_limit_exceeded"
	case http.StatusForbidden:
		return http.StatusForbidden, "forbidden"
	case http.StatusServiceUnavailable:
		return http.StatusServiceUnavailable, "service_unavailable"
	default:
		return http.StatusBadGateway, "upstream_fetch_failed"
	}
}

// webFetchUsageModel 用量账本里的模型维度：优先客户端声明的 model_name，否则固定 webfetch。
func webFetchUsageModel(req *upstream.WebFetchRequest) string {
	if model := strings.TrimSpace(req.ModelName); model != "" {
		return model
	}

	return "webfetch"
}

// webFetchTitle 从 markdown 首个标题取 title；没有则回落 URL（DSH 要求 title 字符串）。
func webFetchTitle(content, url string) string {
	for line := range strings.SplitSeq(content, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "#") {
			if title := strings.TrimSpace(strings.TrimLeft(line, "#")); title != "" {
				return title
			}
		}
	}

	return url
}
