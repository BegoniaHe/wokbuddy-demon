// images.go POST /v1/images/generations 与 POST /v1/images/edits：官方图像端点透传
// （出图 issue #253、图生图 issue #357）。
//
// 默认关闭（features.images_endpoint）。开启后：
//   - 鉴权/命名密钥限额复用 withClientAuth；
//   - 模型按 realm 前缀直路由（无本域账号 fail-fast，不跨域回落）；
//   - 单次尝试，不换号（保守策略，避免重复出图计费）；
//   - 响应 {created,data,usage} 原样透出，usage 与 reqlog 照常记账。
//
// 入站只接受 JSON body（即上游形态：edits 的 image 是 data URL / http URL /
// 裸 base64 字符串或数组，不是文件分片）。OpenAI 官方 multipart 形态**不在范围内**，
// 需要 multipart 的客户端请自行把文件转成 data URL 再发——这是刻意取舍，
// 不是漏实现；加 multipart 解析等于在网关里再写一遍文件上传与掩码逻辑。
package server

import (
	"context"
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

// imagesEndpointHandler 图像端点入口（鉴权由 withClientAuth 负责）。
// upstreamPath 决定打到哪个上游端点：generations 与 edits 共用同一套校验、
// 选号、记账与错误处置，差异只有上游路径与关闭时的提示文案。
func (h *Handler) imagesEndpointHandler(upstreamPath string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		h.serveImages(w, r, upstreamPath)
	}
}

// serveImages 图像端点主体：读请求 → 选号 → 租约 → 刷新 → 打上游。
func (h *Handler) serveImages(w http.ResponseWriter, r *http.Request, upstreamPath string) {
	body, realm, bareModel, prefixed, ok := h.readImagesRequest(w, r)
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
		writeOpenAIError(
			w, http.StatusServiceUnavailable, "no_healthy_account", "account is busy, please retry later",
		)

		return
	}
	defer h.cfg.Pool.Release(acct.UID)

	if !h.ensureImagesToken(w, acct) {
		return
	}

	if prefixed {
		body = rewriteImageModel(body, bareModel)
	}

	h.forwardImages(w, r, acct, body, realm, bareModel, upstreamPath)
}

// readImagesRequest 读取并校验出图请求（开关、body、model、key realm）。
//
//nolint:funlen,maintidx // 校验顺序即语义，平铺便于核对拒绝路径。
func (h *Handler) readImagesRequest(
	w http.ResponseWriter, r *http.Request,
) ([]byte, string, string, bool, bool) {
	if !h.loadLive().ImagesEndpoint {
		// 文案按入站端点区分，避免只提 generations 时让人以为 edits 没做开关。
		writeOpenAIError(w, http.StatusNotFound, "not_found",
			"images endpoint is disabled; enable features.images_endpoint to use POST "+r.URL.Path)

		return nil, "", "", false, false
	}

	limitRequestBody(w, r, h.cfg.MaxBodyBytes)

	body, err := io.ReadAll(r.Body)
	if err != nil {
		if isBodyTooLarge(err) {
			writeOpenAIErrorHint(w, http.StatusRequestEntityTooLarge, "payload_too_large",
				bodyTooLargeMessage(h.cfg.MaxBodyBytes), "")

			return nil, "", "", false, false
		}

		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "read body: "+err.Error())

		return nil, "", "", false, false
	}

	var peek struct {
		Model string `json:"model"`
	}
	if json.Unmarshal(body, &peek) != nil || strings.TrimSpace(peek.Model) == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "model is required")

		return nil, "", "", false, false
	}

	rawModel := strings.TrimSpace(peek.Model)
	keyRealm := keyRealmFrom(r.Context())

	realm, bareModel, err := resolveKeyModel(rawModel, h.requestModelAliases(rawModel, keyRealm), keyRealm)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", err.Error())

		return nil, "", "", false, false
	}

	// 与 chat/Anthropic 同口径：models.hidden 与命名密钥白名单必须在出站前判定，
	// 否则开启图像端点后可以绕过模型限制。
	if !h.imageModelVisible(w, realm, bareModel) {
		return nil, "", "", false, false
	}

	if keyUsage2 := keyUsageFrom(r.Context()); keyUsage2 != nil &&
		!h.cfg.Keys.ModelAllowed(keyUsage2.id, realm+":"+bareModel) {
		h.cfg.Keys.RecordRejected(keyUsage2.id)
		writeOpenAIError(w, http.StatusForbidden, "model_not_allowed", "model is not allowed for this API key")

		return nil, "", "", false, false
	}

	return body, realm, bareModel, rawModel != bareModel, true
}

// imageModelVisible 出图前的模型可见性：models.hidden 与 models.allowed 同口径，
// 任一拒绝即 400 model_unavailable（不打上游、不占号），且先于命名密钥白名单。
func (h *Handler) imageModelVisible(w http.ResponseWriter, realm, bareModel string) bool {
	if h.modelHidden(realm, bareModel) {
		writeOpenAIError(w, http.StatusBadRequest, "model_unavailable",
			"model is hidden by gateway config (models.hidden); pick another model")

		return false
	}

	if !h.modelAllowedByConfig(realm, bareModel) {
		writeOpenAIError(w, http.StatusBadRequest, "model_unavailable",
			"model is not in the gateway allowlist (models.allowed); pick another model")

		return false
	}

	return true
}

// rewriteImageModel 去掉 body.model 的 realm 前缀（上游只认裸名）。
func rewriteImageModel(body []byte, bareModel string) []byte {
	var obj map[string]any
	if json.Unmarshal(body, &obj) != nil || obj == nil {
		return body
	}

	obj["model"] = bareModel

	out, err := json.Marshal(obj)
	if err != nil {
		return body
	}

	return out
}

// ensureImagesToken 临近过期先刷新；失败写出 502。
func (h *Handler) ensureImagesToken(w http.ResponseWriter, acct *auth.Auth) bool {
	if !acct.NeedsRefresh(h.cfg.RefreshSkew) {
		return true
	}

	if refreshErr := h.cfg.Upstream.RefreshToken(acct); refreshErr != nil {
		// 与 chat 同口径：凭证失效连续达阈才禁用，其余只喂熔断，避免健康号被一次
		// 瞬时刷新失败踢出池。
		h.noteRefreshFailure(acct, refreshErr, "images")
		writeOpenAIError(w, http.StatusBadGateway, "upstream_error", "upstream token refresh failed")

		return false
	}

	_ = acct.SaveAtomicRetry()

	return true
}

// forwardImages 发起上游请求，401 同号 refresh 重放一次后记账/收尾。
// upstreamPath 为 /v2/images/generations 或 /v2/images/edits，两者策略完全一致。
func (h *Handler) forwardImages(
	w http.ResponseWriter, r *http.Request, acct *auth.Auth, body []byte, realm, bareModel, upstreamPath string,
) {
	meta := upstream.ChatMeta{
		// X-Conversation-Request-ID 是轮级聚合主键，与 chat/search 同口径用 UUIDv7
		// 32 hex；NewMessageID 不带版本/变体位，会被按官方正则校验的上游拒绝。
		ConversationRequestID: session.NewUUIDv7Hex(),
		ConversationID:        metaConversationID(r),
	}
	started := time.Now()

	call := h.imageCaller(upstreamPath)

	status, raw, err := call(r.Context(), acct, body, httpauthClientIP(r, h), meta)

	var (
		uerr        *upstream.Error
		refreshFail error
	)
	if errors.As(err, &uerr) && status == http.StatusUnauthorized {
		// 401 同号 refresh 重放一次；refresh 失败时按 refresh 错误分类，不能拿原始
		// 401 记会话失效——网络/上游 5xx 会让健康号被连续三次失败误禁用。
		refreshFail = h.cfg.Upstream.RefreshToken(acct)
		if refreshFail == nil {
			_ = acct.SaveAtomicRetry()
			status, raw, err = call(r.Context(), acct, body, httpauthClientIP(r, h), meta)
		}
	}

	h.recordImagesAttempt(acct, realm, bareModel, raw, err, started, r)

	if err != nil {
		if refreshFail != nil && !upstream.IsCredentialDead(refreshFail) {
			// 与 chat 的 penalizeRefreshFailure 非会话失效分支同口径：只喂熔断计数，
			// 不累计 session dead。
			h.cfg.Pool.NoteError(acct.UID)
			writeOpenAIError(w, http.StatusBadGateway, "upstream_error", "upstream token refresh failed")

			return
		}

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

// imageCaller 按上游路径选出图 / 图生图入口：两者签名相同，只有上游路径不同。
func (h *Handler) imageCaller(upstreamPath string) func(
	context.Context, *auth.Auth, []byte, string, upstream.ChatMeta,
) (int, []byte, error) {
	if upstreamPath == upstream.ImageEditsPath {
		return h.cfg.Upstream.EditImage
	}

	return h.cfg.Upstream.GenerateImage
}

// handleImagesError 出图错误处置：请求级错误直接透传；账号级错误按统一策略冷却，
// 但不换号（单次尝试）。
func (h *Handler) handleImagesError(
	w http.ResponseWriter, acct *auth.Auth, model string, status int, raw []byte, uerr *upstream.Error,
) {
	kind := uerr.Kind

	// 尝试与延迟已由 recordImagesAttempt 记账；这里不再补空 delta，避免同一次失败
	// 被记成两次请求。
	if kind == upstream.ErrSessionDead {
		// 与 chat 同口径：连续阈值才禁用，401 本身只进入待重登计数。
		h.cfg.Pool.NoteSessionDead(acct.UID)
	} else if !failFastKind(kind) {
		h.applyErrorPolicy(acct.UID, kind, string(raw), acct.Realm(), model, uerr)
	}

	outStatus := status
	code := "upstream_error"
	// 与 chat 同口径：只透出 upstream.Error 已截断的 Msg（200 字符），避免把
	// 上游可能很大的错误正文（含 HTML/调试信息）整段回给客户端。
	msg := strings.TrimSpace(uerr.Msg)

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

	// 上游 200 + 非零业务码也会走到这里：错误响应不能继续用 2xx 状态，否则
	// OpenAI 兼容客户端会当成成功去解析 data 字段。
	if outStatus < http.StatusBadRequest {
		outStatus = http.StatusBadGateway
	}

	if msg == "" {
		msg = "upstream image request failed"
	}

	writeOpenAIErrorHint(w, outStatus, code, msg, h.hintOf(kind, string(raw), model, true, uerr))
}

// recordImagesAttempt 记账：reqlog 结果 + usage（响应 usage 缺失时仍记一次请求）。
//
//nolint:funlen // 记账分支按结果形态线性展开。
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
			delta.CompletionTokens, delta.HasCompletion =
				payload.Usage.CompletionTokens, payload.Usage.CompletionTokens > 0
			delta.TotalTokens, delta.HasTotal = payload.Usage.TotalTokens, payload.Usage.TotalTokens > 0
			delta.Credit, delta.HasCredit = payload.Usage.Credit, payload.Usage.Credit > 0
		}
	}

	// 命名密钥用量回填：与 chat / anthropic / gemini / responses 同口径，
	// 必须在 withClientAuth 落账前把本次 token 计入该密钥（否则只记请求数）。
	if ku := keyUsageFrom(r.Context()); ku != nil {
		ku.prompt += delta.PromptTokens
		ku.completion += delta.CompletionTokens
		ku.total += delta.TotalTokens
	}

	if h.cfg.Usage == nil {
		return
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
