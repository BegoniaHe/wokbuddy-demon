// Package logincode 汇总 WorkBuddy 设备授权流程的业务码口径。
//
// CLI（cmd/login）与面板（internal/panel）共用同一张码表：只有 11217 / 12151
// 表示「还在等用户完成登录」，许可席位/过期类码是立即失败的终态，必须透出上游 msg，
// 不能当作 pending 空转到 TTL。
package logincode

import "fmt"

// 登录轮询业务码（官方 Win32 客户端同口径）。
const (
	RetryFetchToken   = 11217 // token 尚未就绪，继续轮询
	RetryFetchAccount = 12151 // 账号信息尚未就绪，继续轮询
	LicenseSeatLimit  = 12005 // 许可席位超限（终态）
	LicenseExpired    = 11212 // 许可过期（终态）
	TrialExpired      = 11216 // 试用过期（终态）
)

// Error 携带业务码、上游消息与 HTTP 状态。
type Error struct {
	Code   int
	Msg    string
	Status int
}

// Error 实现 error；上游 msg 非空时直接透出，便于 CLI / 面板显示真实原因。
func (e *Error) Error() string {
	if e == nil {
		return "login error"
	}

	if e.Msg != "" {
		return fmt.Sprintf("upstream code=%d: %s", e.Code, e.Msg)
	}

	return fmt.Sprintf("upstream code=%d", e.Code)
}

// IsPending 报告业务码是否表示「继续轮询」。
func IsPending(code int) bool {
	return code == RetryFetchToken || code == RetryFetchAccount
}

// IsTerminal 报告业务码是否表示立即失败的许可终态。
func IsTerminal(code int) bool {
	switch code {
	case LicenseSeatLimit, LicenseExpired, TrialExpired:
		return true
	default:
		return false
	}
}
