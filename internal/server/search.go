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

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
	"github.com/BegoniaHe/wokbuddy-demon/internal/logfmt"
	"github.com/BegoniaHe/wokbuddy-demon/internal/notify"
	"github.com/BegoniaHe/wokbuddy-demon/internal/pool"
	"github.com/BegoniaHe/wokbuddy-demon/internal/session"
	"github.com/BegoniaHe/wokbuddy-demon/internal/upstream"
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
		meta.ConversationRequestID = session.NewMessageID()
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
// 错误处置与 chat 同口径：`applyErrorPolicy` 决定是否罚号（限流软冷却 / 会话失效禁用 /
// 额度硬冷却等），fail-fast 类错误（内容拦截、参数错误、模型类）不再轮转。
func (h *Handler) runSearch(
	ctx context.Context, query string, maxResults int, meta *upstream.ChatMeta,
) (*upstream.SearchOutcome, string, *searchFailure) {
	tried := map[string]bool{}
	attempts := max(h.cfg.MaxRotate+1, 1)

	var lastFail *searchFailure

	for i := range attempts {
		acct := h.cfg.Pool.PickExcludingForRealm(tried, "", searchRealm)
		if acct == nil {
			if lastFail != nil {
				return nil, "", lastFail
			}

			return nil, "", &searchFailure{
				status: http.StatusServiceUnavailable,
				code:   "no_healthy_account",
				msg:    "all accounts are temporarily unavailable, please retry later",
				hint:   upstream.NoHealthyAccountHint(),
			}
		}

		tried[acct.UID] = true

		// 在途租约：搜索同样占账号名额（并发上限与 chat 共用，避免搜索把池打满）。
		if !h.cfg.Pool.Acquire(acct.UID) {
			continue
		}

		outcome, fail := h.searchOnAccount(ctx, acct, query, maxResults, meta)
		h.cfg.Pool.Release(acct.UID)

		if fail == nil {
			return outcome, acct.UID, nil
		}

		lastFail = fail
		// 请求级错误/内容拦截/队列满/鉴权终态：换号无意义，直接返回。
		if fail.code != "" {
			return nil, acct.UID, fail
		}

		if !rotateBackoff(ctx, i) {
			break
		}
	}

	if lastFail != nil {
		return nil, "", lastFail
	}

	return nil, "", &searchFailure{
		status: http.StatusServiceUnavailable,
		code:   "no_healthy_account",
		msg:    "all accounts are temporarily unavailable, please retry later",
		hint:   upstream.NoHealthyAccountHint(),
	}
}

// searchOnAccount 在指定账号上执行一次搜索：预刷新 token → 调用 → 错误策略。
// 返回的 fail 中 code 非空表示「不再轮转」的终态；code 为空表示可继续换号
// （调用方已在 applyErrorPolicy 里记过惩罚）。
func (h *Handler) searchOnAccount(
	ctx context.Context, acct *auth.Auth, query string, maxResults int, meta *upstream.ChatMeta,
) (*upstream.SearchOutcome, *searchFailure) {
	// token 临近过期 → 先刷新（失败按既有刷新语义罚号，然后换号）。
	if acct.NeedsRefresh(h.cfg.RefreshSkew) {
		if err := h.cfg.Upstream.RefreshToken(acct); err != nil {
			// 刷新失败已在 noteSearchRefreshFailure 里按 chat 同口径罚过账号；
			// 不能再走 searchFailureFor（它会再 applyErrorPolicy 一次，且对
			// session dead 直接 Disable，绕过 NoteSessionDead 的连续阈值）。
			h.noteSearchRefreshFailure(acct, err)

			if ue, ok := errors.AsType[*upstream.Error](err); ok {
				return nil, searchFailureEnvelope(ue)
			}

			// 传输层刷新失败：与 chat 一致继续换号，不当作不可重试的终态。
			return nil, &searchFailure{
				status: http.StatusBadGateway,
				msg:    "upstream token refresh failed",
			}
		}

		if err := acct.SaveAtomic(); err != nil {
			// 刷新成功但落盘失败：下次启动会用旧 token，必须暴露（与 chat 同口径）。
			log.Printf("ERR: [server] search refresh acct=%s: save auth failed: %v",
				logfmt.Label(acct.UID, acct.NicknameValue()), err)
		}
	}

	chatMeta := upstream.ChatMeta{}
	if meta != nil {
		chatMeta = *meta
	}

	outcome, err := h.cfg.Upstream.Search(ctx, acct, query, maxResults, chatMeta)
	if err == nil {
		h.cfg.Pool.NoteSuccess(acct.UID)

		return outcome, nil
	}

	// 与 chat 同口径：上游 401 先同号 refresh 再重放一次。搜索是同步请求，没有
	// chat 的 authReplay 状态机，这里在单次 attempt 内完成；重放后仍 401 才回到
	// 既有分类（可换号），绝不二次刷新循环。
	if ue, ok := errors.AsType[*upstream.Error](err); ok && ue.Status == http.StatusUnauthorized {
		return h.replaySearchAfterRefresh(ctx, acct, query, maxResults, chatMeta)
	}

	var upErr *upstream.Error
	if !errors.As(err, &upErr) {
		// 传输层故障：不罚号（与 chat 同口径），但也不再把请求放大到所有账号。
		log.Printf("WARN: [server] search acct=%s: %v", logfmt.UID8(acct.UID), err)

		return nil, &searchFailure{
			status: http.StatusBadGateway,
			code:   "upstream_unavailable",
			msg:    "upstream search failed (transport error)",
		}
	}

	return nil, h.searchFailureFor(acct.UID, upErr)
}

// replaySearchAfterRefresh 同号 refresh 后重放一次搜索（401 专用）。
//
// 刷新失败时惩罚已由 noteSearchRefreshFailure 完成，这里只返回信封、不再走
// searchFailureFor（避免同一失败被罚两次）；刷新成功但重放仍失败则按重放错误
// 重新分类（与 chat 的 401 重放后轮转同口径）。
//
//nolint:gocritic // ChatMeta 值传递与 Search 保持同一口径（公开 API 简单）。
func (h *Handler) replaySearchAfterRefresh(
	ctx context.Context, acct *auth.Auth, query string, maxResults int, chatMeta upstream.ChatMeta,
) (*upstream.SearchOutcome, *searchFailure) {
	if err := h.cfg.Upstream.RefreshToken(acct); err != nil {
		h.noteSearchRefreshFailure(acct, err)

		if ue, ok := errors.AsType[*upstream.Error](err); ok {
			return nil, searchFailureEnvelope(ue)
		}

		return nil, &searchFailure{
			status: http.StatusBadGateway,
			msg:    "upstream token refresh failed",
		}
	}

	if err := acct.SaveAtomic(); err != nil {
		log.Printf("ERR: [server] search 401 refresh acct=%s: save auth failed: %v",
			logfmt.Label(acct.UID, acct.NicknameValue()), err)
	}

	log.Printf("INFO: [server] search 401 acct=%s: refreshed token, replaying once",
		logfmt.Label(acct.UID, acct.NicknameValue()))

	outcome, err := h.cfg.Upstream.Search(ctx, acct, query, maxResults, chatMeta)
	if err == nil {
		h.cfg.Pool.NoteSuccess(acct.UID)

		return outcome, nil
	}

	var upErr *upstream.Error
	if !errors.As(err, &upErr) {
		log.Printf("WARN: [server] search replay acct=%s: %v", logfmt.UID8(acct.UID), err)

		return nil, &searchFailure{
			status: http.StatusBadGateway,
			code:   "upstream_unavailable",
			msg:    "upstream search failed (transport error)",
		}
	}

	return nil, h.searchFailureFor(acct.UID, upErr)
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

	h.applyErrorPolicy(uid, upErr.Kind, upErr.Msg, searchRealm, "", upErr)

	return fail
}

// searchFailureEnvelope 只把上游错误翻译成终端形态，不做任何账号惩罚。
// 供「惩罚已由别处完成」的路径复用（如 refresh 失败已走 noteSearchRefreshFailure）。
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

// noteSearchRefreshFailure 搜索路径的刷新失败处置：与 chat 的
// penalizeRefreshFailure 同口径（session dead 连续达阈才禁用；其余喂熔断计数）。
func (h *Handler) noteSearchRefreshFailure(acct *auth.Auth, err error) {
	if ue, ok := errors.AsType[*upstream.Error](err); ok && ue.Kind == upstream.ErrSessionDead {
		if h.cfg.Pool.NoteSessionDead(acct.UID) {
			log.Printf("WARN: [server] search refresh acct=%s: 连续 %d 次 session dead — 禁用",
				logfmt.Label(acct.UID, acct.NicknameValue()), pool.SessionDeadThreshold())
			h.notifyAccount(acct.UID, notify.EventSessionDead, "账号连续 session 失效，已禁用，需重新登录",
				map[string]any{"stage": "search_refresh"})
		}

		return
	}

	h.cfg.Pool.NoteError(acct.UID)
}

// searchTerminalStatus 把错误类别映射成「不再轮转」的状态码与 code；
// 空 code 表示可继续换号。
func searchTerminalStatus(upErr *upstream.Error) (int, string) {
	// 401 已在 searchOnAccount 内 refresh + 重放一次；仍 401 说明该账号 token
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
