package server

import (
	"context"
	"net/http"
	"strconv"
	"time"
)

// keyUsage 一次命名密钥请求的用量累加器：withClientAuth 创建并挂到 context，
// chatFlow 在出口回填 token 观测，withClientAuth 在 next 返回后落账到密钥库。
// 默认密钥命中或未鉴权请求不创建累加器（nil），不产生任何记账。
type keyUsage struct {
	id                        string
	prompt, completion, total int64
}

// keyUsageKey context 键类型（私有，避免跨包误用）。
type keyUsageKey struct{}

// keyUsageFrom 从请求 context 取命名密钥用量累加器；非命名密钥请求返回 nil。
func keyUsageFrom(ctx context.Context) *keyUsage {
	ku, _ := ctx.Value(keyUsageKey{}).(*keyUsage)
	return ku
}

// writeKeyLimitError 命名密钥超限响应：429 + Retry-After（秒，至少 1）。
func writeKeyLimitError(w http.ResponseWriter, retryAfter time.Duration) {
	seconds := int((retryAfter + time.Second - 1) / time.Second)
	if seconds < 1 {
		seconds = 1
	}
	w.Header().Set("Retry-After", strconv.Itoa(seconds))
	writeOpenAIError(w, http.StatusTooManyRequests, "rate_limit_exceeded", "api key rate limit exceeded")
}
