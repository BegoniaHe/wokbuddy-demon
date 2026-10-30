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
	"fmt"
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
	body, realm, bareModel, prefixed, ok := h.readImagesRequest(w, r, upstreamPath)
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
		// 与 chat 同一份改写逻辑（logging.go rewriteModel）：只换 model，其余字段原样。
		body = rewriteModel(body, bareModel)
	}

	h.forwardImages(w, r, acct, body, realm, bareModel, upstreamPath)
}

// readImagesRequest 读取并校验出图请求（开关、body、model、key realm）。
// 校验顺序即语义，平铺便于核对拒绝路径。
func (h *Handler) readImagesRequest(
	w http.ResponseWriter, r *http.Request, upstreamPath string,
) ([]byte, string, string, bool, bool) {
	if !h.loadLive().ImagesEndpoint {
		// 文案按入站端点区分，避免只提 generations 时让人以为 edits 没做开关。
		writeOpenAIError(w, http.StatusNotFound, "not_found",
			"images endpoint is disabled; enable features.images_endpoint to use POST "+r.URL.Path)

		return nil, "", "", false, false
	}

	body, ok := h.readImagesBody(w, r, upstreamPath)
	if !ok {
		return nil, "", "", false, false
	}

	rawModel, ok := imageRequestModel(w, body)
	if !ok {
		return nil, "", "", false, false
	}

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

// readImagesBody 读取并校验入站 body 形态与大小。
//
// multipart 判定必须在读 body 之前：OpenAI 官方 edits 形态就是 multipart，先
// ReadAll 会让大文件上传先吃满 server.max_body_bytes 再回 413，把「形态不支持」
// 误报成「请求体过大」，客户端会去调大上限而不是换形态。
func (h *Handler) readImagesBody(
	w http.ResponseWriter, r *http.Request, upstreamPath string,
) ([]byte, bool) {
	if strings.HasPrefix(strings.ToLower(r.Header.Get("Content-Type")), "multipart/form-data") {
		writeMultipartUnsupported(w, upstreamPath)

		return nil, false
	}

	limitRequestBody(w, r, h.cfg.MaxBodyBytes)

	body, err := io.ReadAll(r.Body)
	if err != nil {
		if isBodyTooLarge(err) {
			writeOpenAIErrorHint(w, http.StatusRequestEntityTooLarge, "payload_too_large",
				bodyTooLargeMessage(h.cfg.MaxBodyBytes), "")

			return nil, false
		}

		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "read body: "+err.Error())

		return nil, false
	}

	return body, true
}

// writeMultipartUnsupported 对 OpenAI 官方 multipart 形态给出明确 400。
// 不这么写时，multipart body 解析失败会落成 "model is required"，把「形态不支持」
// 误报成「缺字段」，客户端会反复补 model 而不是换形态。
//
// 补充说明按端点分化：只有 edits 有 `image` 字段可承载文件内容，generations 没有，
// 对它说「把图放进 image 字段」是错的引导。
func writeMultipartUnsupported(w http.ResponseWriter, upstreamPath string) {
	hint := "send a JSON body (`model` + `prompt`) instead of multipart form data"
	if upstreamPath == upstream.ImageEditsPath {
		hint = "put the image in the JSON `image` field as a data URL, http(s) URL or raw base64 " +
			"string (array also allowed) instead of uploading a file part"
	}

	writeOpenAIErrorHint(w, http.StatusBadRequest, "invalid_request",
		"multipart/form-data is not supported; send a JSON body instead", hint)
}

// imageRequestModel 从入站 JSON body 取 model；字段缺失或不是 JSON 时写出 400。
// multipart 已由调用方在读 body 前拒绝（见 readImagesRequest）。
func imageRequestModel(w http.ResponseWriter, body []byte) (string, bool) {
	var peek struct {
		Model string `json:"model"`
	}
	if json.Unmarshal(body, &peek) != nil || strings.TrimSpace(peek.Model) == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "model is required")

		return "", false
	}

	return strings.TrimSpace(peek.Model), true
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
//
// 记账与 chat 同口径：每次上游尝试各记一条用量与延迟，401 首跳不因重放成功而
// 消失（issue #424）；token / credit 只取自拿到响应体的那一跳。
func (h *Handler) forwardImages(
	w http.ResponseWriter, r *http.Request, acct *auth.Auth, body []byte, realm, bareModel, upstreamPath string,
) {
	meta := upstream.ChatMeta{
		// X-Conversation-Request-ID 是轮级聚合主键，与 chat/search 同口径用 UUIDv7
		// 32 hex；NewMessageID 不带版本/变体位，会被按官方正则校验的上游拒绝。
		ConversationRequestID: session.NewUUIDv7Hex(),
		ConversationID:        metaConversationID(r),
	}

	call := h.imageCaller(upstreamPath)

	firstStart := time.Now()
	first := imagesAttempt{}
	first.status, first.raw, first.err = call(r.Context(), acct, body, httpauthClientIP(r, h), meta)
	first.latencyMs = attemptLatencyMs(firstStart)

	final, retried, refreshFail := h.retryImagesAfterRefresh(r, acct, body, meta, call, first)
	if retried {
		// 首跳（401）单独留痕：与 chat 每次尝试都记一次用量同口径（issue #424）。
		h.recordImagesAttempt(acct, realm, bareModel, first, r)
	}

	h.recordImagesAttempt(acct, realm, bareModel, final, r)

	if final.err != nil {
		if refreshFail != nil && !upstream.IsCredentialDead(refreshFail) {
			// 与 chat 的 penalizeRefreshFailure 非会话失效分支同口径：只喂熔断计数，
			// 不累计 session dead。
			h.cfg.Pool.NoteError(acct.UID)
			writeOpenAIError(w, http.StatusBadGateway, "upstream_error", "upstream token refresh failed")

			return
		}

		var uerr *upstream.Error
		if !errors.As(final.err, &uerr) {
			writeOpenAIError(w, http.StatusBadGateway, "upstream_error", "image request failed at transport layer")

			return
		}

		h.handleImagesError(w, acct, bareModel, final.status, final.raw, uerr)

		return
	}

	// 与 chat / search 同口径：出图实测成功即证明账号可用，清熔断与连败计数
	// （只记失败不记成功会让恢复的账号一直挂着「连败中」）。
	h.cfg.Pool.NoteSuccess(acct.UID)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(final.status)
	_, _ = w.Write(final.raw)
}

// imagesAttempt 一次出图上游尝试的结果与本跳耗时。
// 耗时必须在**该跳返回时**采样：首跳的记账发生在 refresh + 重放之后，
// 若此时才用起始时刻计算，会把刷新与重放耗时重复算进首跳（issue #424）。
type imagesAttempt struct {
	status    int
	raw       []byte
	err       error
	latencyMs int64
}

// retryImagesAfterRefresh 上游 401 时同号 refresh 后重放一次。
//
// 返回 (最终结果, 是否发生过重放, refresh 错误)：refresh 失败时保持原始 401 结果
// 且不算重放（首跳即终态，只记一次）。调用方据此给首跳单独留痕（issue #424）。
//
// 不能拿原始 401 记会话失效：refresh 失败可能只是网络 / 上游 5xx，健康号会被连续
// 三次失败误禁用。非 401 或非 *upstream.Error 原样返回。
//
//nolint:gocritic // ChatMeta 值传递与 upstream.ChatHeaders 同一 API。
func (h *Handler) retryImagesAfterRefresh(
	r *http.Request, acct *auth.Auth, body []byte, meta upstream.ChatMeta, call imagesCall, first imagesAttempt,
) (imagesAttempt, bool, error) {
	var uerr *upstream.Error
	if !errors.As(first.err, &uerr) || first.status != http.StatusUnauthorized {
		return first, false, nil
	}

	refreshFail := h.cfg.Upstream.RefreshToken(acct)
	if refreshFail != nil {
		return first, false, fmt.Errorf("refresh token: %w", refreshFail)
	}

	_ = acct.SaveAtomicRetry()

	secondStart := time.Now()
	second := imagesAttempt{}
	second.status, second.raw, second.err = call(r.Context(), acct, body, httpauthClientIP(r, h), meta)
	second.latencyMs = attemptLatencyMs(secondStart)

	return second, true, nil
}

// imagesCall 一次图像上游尝试的签名（出图与图生图一致）。
type imagesCall func(context.Context, *auth.Auth, []byte, string, upstream.ChatMeta) (int, []byte, error)

// imageCaller 按上游路径选出图 / 图生图入口：两者签名相同，只有上游路径不同。
func (h *Handler) imageCaller(upstreamPath string) imagesCall {
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

	writeOpenAIErrorHint(w, outStatus, code, msg, h.hintOf(kind, string(raw), model, true, false, uerr))
}

// recordImagesAttempt 记一次上游尝试的账：reqlog 结果 + usage（响应 usage 缺失时
// 仍记一次请求，与 chat 的失败尝试同口径）。多次尝试（401 重放）各自调用一次，
// 延迟各跳独立（issue #424）。
//
//nolint:funlen // 记账分支按结果形态线性展开。
func (h *Handler) recordImagesAttempt(
	acct *auth.Auth, realm, model string, attempt imagesAttempt, r *http.Request,
) {
	outcome := reqlog.OutcomeSuccess
	if attempt.err != nil {
		outcome = reqlog.OutcomeHTTPError
	}

	if trace := requestTraceFrom(r); trace != nil {
		trace.noteResult(logfmt.Label(acct.UID, acct.NicknameValue()), model, outcome)
	}

	delta := &usage.Delta{
		LatencyMs: attempt.latencyMs, HasLatency: true,
	}
	ok := attempt.err == nil

	if ok {
		var payload struct {
			Usage struct {
				PromptTokens     int64   `json:"prompt_tokens"`
				CompletionTokens int64   `json:"completion_tokens"`
				TotalTokens      int64   `json:"total_tokens"`
				Credit           float64 `json:"credit"`
			} `json:"usage"`
		}
		if json.Unmarshal(attempt.raw, &payload) == nil {
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
