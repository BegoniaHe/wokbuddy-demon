// minievents.go 小程序口径埋点事件构造（WorkBuddy_MP 指纹）。
//
// 上游 growth 域的小程序限定任务（Sequential_Tasks_1..7 / school_season）判据
// 依赖小程序形状的 /v2/report 事件：
//   - 对话类任务：chat_request_send 计数（SchoolSeasonChatEvent 额外带 activityId）。
//   - 专家任务：expert_actual_use。
//   - 模型 / 灵感任务：带模型字段的对话事件与 playbook 事件组。
//
// 端点基址 www.codebuddy.cn（billing 同域）；信封 {code,msg,data}，code=0 成功。
package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"time"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
	"github.com/BegoniaHe/wokbuddy-demon/internal/session"
)

// mpExtVersion 小程序自身版本号（覆盖 mpEventBase 默认的 2.4.0）。
const mpExtVersion = "2.2.8"

var errMpReportNoEvents = errors.New("mp report: no events")

// mpReportPath 小程序埋点上报路径。
const mpReportPath = "/v2/report"

// schoolOpenDayActivityID 校园日活动 id（school_season 事件 activityId 字段值）。
const schoolOpenDayActivityID = "school_open_day_2026"

// mpEventBase 小程序埋点公共指纹（appservice wQ()+Ao() 对齐）。
// machineId 按 uid 稳定派生，与桌面/web 域同思路：每账号一台固定虚拟设备，
// 账号间互异，避免全部账号共用同一硬编码设备标识。
func mpEventBase(account2 *auth.Auth) map[string]any {
	return map[string]any{
		"timestamp":    time.Now().UnixMilli(),
		"ideType":      "WorkBuddy_MP",
		"ideVersion":   "2.4.0",
		"extName":      "workbuddy-mp",
		"extVersion":   "2.4.0",
		"product":      "SaaS",
		"ideName":      "wx_app_cloud",
		"platform":     "mini_program",
		"os":           "windows",
		"osVersion":    "11",
		"arch":         "x64",
		"machineId":    deriveID(account2, "mpmachine"),
		"timezone":     "Asia/Shanghai",
		"userId":       account2.UID,
		"userNickname": account2.NicknameValue(),
	}
}

// ReportMPEvent 以小程序指纹向 www.codebuddy.cn/v2/report 批量上报事件。
func (c *Client) ReportMPEvent(account *auth.Auth, events ...map[string]any) error {
	if len(events) == 0 {
		return errMpReportNoEvents
	}

	base := mpEventBase(account)

	arr := make([]map[string]any, 0, len(events))
	for _, ev := range events {
		m := map[string]any{}
		maps.Copy(m, base)
		maps.Copy(m, ev)
		arr = append(arr, m)
	}

	raw, err := json.Marshal(arr)
	if err != nil {
		return fmt.Errorf("ReportMPEvent: %w", err)
	}

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, c.BillingBaseCN+mpReportPath, bytes.NewReader(raw))
	if err != nil {
		return fmt.Errorf("ReportMPEvent: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+account.AccessTokenValue())
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	if account.UID != "" {
		req.Header.Set("X-User-Id", account.UID)
	}

	req.Header.Set("X-Client-Product", "workbuddy-mp")
	req.Header.Set("X-Client-Version", "2.4.0")
	req.Header.Set("X-Client-Platform", "mp-weixin")
	req.Header.Set("X-Platform", "wechatmp")
	_, err = c.doJSON(req)

	return err
}

// SchoolChatTimesEvents 构造一条 chat_request_send 事件（chat_3_times 计数）。
func SchoolChatTimesEvents(conversationID string) map[string]any {
	rid := session.NewUUIDv7Hex()

	return map[string]any{
		"eventCode":   "chat_request_send",
		"inputLength": 14, "isPlan": false, "isAutoExecuteTerminal": false,
		"isAutoModify": false, "codebaseEnable": false, "maxToken": 0,
		"maxSteps": 500, "temperature": 0, "maxRetries": 0,
		"mentionContexts": []any{}, "knowledgeId": []any{}, "knowledgeName": []any{},
		"codebaseId": "", "mentionContextCount": 0, "command": "",
		"recommendId": "", "skillId": "", "skillCount": 0, "totalCount": 0,
		"requestId": rid, "traceId": rid, "rootRequestId": rid,
		"parentConversationId": conversationID, "conversationId": conversationID,
		"messageId": session.NewUUIDv7Hex(),
		"agentName": "mp", "agentType": "main",
		"codebuddy.session_id":              conversationID,
		"codebuddy.conversation_request_id": rid,
	}
}

// SchoolSeasonChatEvent 构造 growth 域「校园日」（school_season）判据事件：
// mini 指纹 chat_request_send + activityId=school_open_day_2026。事件形状与
// SchoolChatTimesEvents 同构，仅追加 activityId（实测无 activityId 的事件不点亮）。
func SchoolSeasonChatEvent(conversationID string) map[string]any {
	ev := SchoolChatTimesEvents(conversationID)
	ev["activityId"] = schoolOpenDayActivityID

	return ev
}

// MiniExpertUseEvent 构造 growth 域 Sequential_Tasks_2「在小程序内选中专家并完成
// 有效对话」的判据事件：mp 指纹 expert_actual_use。形状对齐小程序源码
// app-service.js 的真实发射点（上游 task_runner 实测 2026-09-23：上报即 completed，
// claim +200c+5e）。与学校活动期的 expert 事件是**两套口径**，勿照抄：
//   - 不带 conversationId/activityId——真实事件就是这两个字段都不带；
//   - extVersion 用小程序自身版本 2.2.8（覆盖 mpEventBase 的 2.4.0）；
//   - source=mini_program + type 固定 "send_message"（小程序恒发此值）。
//
// expertID 必须是专家市场真实 ex_ id（ListMarketExperts），空 id 服务端不入账。
func MiniExpertUseEvent(expertID, expertName, expertType string) map[string]any {
	if expertType == "" {
		expertType = "agent"
	}

	if expertName == "" {
		expertName = expertID
	}

	return map[string]any{
		"eventCode": "expert_actual_use", "reportDelay": 0,
		"extVersion": mpExtVersion, "source": "mini_program",
		"id": expertID, "name": expertID,
		"expertTitle": expertName, "type": "send_message",
		"characterCount": 12, "expertType": expertType,
	}
}

// MiniChatModelEvent mp 对话事件 + 模型字段（Sequential_Tasks_5「使用 GLM5.2」判据
// 载体）：小程序 chat_request_send 真实发射点（mpsrc main 32904 模块）带
// requestModelId / requestModelName——Tasks_1/3 的裸对话事件不带模型，模型任务
// 须用本形态（判据待解锁实测验证）。
func MiniChatModelEvent(conversationID, modelID, modelName string) map[string]any {
	ev := SchoolChatTimesEvents(conversationID)
	ev["requestModelId"] = modelID
	ev["requestModelName"] = modelName

	return ev
}

// MiniPlaybookEvents mp 指纹灵感事件组（Sequential_Tasks_7「体验灵感功能」判据
// 载体，形状对齐 mpsrc main 73640/73665 发射点：playbook_cta_click →
// playbook_prompt_send）。PC 序列（DesktopPlaybookPromptSequence）已实测点亮
// playbook_prompt，本组作为 mp 形态补充（任务在 mp 链上，判据究竟认哪侧待解锁实测）。
func MiniPlaybookEvents(caseID, caseName string) []map[string]any {
	base := map[string]any{
		"id": caseID, "name": caseName, "type": "document",
		"categoryId": "", "categoryName": "",
		"skills": "", "skillNames": "",
	}
	cta := map[string]any{
		"eventCode": "playbook_cta_click", "source": "discover", "position": 1,
		"extVersion": mpExtVersion,
	}
	maps.Copy(cta, base)

	send := map[string]any{
		"eventCode": "playbook_prompt_send", "source": "discover",
		"promptLength": 96, "isOfficial": 1,
		"conversationId": session.NewUUIDv4(),
		"extVersion":     mpExtVersion,
	}
	maps.Copy(send, base)

	return []map[string]any{cta, send}
}
