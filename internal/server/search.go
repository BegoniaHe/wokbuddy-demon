// search.go /v1/search/messages 的搜索执行流程（选号 → 在途租约 → 预刷新 →
// 上游搜索 → 错误策略 → 轮转）。
//
// 与 chat 的区别：搜索是一次同步 JSON 请求，没有 SSE、没有模型/会话上下文，
// 因此不套 chatFlow（那套状态机的复杂度都在流与会话上）。这里只保留必要的三段：
// 选号 + 账号级错误策略 + 有界轮转。
package server

import (
	"context"
	"errors"
	"log"
	"net/http"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
	"github.com/BegoniaHe/wokbuddy-demon/internal/logfmt"
	"github.com/BegoniaHe/wokbuddy-demon/internal/notify"
	"github.com/BegoniaHe/wokbuddy-demon/internal/pool"
	"github.com/BegoniaHe/wokbuddy-demon/internal/upstream"
)

// searchRealm 联网搜索只放行 CN 域选号：上游搜索端点仅在 CN 域实测可用。
// 两域放开留 TODO（需先补 global 域实测，不能靠推断）。
const searchRealm = "cn"

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
			h.noteSearchRefreshFailure(acct, err)

			if ue, ok := errors.AsType[*upstream.Error](err); ok {
				return nil, h.searchFailureFor(acct.UID, ue)
			}

			return nil, &searchFailure{
				status: http.StatusBadGateway,
				code:   "upstream_auth",
				msg:    "upstream token refresh failed",
			}
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

// searchFailureFor 按上游错误分类决定处置与终端形态：
//   - 走既有 applyErrorPolicy（与 chat 同一套惩罚口径，不新增第二套）；
//   - 请求级/内容/队列类不可自愈 → 返回带 code 的终态（不再轮转）；
//   - 其余（限流/会话/额度/账号故障/5xx）→ code 为空，让调用方换号；但**状态码与
//     原文照样填好**——轮转耗尽时直接把这一份透给客户端，留零值会走到
//     WriteHeader(0) 直接 panic（客户端只看到空 200）。
func (h *Handler) searchFailureFor(uid string, upErr *upstream.Error) *searchFailure {
	h.applyErrorPolicy(uid, upErr.Kind, upErr.Msg, searchRealm, "", upErr)

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
	switch upErr.Kind {
	case upstream.ErrBadParams:
		return http.StatusBadRequest, "bad_params"
	case upstream.ErrContentBlocked:
		return http.StatusBadRequest, "content_blocked"
	case upstream.ErrModelBlocked, upstream.ErrModelAccess, upstream.ErrModelNotFound:
		return http.StatusBadRequest, "model_unavailable"
	case upstream.ErrQueueWaiting:
		return http.StatusTooManyRequests, "queue_waiting"
	case upstream.ErrQueueFull:
		return http.StatusServiceUnavailable, "queue_full"
	default:
		return 0, ""
	}
}
