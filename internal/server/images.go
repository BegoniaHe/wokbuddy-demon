// images.go POST /v1/images/generations：官方出图端点透传（issue #253）。
//
// 默认关闭（features.images_endpoint）。开启后：
//   - 鉴权/命名密钥限额复用 withClientAuth；
//   - 模型按 realm 前缀直路由（无本域账号 fail-fast，不跨域回落）；
//   - 单次尝试，不换号（保守策略，避免重复出图计费）；
//   - 响应 {created,data,usage} 原样透出，usage 与 reqlog 照常记账。
package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
	"github.com/BegoniaHe/wokbuddy-demon/internal/httpauth"
	"github.com/BegoniaHe/wokbuddy-demon/internal/logfmt"
	"github.com/BegoniaHe/wokbuddy-demon/internal/pool"
	"github.com/BegoniaHe/wokbuddy-demon/internal/reqlog"
	"github.com/BegoniaHe/wokbuddy-demon/internal/session"
	"github.com/BegoniaHe/wokbuddy-demon/internal/upstream"
	"github.com/BegoniaHe/wokbuddy-demon/internal/usage"
)

// imagesGenerationsHandler 出图入口（鉴权由 withClientAuth 负责）。
func (h *Handler) imagesGenerationsHandler(w http.ResponseWriter, r *http.Request) {
	body, realm, bareModel, ok := h.readImagesRequest(w, r)
	if !ok {
		return
	}

	acct := h.cfg.Pool.PickExcludingForRealm(map[string]bool{}, bareModel, realm)
	if acct == nil {
		writeOpenAIErrorHint(w, http.StatusServiceUnavailable, "no_healthy_account",
			"all accounts are temporarily unavailable, please retry later", upstream.NoHealthyAccountHint())

		return
	}

	// 单账号租约：出图是低频重请求，占在途名额防止并发把池打满。
	if !h.cfg.Pool.Acquire(acct.UID) {
		writeOpenAIError(w, http.StatusServiceUnavailable, "no_healthy_account", "account is busy, please retry later")

		return
	}
	defer h.cfg.Pool.Release(acct.UID)

	if !h.ensureImagesToken(w, acct) {
		return
	}

	h.generateImages(w, r, acct, body, realm, bareModel)
}

// readImagesRequest 读取并校验出图请求（开关、body、model、key realm）。
func (h *Handler) readImagesRequest(w http.ResponseWriter, r *http.Request) ([]byte, string, string, bool) {
	if !h.loadLive().ImagesEndpoint {
		writeOpenAIError(w, http.StatusNotFound, "not_found",
			"images endpoint is disabled; enable features.images_endpoint to use POST /v1/images/generations")

		return nil, "", "", false
	}

	limitRequestBody(w, r, h.cfg.MaxBodyBytes)

	body, err := io.ReadAll(r.Body)
	if err != nil {
		if isBodyTooLarge(err) {
			writeOpenAIErrorHint(w, http.StatusRequestEntityTooLarge, "payload_too_large",
				bodyTooLargeMessage(h.cfg.MaxBodyBytes), "")

			return nil, "", "", false
		}

		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "read body: "+err.Error())

		return nil, "", "", false
	}

	var peek struct {
		Model string `json:"model"`
	}
	if json.Unmarshal(body, &peek) != nil || strings.TrimSpace(peek.Model) == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "model is required")

		return nil, "", "", false
	}

	realm, bareModel, _ := splitRealm(strings.TrimSpace(peek.Model))
	if keyRealm := keyRealmFrom(r.Context()); keyRealm != "" && keyRealm != realm {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request",
			"model realm "+realm+" does not match API key realm "+keyRealm)

		return nil, "", "", false
	}

	return body, realm, bareModel, true
}

// ensureImagesToken 临近过期先刷新；失败写出 502。
func (h *Handler) ensureImagesToken(w http.ResponseWriter, acct *auth.Auth) bool {
	if !acct.NeedsRefresh(h.cfg.RefreshSkew) {
		return true
	}

	if refreshErr := h.cfg.Upstream.RefreshToken(acct); refreshErr != nil {
		writeOpenAIError(w, http.StatusBadGateway, "upstream_error", "upstream token refresh failed")

		return false
	}

	_ = acct.SaveAtomicRetry()

	return true
}

// generateImages 发起上游请求，401 同号 refresh 重放一次后记账/收尾。
func (h *Handler) generateImages(
	w http.ResponseWriter, r *http.Request, acct *auth.Auth, body []byte, realm, bareModel string,
) {
	meta := upstream.ChatMeta{
		ConversationRequestID: session.NewMessageID(),
		ConversationID:        metaConversationID(r),
	}
	started := time.Now()

	status, raw, err := h.cfg.Upstream.GenerateImage(r.Context(), acct, body, httpauthClientIP(r, h), meta)

	var uerr *upstream.Error
	if errors.As(err, &uerr) && status == http.StatusUnauthorized {
		// 401 同号 refresh 重放一次；仍失败按会话失效记录。
		if refreshErr := h.cfg.Upstream.RefreshToken(acct); refreshErr == nil {
			_ = acct.SaveAtomicRetry()
			status, raw, err = h.cfg.Upstream.GenerateImage(r.Context(), acct, body, httpauthClientIP(r, h), meta)
		}
	}

	h.recordImagesAttempt(acct, realm, bareModel, raw, err, started, r)

	if err != nil {
		if !errors.As(err, &uerr) {
			writeOpenAIError(w, http.StatusBadGateway, "upstream_error", "image request failed at transport layer")

			return
		}

		h.handleImagesError(w, acct, bareModel, status, raw, uerr)

		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(raw)
}

// handleImagesError 出图错误处置：请求级错误直接透传；账号级错误按统一策略冷却，
// 但不换号（单次尝试）。
func (h *Handler) handleImagesError(
	w http.ResponseWriter, acct *auth.Auth, model string, status int, raw []byte, uerr *upstream.Error,
) {
	kind := uerr.Kind

	h.cfg.Pool.RecordTokenUsage(acct.UID, &pool.TokenUsageDelta{})

	if kind == upstream.ErrSessionDead {
		// 与 chat 同口径：连续阈值才禁用，401 本身只进入待重登计数。
		h.cfg.Pool.NoteSessionDead(acct.UID)
	} else if !failFastKind(kind) {
		h.applyErrorPolicy(acct.UID, kind, string(raw), acct.Realm(), model, uerr)
	}

	outStatus := status
	code := "upstream_error"
	msg := strings.TrimSpace(string(raw))

	switch {
	case failFastKind(kind):
		outStatus = http.StatusBadRequest
		code = "invalid_request"
	case kind == upstream.ErrSessionDead:
		outStatus = http.StatusBadGateway
	case kind == upstream.ErrSoftRate:
		outStatus = http.StatusTooManyRequests
	case kind == upstream.ErrServer, kind == upstream.ErrWafBlock, kind == upstream.ErrModelBlocked:
		outStatus = http.StatusBadGateway
	}

	if outStatus <= 0 {
		outStatus = http.StatusBadGateway
	}

	if msg == "" {
		msg = "upstream image request failed"
	}

	writeOpenAIErrorHint(w, outStatus, code, msg, h.hintOf(kind, string(raw), model, true, uerr))
}

// recordImagesAttempt 记账：reqlog 结果 + usage（响应 usage 缺失时仍记一次请求）。
func (h *Handler) recordImagesAttempt(
	acct *auth.Auth, realm, model string, raw []byte, err error, started time.Time, r *http.Request,
) {
	outcome := reqlog.OutcomeSuccess
	if err != nil {
		outcome = reqlog.OutcomeHTTPError
	}

	if trace := requestTraceFrom(r); trace != nil {
		trace.noteResult(logfmt.Label(acct.UID, acct.NicknameValue()), model, outcome)
	}

	if h.cfg.Usage == nil {
		return
	}

	delta := &usage.Delta{
		LatencyMs: attemptLatencyMs(started), HasLatency: true,
	}
	ok := err == nil

	if ok {
		var payload struct {
			Usage struct {
				PromptTokens     int64   `json:"prompt_tokens"`
				CompletionTokens int64   `json:"completion_tokens"`
				TotalTokens      int64   `json:"total_tokens"`
				Credit           float64 `json:"credit"`
			} `json:"usage"`
		}
		if json.Unmarshal(raw, &payload) == nil {
			delta.PromptTokens, delta.HasPromptTokens = payload.Usage.PromptTokens, payload.Usage.PromptTokens > 0
			delta.CompletionTokens, delta.HasCompletion = payload.Usage.CompletionTokens, payload.Usage.CompletionTokens > 0
			delta.TotalTokens, delta.HasTotal = payload.Usage.TotalTokens, payload.Usage.TotalTokens > 0
			delta.Credit, delta.HasCredit = payload.Usage.Credit, payload.Usage.Credit > 0
		}
	}

	h.cfg.Usage.Add(time.Now(), realm, acct.UID, model, delta, ok)
	h.cfg.Pool.RecordTokenUsage(acct.UID, &pool.TokenUsageDelta{
		PromptTokens: delta.PromptTokens, HasPromptTokens: delta.HasPromptTokens,
		CompletionTokens: delta.CompletionTokens, HasCompletionTokens: delta.HasCompletion,
		TotalTokens: delta.TotalTokens, HasTotalTokens: delta.HasTotal,
		LatencyMs: delta.LatencyMs, HasLatencyMs: delta.HasLatency,
	})
}

// httpauthClientIP 出图路径的来源 IP（与 chat 同口径）。
func httpauthClientIP(r *http.Request, h *Handler) string {
	return httpauth.ClientIP(r, h.cfg.TrustedProxies)
}

// metaConversationID 入站会话 ID（可空）。
func metaConversationID(r *http.Request) string {
	return strings.TrimSpace(r.Header.Get("X-Conversation-Id"))
}
