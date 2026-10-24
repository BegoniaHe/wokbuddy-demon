// search.go /v1/search/messages 的搜索执行流程（选号 → 在途租约 → 预刷新 →
// 上游搜索 → 错误策略 → 轮转）。
//
// 与 chat 的区别：搜索是一次同步 JSON 请求，没有 SSE；入站会话头族仍透传给上游
// （见 searchMetaFromRequest），但不套 chatFlow（那套状态机的复杂度都在流与模型
// 上下文上）。这里只保留必要的三段：选号 + 账号级错误策略 + 有界轮转。
package server

import (
	"context"
	"errors"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
	"github.com/BegoniaHe/wokbuddy-demon/internal/logfmt"
	"github.com/BegoniaHe/wokbuddy-demon/internal/notify"
	"github.com/BegoniaHe/wokbuddy-demon/internal/pool"
	"github.com/BegoniaHe/wokbuddy-demon/internal/session"
	"github.com/BegoniaHe/wokbuddy-demon/internal/upstream"
	"github.com/BegoniaHe/wokbuddy-demon/internal/usage"
)

// searchRealm 联网搜索只放行 CN 域选号：上游搜索端点仅在 CN 域实测可用。
// 两域放开留 TODO（需先补 global 域实测，不能靠推断）。
const searchRealm = "cn"

// searchMetaFromRequest 提取入站搜索请求的会话头族。
//
// 搜索通常由某个会话内的 agent 发起：X-Conversation-ID / X-Conversation-Request-ID /
// X-Trace-ID 必须原样透传给上游（上游按会话聚合搜索流量，缺失会被视为请求不完整）。
// 客户端没带 request ID 时本地生成一个——同一次搜索的换号重试必须复用同一个聚合主键
// （meta 在 runSearch 外构造一次），不能每轮随机。
func searchMetaFromRequest(r *http.Request) upstream.ChatMeta {
	meta := upstream.ChatMeta{
		ConversationID:        strings.TrimSpace(r.Header.Get("X-Conversation-Id")),
		ConversationRequestID: strings.TrimSpace(r.Header.Get("X-Conversation-Request-Id")),
		TraceID:               strings.TrimSpace(r.Header.Get("X-Trace-Id")),
	}
	if meta.ConversationRequestID == "" {
		meta.ConversationRequestID = session.NewUUIDv7Hex()
	}

	return meta
}

// searchFailure 搜索失败的终端形态（status / code / message / hint）。
type searchFailure struct {
	status int
	code   string
	msg    string
	hint   string
}

// runSearch 执行一次联网搜索（内部有界轮转），并返回最终服务该请求的账号 UID
// （供请求日志记账：搜索路径没有 chatStat，账号/模型要显式写进 trace）。
//
// 公共账号执行逻辑见 runAgentTool；snippet 搜索保持既有语义：传输层故障不换号。
func (h *Handler) runSearch(
	ctx context.Context, query string, opts upstream.SearchOptions, meta *upstream.ChatMeta,
) (*upstream.SearchOutcome, string, *searchFailure) {
	chatMeta := upstream.ChatMeta{}
	if meta != nil {
		chatMeta = *meta
	}

	return runAgentTool(ctx, h, "search", max(h.cfg.MaxRotate+1, 1), false,
		func(ctx context.Context, acct *auth.Auth) (*upstream.SearchOutcome, error) {
			return h.cfg.Upstream.Search(ctx, acct, query, opts, chatMeta)
		})
}

// runAgenticSearch 执行一次综述式深检索：单次尝试（不随 MaxRotate 放大 120s
// 超时），仍走同一套 Acquire/Release 与错误处置。
func (h *Handler) runAgenticSearch(
	ctx context.Context, query string, meta *upstream.ChatMeta,
) (*upstream.AgenticOutcome, string, *searchFailure) {
	chatMeta := upstream.ChatMeta{}
	if meta != nil {
		chatMeta = *meta
	}

	return runAgentTool(ctx, h, "search_deep", 1, false,
		func(ctx context.Context, acct *auth.Auth) (*upstream.AgenticOutcome, error) {
			bizVia := "public"
			if acct.IsEnterprise() {
				bizVia = "internal"
			}

			return h.cfg.Upstream.AgenticSearch(ctx, acct, query, bizVia, chatMeta)
		})
}

// recordSearchUsage 把搜索/深检索归档进用量账本：请求数 + 延迟恒记，token 只在
// 上游返回 usage 时记（snippet 搜索没有 token 维度）。不记录 query 原文。
//
// 命名密钥的 token 回填与出图路径同口径：深检索消耗的 token 也必须计入该密钥
// 的用量与 TPM，否则按密钥限流会被绕过。
func (h *Handler) recordSearchUsage(
	r *http.Request, uid, model string, started time.Time, agenticUsage *upstream.AgenticUsage, ok bool,
) {
	delta := &usage.Delta{LatencyMs: attemptLatencyMs(started), HasLatency: true}
	if agenticUsage != nil {
		delta.PromptTokens, delta.HasPromptTokens = agenticUsage.PromptTokens, agenticUsage.PromptTokens > 0
		delta.CompletionTokens, delta.HasCompletion = agenticUsage.CompletionTokens, agenticUsage.CompletionTokens > 0
		delta.TotalTokens, delta.HasTotal = agenticUsage.TotalTokens, agenticUsage.TotalTokens > 0

		if !delta.HasTotal && (delta.HasPromptTokens || delta.HasCompletion) {
			delta.TotalTokens = delta.PromptTokens + delta.CompletionTokens
			delta.HasTotal = true
		}
	}

	if ku := keyUsageFrom(r.Context()); ku != nil {
		ku.prompt += delta.PromptTokens
		ku.completion += delta.CompletionTokens
		ku.total += delta.TotalTokens
	}

	if h.cfg.Usage == nil || uid == "" {
		return
	}

	h.cfg.Usage.Add(time.Now(), searchRealm, uid, model, delta, ok)
}

// runAgentTool 是搜索 / webfetch / agentic 搜索共用的账号执行器：选号、在途租约、
// 预刷新、401 同号 refresh 重放一次、错误处置与有界轮转。
//
// rotateTransport=false 时传输层故障直接作为终态（与既有搜索一致）；
// true 时按「超时/5xx 有界换号」处理（webfetch）。
//
// 不接收 model：snippet/深检索请求体不含 model，webfetch 的 model_name 也不是
// 网关池里的聊天模型；把客户端模型名交给 applyErrorPolicy 会误写模型级冷却/路由熔断。
func runAgentTool[T any](
	ctx context.Context, handler *Handler, stage string, attempts int, rotateTransport bool,
	call func(context.Context, *auth.Auth) (T, error),
) (T, string, *searchFailure) {
	var zero T

	if attempts < 1 {
		attempts = 1
	}

	tried := map[string]bool{}

	var lastFail *searchFailure

	for i := range attempts {
		acct := handler.cfg.Pool.PickExcludingForRealm(tried, "", searchRealm)
		if acct == nil {
			if lastFail != nil {
				return zero, "", lastFail
			}

			return zero, "", noHealthyAccountFailure()
		}

		tried[acct.UID] = true

		// 在途租约：搜索/抓取同样占账号名额（与 chat 共用，避免把池打满）。
		if !handler.cfg.Pool.Acquire(acct.UID) {
			continue
		}

		out, fail := handler.agentToolOnAccount(ctx, stage, acct, rotateTransport, call)
		handler.cfg.Pool.Release(acct.UID)

		if fail == nil {
			return out, acct.UID, nil
		}

		lastFail = fail
		// 请求级错误/内容拦截/队列满/鉴权终态：换号无意义，直接返回。
		if fail.code != "" {
			return zero, acct.UID, fail
		}

		// 没有下一次尝试时不必再等退避（单次尝试的深检索/末号失败会白加延迟）。
		if !rotateAfterFailure(ctx, i, attempts) {
			break
		}
	}

	if lastFail != nil {
		return zero, "", lastFail
	}

	return zero, "", noHealthyAccountFailure()
}

// rotateAfterFailure 在第 i 次尝试失败后等待下一次尝试；已是最后一次时直接
// 返回 false（避免为不存在的下一次白睡退避）。
func rotateAfterFailure(ctx context.Context, i, attempts int) bool {
	if i+1 >= attempts {
		return false
	}

	return rotateBackoff(ctx, i)
}

// noHealthyAccountFailure 搜索/抓取路径的池耗尽终端形态。
func noHealthyAccountFailure() *searchFailure {
	return &searchFailure{
		status: http.StatusServiceUnavailable,
		code:   "no_healthy_account",
		msg:    "all accounts are temporarily unavailable, please retry later",
		hint:   upstream.NoHealthyAccountHint(),
	}
}

// agentToolOnAccount 在指定账号上执行一次 agenttool 调用：预刷新 token → 调用 →
// 错误策略。返回的 fail 中 code 非空表示「不再轮转」的终态；code 为空表示可继续
// 换号（调用方已在 applyErrorPolicy 里记过惩罚）。
func (h *Handler) agentToolOnAccount[T any](
	ctx context.Context, stage string, acct *auth.Auth, rotateTransport bool,
	call func(context.Context, *auth.Auth) (T, error),
) (T, *searchFailure) {
	var zero T

	refreshStage := stage + "_refresh"

	// token 临近过期 → 先刷新（失败按既有刷新语义罚号，然后换号）。
	if acct.NeedsRefresh(h.cfg.RefreshSkew) {
		if err := h.cfg.Upstream.RefreshToken(acct); err != nil {
			h.noteRefreshFailure(acct, err, refreshStage)

			if ue, ok := errors.AsType[*upstream.Error](err); ok {
				return zero, searchFailureEnvelope(ue)
			}

			return zero, &searchFailure{status: http.StatusBadGateway, msg: "upstream token refresh failed"}
		}

		if err := acct.SaveAtomicRetry(); err != nil {
			log.Printf("ERR: [server] %s refresh acct=%s: save auth failed: %v",
				stage, logfmt.Label(acct.UID, acct.NicknameValue()), err)
		}
	}

	out, err := call(ctx, acct)
	if err == nil {
		h.cfg.Pool.NoteSuccess(acct.UID)

		return out, nil
	}

	// 与 chat 同口径：上游 401 先同号 refresh 再重放一次，绝不二次刷新循环。
	if ue, ok := errors.AsType[*upstream.Error](err); ok && ue.Status == http.StatusUnauthorized {
		return h.agentToolReplayAfterRefresh(ctx, stage, acct, rotateTransport, call)
	}

	return zero, h.agentToolErrorFailure(stage, acct.UID, rotateTransport, err)
}

// agentToolErrorFailure 统一处置调用后的错误（确定性失败 / 传输层 / 上游分类）。
func (h *Handler) agentToolErrorFailure(
	stage, uid string, rotateTransport bool, err error,
) *searchFailure {
	// 确定性上游结果（缺正文/非文本/超限/无综述）：换号不会改变，直接终态。
	if agentToolDeterministicError(err) {
		return deterministicAgentToolFailure(stage)
	}

	var upErr *upstream.Error
	if !errors.As(err, &upErr) {
		// 传输层故障：只保留脱敏后的错误（可能含代理凭证/内部 URL），便于排障。
		log.Printf("WARN: [server] %s acct=%s: transport error: %s",
			stage, logfmt.UID8(uid), upstream.RedactForLog(err.Error(), 200))

		if rotateTransport {
			return &searchFailure{status: http.StatusBadGateway, msg: "upstream " + stage + " failed (transport error)"}
		}

		return &searchFailure{
			status: http.StatusBadGateway,
			code:   "upstream_unavailable",
			msg:    "upstream " + stage + " failed (transport error)",
		}
	}

	return h.searchFailureFor(uid, upErr)
}

// agentToolReplayAfterRefresh 同号 refresh 后重放一次（401 专用）。
//
// 刷新失败时惩罚已由 noteRefreshFailure 完成，这里只返回信封、不再走
// searchFailureFor（避免同一失败被罚两次）；刷新成功但重放仍失败则按重放错误
// 重新分类（与 chat 的 401 重放后轮转同口径）。
func (h *Handler) agentToolReplayAfterRefresh[T any](
	ctx context.Context, stage string, acct *auth.Auth, rotateTransport bool,
	call func(context.Context, *auth.Auth) (T, error),
) (T, *searchFailure) {
	var zero T

	if err := h.cfg.Upstream.RefreshToken(acct); err != nil {
		h.noteRefreshFailure(acct, err, stage+"_refresh")

		if ue, ok := errors.AsType[*upstream.Error](err); ok {
			return zero, searchFailureEnvelope(ue)
		}

		return zero, &searchFailure{status: http.StatusBadGateway, msg: "upstream token refresh failed"}
	}

	if err := acct.SaveAtomicRetry(); err != nil {
		log.Printf("ERR: [server] %s 401 refresh acct=%s: save auth failed: %v",
			stage, logfmt.Label(acct.UID, acct.NicknameValue()), err)
	}

	log.Printf("INFO: [server] %s 401 acct=%s: refreshed token, replaying once",
		stage, logfmt.Label(acct.UID, acct.NicknameValue()))

	out, err := call(ctx, acct)
	if err == nil {
		h.cfg.Pool.NoteSuccess(acct.UID)

		return out, nil
	}

	// 重放后仍是确定性失败（缺正文/非文本/超限/无综述）：与首次失败同口径直接
	// 终态，不能当成传输层错误去换号（换号只会重复同一份响应）。
	if agentToolDeterministicError(err) {
		return zero, deterministicAgentToolFailure(stage)
	}

	var upErr *upstream.Error
	if !errors.As(err, &upErr) {
		log.Printf("WARN: [server] %s replay acct=%s: transport error: %s",
			stage, logfmt.UID8(acct.UID), upstream.RedactForLog(err.Error(), 200))

		if rotateTransport {
			return zero, &searchFailure{status: http.StatusBadGateway, msg: "upstream " + stage + " failed (transport error)"}
		}

		return zero, &searchFailure{
			status: http.StatusBadGateway,
			code:   "upstream_unavailable",
			msg:    "upstream " + stage + " failed (transport error)",
		}
	}

	return zero, h.searchFailureFor(acct.UID, upErr)
}

// deterministicAgentToolFailure 确定性失败的终端形态：带 code 让调用方停止轮转。
func deterministicAgentToolFailure(stage string) *searchFailure {
	return &searchFailure{
		status: http.StatusBadGateway,
		code:   "upstream_" + stage + "_failed",
		msg:    "upstream " + stage + " failed",
	}
}

// agentToolDeterministicError 报告不随账号变化的确定性失败：缺正文/非文本/
// 超限/坏响应/无综述。换号只会重复失败，必须直接终态。
func agentToolDeterministicError(err error) bool {
	return errors.Is(err, upstream.ErrWebFetchEmptyContent) ||
		errors.Is(err, upstream.ErrWebFetchNonText) ||
		errors.Is(err, upstream.ErrWebFetchTooLarge) ||
		errors.Is(err, upstream.ErrWebFetchBadResponse) ||
		errors.Is(err, upstream.ErrAgenticSearchNoSynthesis) ||
		errors.Is(err, upstream.ErrAgenticSearchTooLarge)
}

// searchFailureFor 按上游错误分类决定处置与终端形态：
//   - 走既有 applyErrorPolicy（与 chat 同一套惩罚口径，不新增第二套）；
//   - 请求级/内容/队列类不可自愈 → 返回带 code 的终态（不再轮转）；
//   - 其余（限流/会话/额度/账号故障/5xx）→ code 为空，让调用方换号；但**状态码与
//     原文照样填好**——轮转耗尽时直接把这一份透给客户端，留零值会走到
//     WriteHeader(0) 直接 panic（客户端只看到空 200）。
func (h *Handler) searchFailureFor(uid string, upErr *upstream.Error) *searchFailure {
	fail := searchFailureEnvelope(upErr)
	// 终态错误（请求体/内容/模型/队列）是请求或通道本身的问题：换号与账号冷却都
	// 无意义，不能因为一次坏请求给账号记失败（尤其 ErrClient 被 chat 口径当作
	// 未知 4xx 喂连败计数）。
	if fail.code != "" {
		return fail
	}

	// 搜索/抓取请求体里的 model 信息不是网关聊天模型：只做账号级处置，
	// 不能把错误写到客户端模型上（模型级冷却/路由熔断会误伤后续 chat）。
	h.applyErrorPolicy(uid, upErr.Kind, upErr.Msg, searchRealm, "", upErr)

	return fail
}

// searchFailureEnvelope 只把上游错误翻译成终端形态，不做任何账号惩罚。
// 供「惩罚已由别处完成」的路径复用（如 refresh 失败已走 noteRefreshFailure）。
func searchFailureEnvelope(upErr *upstream.Error) *searchFailure {
	status, code := searchTerminalStatus(upErr)
	if code == "" {
		status = searchRetryableStatus(upErr.Kind)
	}

	msg := upErr.Msg
	if msg == "" {
		msg = "upstream search failed"
	}

	return &searchFailure{
		status: status,
		code:   code,
		msg:    msg,
		hint:   upstream.GatewayHint(upErr.Kind, upErr.Msg, upstream.HintContext{}),
	}
}

// searchRetryableStatus 可换号类别的终端状态码（轮转耗尽时透传给客户端）：
// 限流/额度按 429（客户端应退避后重试），账号级/上游级故障按 503。
func searchRetryableStatus(kind upstream.ErrKind) int {
	switch kind {
	case upstream.ErrSoftRate, upstream.ErrHardCredit:
		return http.StatusTooManyRequests
	case upstream.ErrWafBlock:
		return http.StatusForbidden
	default:
		return http.StatusServiceUnavailable
	}
}

// noteRefreshFailure 非 chat 入口（搜索/出图）的刷新失败处置：与 chat 的
// penalizeRefreshFailure 同口径（session dead 连续达阈才禁用；其余喂熔断计数）。
// 判定用 IsCredentialDead 覆盖 refresh 侧 401/403、12153 与无 RT 的错误。
func (h *Handler) noteRefreshFailure(acct *auth.Auth, err error, stage string) {
	if upstream.IsCredentialDead(err) {
		if h.cfg.Pool.NoteSessionDead(acct.UID) {
			log.Printf("WARN: [server] %s refresh acct=%s: 连续 %d 次 session dead — 禁用",
				stage, logfmt.Label(acct.UID, acct.NicknameValue()), pool.SessionDeadThreshold())
			h.notifyAccount(acct.UID, notify.EventSessionDead, "账号连续 session 失效，已禁用，需重新登录",
				map[string]any{"stage": stage})
		}

		return
	}

	h.cfg.Pool.NoteError(acct.UID)
}

// searchTerminalStatus 把错误类别映射成「不再轮转」的状态码与 code；
// 空 code 表示可继续换号。
func searchTerminalStatus(upErr *upstream.Error) (int, string) {
	// 401 已在 agentToolOnAccount 内 refresh + 重放一次；仍 401 说明该账号 token
	// 不可恢复，按账号级故障换号（与 chat 的 401 重放后轮转同口径），不要降级成
	// 请求级 400 把可换号的故障说成客户端问题。
	if upErr.Status == http.StatusUnauthorized {
		return 0, ""
	}

	switch upErr.Kind {
	case upstream.ErrBadParams:
		return http.StatusBadRequest, "bad_params"
	case upstream.ErrContentBlocked:
		return http.StatusBadRequest, "content_blocked"
	case upstream.ErrModelBlocked, upstream.ErrModelAccess, upstream.ErrModelNotFound:
		return http.StatusBadRequest, "model_unavailable"
	case upstream.ErrClient:
		// 未知 4xx / 业务码（如 15003 unsupported search type）：同一请求换号必然
		// 同样失败，不能当可重试的 503 逐号轮转。
		return http.StatusBadRequest, "bad_params"
	case upstream.ErrQueueWaiting:
		return http.StatusTooManyRequests, "queue_waiting"
	case upstream.ErrQueueFull:
		return http.StatusServiceUnavailable, "queue_full"
	default:
		return 0, ""
	}
}
