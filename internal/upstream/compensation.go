// compensation.go 官方补偿活动的状态查询（/billing/meter/compensation-status）。
//
// 背景：官方在「套餐与积分」页挂限时补偿横幅（例：「DeepSeek 模型服务异常额外补偿
// 500 积分」，带领取截止日），账号侧看得见，网关这边此前却是**盲领**——签到后顺带
// 调一次 ClaimCompensation，写法是 `if credit, err := ...; err == nil`，于是
// ① 没有补偿 ② 已经领过 ③ **真的领取失败** 三种情形在日志里完全同构。补偿带截止日，
// 签到停摆（被禁用 / 机器关机 / 账号被禁用）时就会静默过期。
//
// 上游口径（实测 2026-10-16，cn 域 CLI 侧与 Web 侧返回一致）：
//
//	{"code":0,"msg":"OK","data":{
//	   "claimed":false, "active":true, "credit_num":500,
//	   "validity_period":1, "end_time":"2026-10-22 23:59:59"}}
//
// active=false 是「当前无补偿活动」的正常 200 返回，**不是错误**——这条语义搞错会让
// 日常无活动的账号把排程日志刷成一片错误。故本接口只在传输失败 / 非 2xx / 业务码非 0
// 时返回 error。
package upstream

import (
	"encoding/json"
	"fmt"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
)

// compensationStatusPath 补偿状态查询路径（与 ClaimCompensation 同域同头）。
const compensationStatusPath = "/billing/meter/compensation-status"

// CompensationStatus 单账号的补偿活动状态。
type CompensationStatus struct {
	// Active 当前是否有生效中的补偿活动（无活动时为 false，属正常返回）。
	Active bool `json:"active"`
	// Claimed 本次活动是否已领取。
	Claimed bool `json:"claimed"`
	// CreditNum 待领额度（积分）。
	CreditNum int64 `json:"credit_num"`
	// ValidityPeriod 发放积分的有效期倍数。上游未明示单位，前端也不渲染，
	// 故只透传不做换算（避免臆造语义）。
	ValidityPeriod int64 `json:"validity_period"`
	// EndTime 领取截止时间（上游为 "2006-01-02 15:04:05" 无时区字符串）。
	EndTime string `json:"end_time"`
}

// Pending 是否存在「可领且未领」的补偿——ClaimCompensation 的前置判据。
//
// 三者与门：active=false 是没有活动；claimed=true 是已领完（再调只会拿到业务错误）；
// credit_num<=0 是活动存在但无额度。三种都归为「无需领取」。
func (s CompensationStatus) Pending() bool {
	return s.Active && !s.Claimed && s.CreditNum > 0
}

// GetCompensationStatus 查补偿活动状态（billingBase，与 ClaimCompensation 同域）。
func (c *Client) GetCompensationStatus(account *auth.Auth) (CompensationStatus, error) {
	data, err := c.billingJSON(account, compensationStatusPath, map[string]any{})
	if err != nil {
		return CompensationStatus{}, fmt.Errorf("GetCompensationStatus: %w", err)
	}

	var status CompensationStatus
	if err := json.Unmarshal(data, &status); err != nil {
		return CompensationStatus{}, fmt.Errorf("compensation status parse: %w", err)
	}

	return status, nil
}
