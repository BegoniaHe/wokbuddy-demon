// videos.go POST /v1/videos/generations 与 GET /v1/videos/{task_id}：官方视频端点
// 透传（issue #467）。
//
// 默认关闭（features.videos_endpoint）。开启后：
//   - 鉴权/命名密钥限额复用 withClientAuth；
//   - 模型按 realm 前缀直路由（无本域账号 fail-fast，不跨域回落）；
//   - 单次尝试，不换号（与出图同口径，避免重复提交计费）；
//   - 提交透出上游原文（`data.id` 即 task id）；查询在 completed 时透出原文
//     （含 `data[].url` 与 `data.usage.credit`），failed 归一为 502 通用文案；
//   - 网关不做轮询：官方 CLI 自己「提交后循环 poll 直到 completed」，网关按
//     无状态透传设计，与 images.go 一致。
package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
	"github.com/BegoniaHe/wokbuddy-demon/internal/session"
	"github.com/BegoniaHe/wokbuddy-demon/internal/upstream"
)

// videoGenerateHandler 视频提交入口（鉴权由 withClientAuth 负责）。
func (h *Handler) videoGenerateHandler(w http.ResponseWriter, r *http.Request) {
	h.serveVideoGenerate(w, r)
}

// videoTaskHandler 视频任务查询入口（鉴权由 withClientAuth 负责）。
func (h *Handler) videoTaskHandler(w http.ResponseWriter, r *http.Request) {
	h.serveVideoTask(w, r)
}

// videosEnabled 开关判定；关闭时 404 + 明确文案（与图像端点同口径）。
func (h *Handler) videosEnabled(w http.ResponseWriter, r *http.Request) bool {
	if h.loadLive().VideosEndpoint {
		return true
	}

	writeOpenAIError(w, http.StatusNotFound, "not_found",
		"videos endpoint is disabled; enable features.videos_endpoint to use "+r.Method+" "+r.URL.Path)

	return false
}

// serveVideoGenerate 提交主体：读请求 → 选号 → 租约 → 刷新 → 打上游。
func (h *Handler) serveVideoGenerate(w http.ResponseWriter, r *http.Request) {
	if !h.videosEnabled(w, r) {
		return
	}

	body, ok := h.readVideoBody(w, r)
	if !ok {
		return
	}

	rawModel, ok := requestBodyModel(w, body)
	if !ok {
		return
	}

	realm, bareModel, ok := h.resolveVideoModel(w, r, rawModel)
	if !ok {
		return
	}

	acct, ok := h.acquireVideoAccount(w, realm, bareModel)
	if !ok {
		return
	}
	defer h.cfg.Pool.Release(acct.UID)

	if rawModel != bareModel {
		// 与出图同一份改写逻辑（logging.go rewriteModel）：只换 model，其余字段原样。
		body = rewriteModel(body, bareModel)
	}

	h.forwardVideo(w, r, acct, body, realm, bareModel, h.cfg.Upstream.GenerateVideo, false)
}

// serveVideoTask 查询主体：task_id 来自路径，其余（选号 / 租约 / 刷新 / 单次尝试）
// 与提交同口径。
func (h *Handler) serveVideoTask(w http.ResponseWriter, r *http.Request) {
	if !h.videosEnabled(w, r) {
		return
	}

	taskID := strings.TrimSpace(r.PathValue("task_id"))
	if taskID == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "task_id is required")
		return
	}

	realm, bareModel, ok := h.videoTaskModel(w, r)
	if !ok {
		return
	}

	acct, ok := h.acquireVideoAccount(w, realm, bareModel)
	if !ok {
		return
	}
	defer h.cfg.Pool.Release(acct.UID)

	// 查询请求 body 由上游客户端内部拼 {"task_id":...}，这里不传 body。
	call := func(
		ctx context.Context, acct *auth.Auth, _ []byte, clientIP string, meta upstream.ChatMeta,
	) (int, []byte, error) {
		return h.cfg.Upstream.GetVideoTask(ctx, acct, taskID, clientIP, meta)
	}

	h.forwardVideo(w, r, acct, nil, realm, bareModel, call, true)
}

// readVideoBody 读取入站 body（视频面只接受 JSON，没有图像面的 multipart 兼容问题）。
func (h *Handler) readVideoBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
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

// resolveVideoModel 与出图同口径：别名 + realm 前缀 + 命名密钥绑定，再判
// models.hidden / models.allowed / 密钥白名单（必须在出站前收口）。
func (h *Handler) resolveVideoModel(
	w http.ResponseWriter, r *http.Request, rawModel string,
) (string, string, bool) {
	keyRealm := keyRealmFrom(r.Context())

	realm, bareModel, err := resolveKeyModel(rawModel, h.requestModelAliases(rawModel, keyRealm), keyRealm)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", err.Error())

		return "", "", false
	}

	if !h.mediaModelVisible(w, realm, bareModel) {
		return "", "", false
	}

	if keyUsage2 := keyUsageFrom(r.Context()); keyUsage2 != nil &&
		!h.cfg.Keys.ModelAllowed(keyUsage2.id, realm+":"+bareModel) {
		h.cfg.Keys.RecordRejected(keyUsage2.id)
		writeOpenAIError(w, http.StatusForbidden, "model_not_allowed", "model is not allowed for this API key")

		return "", "", false
	}

	return realm, bareModel, true
}

// videoTaskModel 查询端点的选号域。官方查询 body 只有 task_id、没有模型，故：
//   - 可选 ?model= 与提交同口径解析（含可见性 / 白名单校验），供跨域任务查询；
//   - 缺省 = 命名密钥绑定的域，否则 cn（与裸模型名默认域一致）。
func (h *Handler) videoTaskModel(w http.ResponseWriter, r *http.Request) (string, string, bool) {
	rawModel := strings.TrimSpace(r.URL.Query().Get("model"))
	if rawModel != "" {
		return h.resolveVideoModel(w, r, rawModel)
	}

	if keyRealm := keyRealmFrom(r.Context()); keyRealm != "" {
		return keyRealm, "", true
	}

	return realmCN, "", true
}

// acquireVideoAccount 选号 + 在途租约 + 临近过期刷新；失败路径已写出响应。
// 调用方负责 Release 返回的账号。
func (h *Handler) acquireVideoAccount(
	w http.ResponseWriter, realm, bareModel string,
) (*auth.Auth, bool) {
	// 号池分区（issue #491）：媒体请求只从媒体号选；分区关闭时与普通选号等价。
	acct := h.cfg.Pool.PickExcludingForRealmMedia(map[string]bool{}, bareModel, realm, true)
	if acct == nil {
		writeOpenAIErrorHint(w, http.StatusServiceUnavailable, "no_healthy_account",
			"all accounts are temporarily unavailable, please retry later", upstream.NoHealthyAccountHint())

		return nil, false
	}

	// 单账号租约：视频是低频重请求，占在途名额防止并发把池打满。
	if !h.cfg.Pool.Acquire(acct.UID) {
		writeOpenAIError(
			w, http.StatusServiceUnavailable, "no_healthy_account", "account is busy, please retry later",
		)

		return nil, false
	}

	if !h.ensureMediaToken(w, acct, "videos") {
		h.cfg.Pool.Release(acct.UID)

		return nil, false
	}

	return acct, true
}

// forwardVideo 发起上游请求，401 同号 refresh 重放一次后记账/收尾。
// isTaskQuery 为 true 时额外做任务终态归一（failed → 502 通用文案）。
//
// 记账与出图同口径：每次上游尝试各记一条用量与延迟，401 首跳不因重放成功而消失。
func (h *Handler) forwardVideo(
	w http.ResponseWriter, r *http.Request, acct *auth.Auth, body []byte, realm, bareModel string,
	call mediaCall, isTaskQuery bool,
) {
	meta := upstream.ChatMeta{
		// 与 chat/search/出图同口径：轮级聚合主键用 UUIDv7 32 hex。
		ConversationRequestID: session.NewUUIDv7Hex(),
		ConversationID:        metaConversationID(r),
	}

	firstStart := time.Now()
	first := mediaAttempt{}
	first.status, first.raw, first.err = call(r.Context(), acct, body, httpauthClientIP(r, h), meta)
	first.latencyMs = attemptLatencyMs(firstStart)

	final, retried, refreshFail := h.retryMediaAfterRefresh(r, acct, body, meta, call, first)
	if retried {
		h.recordMediaAttempt(acct, realm, bareModel, first, r)
	}

	h.recordMediaAttempt(acct, realm, bareModel, final, r)

	if h.handleMediaFailure(w, acct, bareModel, final, refreshFail, "video") {
		return
	}

	// 上游实测成功即证明账号可用，清熔断与连败计数（与 chat / 出图同口径）。
	h.cfg.Pool.NoteSuccess(acct.UID)

	if isTaskQuery && videoTaskFailed(final.raw) {
		// 终态 failed 不透出上游正文：可能含直链、调试信息或凭证片段。
		writeOpenAIError(w, http.StatusBadGateway, "upstream_error", "video generation task failed upstream")

		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(final.status)
	_, _ = w.Write(final.raw)
}

// videoTaskFailed 报告任务终态是否为 failed；其余状态（排队 / 运行中 / completed）
// 一律透传，由客户端自行决定是否继续轮询。
func videoTaskFailed(raw []byte) bool {
	var env struct {
		Data struct {
			Status string `json:"status"`
		} `json:"data"`
	}

	return json.Unmarshal(raw, &env) == nil && env.Data.Status == "failed"
}
