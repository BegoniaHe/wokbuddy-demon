// trial.go global 专属「一次性 trial 加油包」领取：POST {billingBase}/billing/ide/trial。
// 仅 global 账号适用（CN 无此端点）；幂等码 14051 = 已领过（视为正常，非错误）。
// 这是 global 唯一天然的积分增益动作（无签到/任务中心，见 PLAN D4）。
package upstream

import (
	"errors"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
)

// trialPath global trial 加油包端点（Maquer/workbuddy-checkin 实测）。
const trialPath = "/billing/ide/trial"

var errClaimTrialOnlyGlobalAccounts = errors.New("claim trial: only global accounts")

// ClaimTrial 领取一次性 trial 加油包。仅 global 账号可调（CN 无此端点）：
// 非 global → 直接报错（工具层还会再拦一道，这里是客户端侧防线）。
// 返回 claimed：true=成功新领；false=已领过（幂等，不算失败）。
func (c *Client) ClaimTrial(a *auth.Auth) (claimed bool, err error) {
	if a == nil || a.Realm() != "global" {
		return false, errClaimTrialOnlyGlobalAccounts
	}

	_, err = c.billingJSON(a, trialPath, nil)
	if err != nil {
		var ue *Error
		if errors.As(err, &ue) && trialAlreadyErr(ue.Msg) {
			return false, nil // 已领过：幂等成功，非错误
		}

		return false, err
	}

	return true, nil
}

// trialAlreadyErr 判定错误 Msg（原始 JSON 信封）是否携带幂等码 14051（已领取过）。
func trialAlreadyErr(msg string) bool {
	return hasBusinessCode(msg, "14051")
}
