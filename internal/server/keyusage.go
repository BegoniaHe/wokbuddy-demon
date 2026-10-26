package server

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/BegoniaHe/wokbuddy-demon/internal/apikey"
)

// keyUsage 一次命名密钥请求的用量累加器：withClientAuth 创建并挂到 context，
// chatFlow 在出口回填 token 观测，withClientAuth 在 next 返回后落账到密钥库。
// 默认密钥命中或未鉴权请求不创建累加器（nil），不产生任何记账。
type keyUsage struct {
	id                        string
	realm                     string
	prompt, completion, total int64
}

// keyUsageKey context 键类型（私有，避免跨包误用）。
type keyUsageKey struct{}

// keyUsageFrom 从请求 context 取命名密钥用量累加器；非命名密钥请求返回 nil。
func keyUsageFrom(ctx context.Context) *keyUsage {
	ku, _ := ctx.Value(keyUsageKey{}).(*keyUsage)
	return ku
}

// writeKeyRejection 按准入拒绝原因写出响应：
//   - 过期：401 泛化错误（与不存在/已撤销同口径）；
//   - IP 未命中：403 ip_not_allowed，不回显来源 IP；
//   - 配额耗尽：429 insufficient_quota，不带 Retry-After（生命周期配额不自动恢复）；
//   - RPM/TPM/并发：沿用 429 + Retry-After。
func writeKeyRejection(
	w http.ResponseWriter, r *http.Request, reason apikey.RejectReason, retryAfter time.Duration,
) {
	switch reason {
	case apikey.RejectExpired:
		writeClientAuthError(w, r)
	case apikey.RejectIPNotAllowed:
		writeKeyScopedError(w, r, http.StatusForbidden,
			"ip_not_allowed", "permission_error", "ip not allowed")
	case apikey.RejectQuotaExhausted:
		writeKeyScopedError(w, r, http.StatusTooManyRequests,
			"insufficient_quota", "rate_limit_error", "insufficient quota")
	default:
		writeKeyLimitError(w, r, retryAfter)
	}
}

// writeKeyScopedError 按入站协议写出带状态码的密钥拒绝错误（不含 Retry-After）。
func writeKeyScopedError(
	w http.ResponseWriter, r *http.Request, status int, openAICode, anthropicType, msg string,
) {
	if requestsGemini(r) {
		writeGeminiError(w, status, msg, "")

		return
	}

	if wantsAnthropicErrorBody(r) {
		setAnthropicRequestID(w, r)
		writeAnthropicError(w, status, anthropicType, msg, "")

		return
	}

	writeOpenAIError(w, status, openAICode, msg)
}

// writeKeyLimitError 命名密钥超限响应：429 + Retry-After（秒，至少 1）。
// 错误体按入站协议编码，Anthropic SDK 只解析顶层 type=error 的信封。
func writeKeyLimitError(w http.ResponseWriter, r *http.Request, retryAfter time.Duration) {
	writeLimitError(w, r, retryAfter, "rate_limit_exceeded", "api key rate limit exceeded")
}

// writeLimitError 429 限流响应：Retry-After + 按入站协议编码错误体。
func writeLimitError(w http.ResponseWriter, r *http.Request, retryAfter time.Duration, code, msg string) {
	seconds := max(int((retryAfter+time.Second-1)/time.Second), 1)
	w.Header().Set("Retry-After", strconv.Itoa(seconds))

	if requestsGemini(r) {
		writeGeminiError(w, http.StatusTooManyRequests, msg, "")

		return
	}

	if wantsAnthropicErrorBody(r) {
		setAnthropicRequestID(w, r)
		writeAnthropicError(w, http.StatusTooManyRequests, "rate_limit_error", msg, "")

		return
	}

	writeOpenAIError(w, http.StatusTooManyRequests, code, msg)
}

// keyRealmFrom 返回命名密钥的出口绑定（issue #94）；默认密钥/未绑定返回 ""。
// 出口绑定挂在 keyUsage 上（同一次 context 注入），不再单独占用一个 context 键。
func keyRealmFrom(ctx context.Context) string {
	if ku := keyUsageFrom(ctx); ku != nil {
		return ku.realm
	}

	return ""
}
