// desktop.go 桌面客户端（WorkBuddy Desktop）行为指纹上报。
//
// 来源：2026-09-12 Sunny 抓包实测（data/desktop-task-protocol.md）+ 官方分发包
// app.asar 逆向（CN 5.8.3 / global 5.8.4）。桌面端点亮「需电脑端」类任务的关键
// 不是独立端点，而是同一 POST /v2/report 通道上**不同的客户端指纹**：
//
//	POST https://www.workbuddy.cn/v2/report           ← chatBase（CLI 上报走 billingBase）
//	User-Agent: WorkBuddy/<realm 版本>
//	X-Domain: www.workbuddy.cn（host 形态）, X-Product: SaaS, X-User-Id: <uid>
//	Body: [ {...event...} ]                           ← 数组
//
// 每个事件除业务字段外必带桌面指纹（ideName/ideType=WorkBuddy、
// extName=workbuddy-desktop、commit/releaseDate 取分发包 product.json 真值等）。
// 实测点亮记录：
//   - RichMeow_Chat（桌面端对话1次）：agent_task_created + 成功的
//     chat_message_response(isSuccessful=true) → 1/1 + UR Buddy。
//   - Hp_Appearance（主题任务）：独立 API
//     POST /v2/user-asset/appearance/set {kind:"theme",resource_key} → SetAppearanceTheme。
//
// 注意：service 端对事件链有一定真实性校验倾向（RichMeow 需要消息成功回执），
// 本模块按实测事件形状发送，不保证所有任务都能 API 侧点亮——autotask 侧
// 仍按 attempt 语义处理结果。
package upstream

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
	"github.com/BegoniaHe/wokbuddy-demon/internal/session"
)

const (
	desktopReportPath    = "/v2/report"
	desktopAppearanceSet = "/v2/user-asset/appearance/set"

	// 官方桌面分发包构建指纹（realm 分档，取自各包 cli/product.json）。
	//   cn     5.8.3  commit 64d249be…  date 2026-10-14T10:11:49.257Z
	//   global 5.8.4  commit 73952edb…  date 2026-10-15T11:17:03.116Z
	// 上报事件 product/releaseDate/commit/ideVersion/extVersion 取此组，与 UA 版本自洽。
	cnDesktopCommit          = "64d249bed3dec40edd720537d96f27e70a3f6a4f"
	cnDesktopReleaseDate     = int64(1791972709257) // 2026-10-14T10:11:49.257Z
	globalDesktopCommit      = "73952edb48da46bd0b244661efe22f4f247c4977"
	globalDesktopReleaseDate = int64(1792063023116) // 2026-10-15T11:17:03.116Z
)

// desktopLocal 桌面端事件中 source/mode 字段的本地标记值（官方分发端恒发此值）。
const desktopLocal = "LOCAL"

var (
	errDesktopReportNoEvents       = errors.New("desktop report: no events")
	errDesktopChatWithExpertFailed = errors.New("DesktopChatWithExpert failed")
	errSSERequestID                = errors.New("SSE 中未找到服务端 requestId")
)

// desktopBuild 官方桌面分发包构建指纹（版本 + commit + releaseDate）。
type desktopBuild struct {
	version     string
	commit      string
	releaseDate int64
}

// desktopBuildFor 按账号 realm 返回桌面构建指纹（nil 账号按 cn）。
func desktopBuildFor(a *auth.Auth) desktopBuild {
	if a != nil && a.IsGlobal() {
		return desktopBuild{version: globalClientVersion, commit: globalDesktopCommit, releaseDate: globalDesktopReleaseDate}
	}

	return desktopBuild{version: cnClientVersion, commit: cnDesktopCommit, releaseDate: cnDesktopReleaseDate}
}

// desktopUAFor 桌面上报/白名单类出站 UA：单段 `WorkBuddy/<realm 版本>`
// （官方 daemon 上报头组 `${applicationName}/${appVersion}` 形态，不带 CLI 段）。
func desktopUAFor(a *auth.Auth) string {
	return "WorkBuddy/" + desktopBuildFor(a).version
}

// desktopBase 桌面端 /v2/report 与 user-asset 走 chatBase（copilot.tencent.com）。
func (c *Client) desktopBase(a *auth.Auth) string { return c.chatBase(a) }

// desktopDomain 桌面端 X-Domain 取 base 的 host 形态（官方不发送完整 URL）。
func (c *Client) desktopDomain(a *auth.Auth) string {
	base := c.desktopBase(a)
	if u, err := url.Parse(base); err == nil && u.Host != "" {
		return u.Host
	}

	return strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(base, "https://"), "http://"), "/")
}

// deriveID 由 uid 稳定派生一个 36 位 hex 设备标识（machineId/qimei36 复用），
// 幂等：同一账号每次生成相同值，模拟固定设备。
func deriveID(a *auth.Auth, salt string) string {
	sum := sha256.Sum256([]byte(salt + ":" + a.UID))
	return hex.EncodeToString(sum[:18]) // 36 hex chars
}

// DesktopEvent 桌面端事件：业务字段任意（map），公共指纹由 ReportDesktopEvent 注入。
type DesktopEvent map[string]any

// desktopFingerprint 公共桌面指纹字段（注入每个事件，覆盖同名业务键）。
// 版本/commit/releaseDate 按 realm 取官方分发包真值；os/arch 固定为常见桌面
// （win32/x64）——本网关非桌面运行，保留「伪装桌面」常量而非暴露宿主平台。
func desktopFingerprint(account7 *auth.Auth) map[string]any {
	desktopBuild2 := desktopBuildFor(account7)
	now := time.Now().UnixMilli()

	fingerprint := map[string]any{
		"timezone":     "Asia/Shanghai",
		"reportDelay":  2000,
		"userId":       account7.UID,
		"username":     account7.NicknameValue(),
		"userNickname": account7.NicknameValue(),
		"product":      "SaaS",
		"releaseDate":  desktopBuild2.releaseDate,
		"commit":       desktopBuild2.commit,
		"ideName":      "WorkBuddy",
		"ideType":      "WorkBuddy",
		"ideVersion":   desktopBuild2.version,
		"machineId":    deriveID(account7, "machine"),
		"sessionId":    deriveID(account7, "session"),
		"extName":      "workbuddy-desktop",
		"extVersion":   desktopBuild2.version,
		"os":           "win32",
		"arch":         "x64",
		"osVersion":    "10.0.26220",
		"cpuCores":     20,
		"memorySize":   24,
		"timestamp":    now,
		"presentAt":    now,
	}

	if account7.EnterpriseID != "" {
		fingerprint["enterpriseId"] = account7.EnterpriseID
		fingerprint["enterprise"] = account7.EnterpriseID
	}

	return fingerprint
}

// ReportDesktopEvent 以桌面客户端指纹向 copilot.tencent.com/v2/report 批量上报事件。
// events 为业务载荷（eventCode 等字段由调用方给出）；公共指纹自动注入，
// 业务字段优先（可用于覆盖 qimei36/machineId 等设备标识做真实设备对齐）。
func (c *Client) ReportDesktopEvent(account2 *auth.Auth, events ...DesktopEvent) error {
	if len(events) == 0 {
		return errDesktopReportNoEvents
	}

	fp := desktopFingerprint(account2)

	arr := make([]map[string]any, 0, len(events))
	for _, ev := range events {
		m := map[string]any{}
		maps.Copy(m, fp)
		maps.Copy(m, ev)
		arr = append(arr, m)
	}

	raw, err := json.Marshal(arr)
	if err != nil {
		return fmt.Errorf("ReportDesktopEvent: %w", err)
	}

	req, err := http.NewRequestWithContext(
		context.Background(),
		http.MethodPost,
		c.desktopBase(account2)+desktopReportPath,
		bytes.NewReader(raw),
	)
	if err != nil {
		return fmt.Errorf("ReportDesktopEvent: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+account2.AccessTokenValue())
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Content-Type", "application/json;charset=UTF-8")
	req.Header.Set("User-Agent", desktopUAFor(account2))
	req.Header.Set("X-Domain", c.desktopDomain(account2))
	req.Header.Set("X-Product", "SaaS")
	injectEnterpriseHeaders(req, account2)
	req.Header.Set("X-Request-ID", deriveID(account2, "req")+strconv.FormatInt(time.Now().UnixNano()%1e6, 10))

	if account2.UID != "" {
		req.Header.Set("X-User-Id", account2.UID)
	}

	_, err = c.doJSON(req)

	return err
}

// DesktopChatSequence 构造一次「桌面端成功对话」的完整事件链
// （agent_task_created → chat_message_send → chat_request_send →
// chat_message_response(isSuccessful) → chat_message_status → chat_request_response）。
// 实测该链点亮 RichMeow_Chat。conversationID/requestID/messageID 由调用方生成。
func DesktopChatSequence(conversationID, requestID, messageID, modelID, modelName string) []DesktopEvent {
	return []DesktopEvent{
		desktopTaskCreated(conversationID, messageID, modelID, modelName),
		desktopMessageSend(conversationID, requestID, messageID),
		desktopRequestSend(conversationID, requestID),
		desktopMessageResponse(conversationID, requestID, messageID, modelID),
		desktopMessageStatus(conversationID, requestID, messageID),
		desktopRequestResponse(conversationID, requestID),
	}
}

func desktopEvent(code string, extra map[string]any) DesktopEvent {
	ev := DesktopEvent{"eventCode": code}
	maps.Copy(ev, extra)

	return ev
}

func desktopTaskCreated(conversationID, messageID, modelID, modelName string) DesktopEvent {
	return desktopEvent("agent_task_created", map[string]any{
		"source": desktopLocal, "name": "working", "task_target": "local", "mode": "craft",
		"requestModelId": modelID, "requestModelName": modelName,
		"has_repo": false, "repo_type": "none", "workspace_type": "empty",
		"has_connector": false, "connector_types": []any{},
		"has_mention": false, "mention_types": []any{},
		"has_template": false, "action": "", "template_name": "",
		"has_expert": false, "expert_id": "", "expert_name": "", "expert_industry_id": "",
		"has_skill": false, "skill_names": []any{},
		"conversationId": conversationID, "messageId": messageID,
		"buddyId": "", "buddyName": "",
	})
}

func desktopMessageSend(conversationID, requestID, messageID string) DesktopEvent {
	return desktopEvent("chat_message_send", map[string]any{
		"messageId": messageID + "-assistant", "historyCount": 0,
		"isContextTruncated": false, "currentStepCount": 1,
		"traceId": requestID, "rootRequestId": requestID,
		"parentConversationId": conversationID,
		"agentName":            "cli", "agentType": "main",
	})
}

func desktopRequestSend(conversationID, requestID string) DesktopEvent {
	return desktopEvent("chat_request_send", map[string]any{
		"inputLength": 24, "isPlan": false, "isAutoExecuteTerminal": false,
		"isAutoModify": false, "codebaseEnable": false, "maxToken": 0,
		"maxSteps": 500, "temperature": 0, "maxRetries": 0,
		"mentionContexts": []any{}, "knowledgeId": []any{}, "knowledgeName": []any{},
		"codebaseId": "", "mentionContextCount": 0, "command": "",
		"recommendId": "", "skillId": "", "skillCount": 0, "totalCount": 0,
		"traceId": requestID, "rootRequestId": requestID,
		"parentConversationId": conversationID,
		"agentName":            "cli", "agentType": "main",
		"codebuddy.session_id":              conversationID,
		"codebuddy.conversation_request_id": requestID,
	})
}

func desktopMessageResponse(conversationID, requestID, messageID, modelID string) DesktopEvent {
	return desktopEvent("chat_message_response", map[string]any{
		"messageId": messageID + "-assistant", "responseModelId": modelID,
		"inputToken": 120, "outputToken": 80, "totalToken": 200,
		"cachedTokens": 0, "cachedWriteTokens": 0, "cachedMissTokens": 0,
		"isSuccessful": true, "messageErrorCode": "", "finishReason": "stop",
		"firstTokenAt": time.Now().UnixMilli(), "traceId": requestID,
		"conversationId": conversationID,
		"rootRequestId":  requestID, "parentConversationId": conversationID,
		"agentName": "cli", "agentType": "main",
		"codebuddy.session_id":              conversationID,
		"codebuddy.conversation_request_id": requestID,
	})
}

func desktopMessageStatus(conversationID, requestID, messageID string) DesktopEvent {
	return desktopEvent("chat_message_status", map[string]any{
		"messageId": messageID + "-assistant", "messageErrorCode": "0",
		"traceId": requestID, "rootRequestId": requestID,
		"parentConversationId": conversationID,
		"agentName":            "cli", "agentType": "main",
	})
}

func desktopRequestResponse(conversationID, requestID string) DesktopEvent {
	return desktopEvent("chat_request_response", map[string]any{
		"mode": "craft", "toolCallCount": 0,
		"inputToken": 120, "outputToken": 80, "totalToken": 200,
		"cachedTokens": 0, "cachedWriteTokens": 0, "cachedMissTokens": 0,
		"isSuccessful": true, "messageErrorCode": "", "finishReason": "stop",
		"rootRequestId": requestID, "parentConversationId": conversationID,
	})
}

// SetAppearanceTheme 应用外观主题（实测：POST copilot.tencent.com/v2/user-asset/appearance/set，
// 和品主题 resource_key 为 "theme-tkmw7j"，浅色 "light"、深色 "dark"）。纯 API set 不计
// Hp_Appearance 分（需客户端切主题后真实活跃），保留供调色/还原与后续验证用。
func (c *Client) SetAppearanceTheme(account *auth.Auth, resourceKey string) error {
	body := map[string]string{"kind": "theme", "resource_key": resourceKey}

	raw, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("SetAppearanceTheme: %w", err)
	}

	req, err := http.NewRequestWithContext(
		context.Background(),
		http.MethodPost,
		c.desktopBase(account)+desktopAppearanceSet,
		bytes.NewReader(raw),
	)
	if err != nil {
		return fmt.Errorf("SetAppearanceTheme: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+account.AccessTokenValue())
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Content-Type", "application/json;charset=UTF-8")
	req.Header.Set("User-Agent", desktopUAFor(account))
	req.Header.Set("X-Product", "SaaS")

	if account.UID != "" {
		req.Header.Set("X-User-Id", account.UID)
	}

	_, err = c.doJSON(req)

	return err
}

// DesktopBuddyAppSequence 构造「进入 Buddy 应用」五连事件（实测两账号纯 API 点亮
// Buddy_App 与 Buddy_App_QQ）：discover → show → enter_click → auth_confirm →
// bindaccount_skip。buddyID 固定用企鹅教师助手 cb_y5Dy46tPQGGWtueMxXbe
// （Buddy_App_QQ 的判据应用），同时满足 Buddy_App「进入任一应用」。
func DesktopBuddyAppSequence(buddyID, buddyName string) []DesktopEvent {
	callback := func(code string, extra map[string]any) DesktopEvent {
		desktopEvent2 := DesktopEvent{
			"eventCode": code, "mode": desktopLocal,
			"buddyId": buddyID, "buddyName": buddyName,
		}
		maps.Copy(desktopEvent2, extra)

		return desktopEvent2
	}

	return []DesktopEvent{
		callback("buddyapp_discover_click", nil),
		callback("buddyapp_show", map[string]any{"elementId": buddyID, "elementName": buddyName, "position": 2}),
		callback("buddyapp_enter_click", map[string]any{"elementId": buddyID, "elementName": buddyName, "position": 2, "isFirstPage": "1"}),
		callback("buddyapp_auth_confirm_click", map[string]any{"elementId": buddyID, "elementName": buddyName}),
		callback("buddyapp_bindaccount_skip_click", map[string]any{"elementId": buddyID, "elementName": buddyName}),
	}
}

// DesktopAutomationCreateEvent 构造「定时任务创建成功」事件（实测两账号纯 API
// 点亮 automation_1）。name 为任务名，可与真实创建语义对齐。
func DesktopAutomationCreateEvent(name string) DesktopEvent {
	return DesktopEvent{
		"eventCode": "automated_task_create_suc", "name": name,
		"source": "manually", "modelId": "fast-model", "modelIsThinking": true,
		"connectorCount": 0, "skills": "", "skillCount": 0,
		"scheduleType": "once", "mode": desktopLocal,
	}
}

// ReportWebEvent 以 Web 端指纹向 www.workbuddy.cn/v2/report 上报单事件。
// 与桌面指纹（copilot 域）不同：web 域事件是浏览器形状（os/machineId/userAgent），
// 用于 Library_read 等页面行为类任务（实测 library_doc_intro_click 4 秒点亮）。
func (c *Client) ReportWebEvent(account6 *auth.Auth, eventCode, pageURL, elementID, elementName string) error {
	str := "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/152.0.0.0 Safari/537.36"
	table := map[string]any{
		"eventCode": eventCode, "timestamp": time.Now().UnixMilli(), "reportDelay": 0,
		"pageURL": pageURL, "elementId": elementID, "elementName": elementName,
		"os": "Win32", "arch": "", "osVersion": "10.0", "userAgent": str,
		"machineId": deriveID(account6, "webmachine"), "userId": account6.UID,
		"userNickname": account6.NicknameValue(), "enterpriseId": account6.EnterpriseID,
	}

	raw, err := json.Marshal([]map[string]any{table})
	if err != nil {
		return fmt.Errorf("ReportWebEvent: %w", err)
	}

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, c.webBase(account6)+"/v2/report", bytes.NewReader(raw))
	if err != nil {
		return fmt.Errorf("ReportWebEvent: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+account6.AccessTokenValue())
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Client-Platform", "web")
	req.Header.Set("Origin", c.webBase(account6))
	req.Header.Set("Referer", pageURL)
	req.Header.Set("User-Agent", str)

	if account6.UID != "" {
		req.Header.Set("X-User-Id", account6.UID)
	}

	_, err = c.doJSON(req)

	return err
}

// ---------------------------------------------------------------------------
// 2026-09-12 第五轮：客户端 asar 逆向 + Sunny 真实样本驱动的新判据（三账号实测点亮）。
// 来源：WorkBuddy.exe 5.5.6 app.asar 渲染层事件枚举/载荷 + 抓包真实专家召唤样本
// （data/desktop-task-protocol.md §7.3）。
// ---------------------------------------------------------------------------

// DesktopTemplateUseSequence 构造「使用模板创建任务」事件组（实测 template_5 计数）：
// agent_task_created_with_template {mode,isCustomModel,id,name,requestId} +
// template_used {template_id,task_mode}，JOIN 一条完整 chat 链。
// 三账号实测：5 组（不同模板）一次上报 → 5/5 点亮。
func DesktopTemplateUseSequence(conversationID, requestID, templateID, templateName string) []DesktopEvent {
	events := DesktopChatSequence(conversationID, requestID, "msg-"+templateID, "fast-model", "fast-model")
	events = append(events,
		DesktopEvent{
			"eventCode": "agent_task_created_with_template", "mode": "working",
			"isCustomModel": false, "id": templateID, "name": templateName, "requestId": requestID,
		},
		DesktopEvent{"eventCode": "template_used", "template_id": templateID, "task_mode": "working"},
	)

	return events
}

// DesktopPlaybookPromptSequence 构造「灵感案例做同款」事件组（实测 playbook_prompt 计数）：
// web_element_click(playbook_ctaClick) + playbook_cta_click + playbook_prompt_send
// （Dialog 发送 Prompt，带 conversationId/requestId JOIN chat 链）。三账号实测 1/1 点亮。
func DesktopPlaybookPromptSequence(conversationID, requestID, caseID, caseName string) []DesktopEvent {
	events := DesktopChatSequence(conversationID, requestID, "msg-pb", "fast-model", "fast-model")
	payload := map[string]any{
		"id": caseID, "name": caseName, "type": "document",
		"categoryId": "", "categoryName": "",
	}
	events = append(events,
		DesktopEvent{
			"eventCode": "web_element_click", "pageName": "playbook_detail",
			"elementId": "playbook_ctaClick", "elementName": caseName, "source": "discover",
		},
		DesktopEvent(func() map[string]any {
			m := map[string]any{"eventCode": "playbook_cta_click", "source": "discover", "position": 0}
			maps.Copy(m, payload)

			return m
		}()),
		DesktopEvent(func() map[string]any {
			m := map[string]any{"eventCode": "playbook_prompt_send", "conversationId": conversationID, "requestId": requestID}
			maps.Copy(m, payload)

			return m
		}()),
	)

	return events
}

// DesktopDesignCanvasSequence 构造「设计创意画布」事件组（实测 create_canvas 计数）：
// wbx_design_canvas_task_create + wbx_design_canvas_open（Ardot create_design 工具
// 完成时客户端经 metrics 通道上报，同一 /v2/report 端点）。三账号实测 1/1 点亮。
func DesktopDesignCanvasSequence(conversationID, requestID string) []DesktopEvent {
	events := DesktopChatSequence(conversationID, requestID, "msg-canvas", "fast-model", "fast-model")

	return append(events,
		DesktopEvent{
			"eventCode": "wbx_design_canvas_task_create", "conversationId": conversationID,
			"requestId": requestID, "source": "summon_keyword", "cost": 12000, "isSuccessful": true,
		},
		DesktopEvent{
			"eventCode": "wbx_design_canvas_open", "conversationId": conversationID,
			"requestId": requestID, "id": "ardot-file-" + requestID[len(requestID)-8:],
			"source": "summon_keyword", "type": "page", "cost": 13000, "isSuccessful": true,
		},
	)
}

// MarketExpert 专家市场的单个专家（/portal/operation-platform/market/expert/list 响应子集）。
type MarketExpert struct {
	ExpertID      string `json:"expert_id"`
	ExpertType    string `json:"expert_type"`
	DisplayNameZH string `json:"display_name_zh"`
	ProfessionZH  string `json:"profession_zh"`
	Version       string `json:"version"`
	Categories    []any  `json:"categories"`
}

// MarketExpertList 拉取专家市场真实专家列表（expertType: "agent" 单专家 / "team" 专家团）。
// expert_actual_use 的判据校验要求 id 是平台上真实存在的专家（编造 id 不计数）。
func (c *Client) MarketExpertList(account4 *auth.Auth, expertType string) ([]MarketExpert, error) {
	body := map[string]any{"page": 1, "page_size": 20, "sort_by": "reco_rank", "sort_order": "desc"}
	if expertType != "" {
		body["expert_type"] = expertType
	}

	raw, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("MarketExpertList: %w", err)
	}

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
		c.chatBase(account4)+"/portal/operation-platform/market/expert/list", bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("MarketExpertList: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+account4.AccessTokenValue())
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", desktopUAFor(account4))
	req.Header.Set("X-Domain", c.desktopDomain(account4))
	req.Header.Set("X-Product", "SaaS")

	if account4.UID != "" {
		req.Header.Set("X-User-Id", account4.UID)
	}

	var out struct {
		Experts []MarketExpert `json:"experts"`
	}

	data, err := c.doJSON(req)
	if err != nil {
		return nil, err
	}

	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("expert list parse: %w", err)
	}

	return out.Experts, nil
}

// DesktopChatWithExpert 发一条真实桌面指纹 chat 请求（可带 X-Expert-Id），从 SSE 流
// 解析**服务端返回的 requestId**（data.id，如 cmb-xxxx / 32hex）并返回。
// expert_actual_use 等 JOIN 事件的 requestId 必须是该服务端 id——自造 UUID 不计数
// （客户端 resolveRealRequestId 同款语义，Sunny row 2113 实证）。
func (c *Client) DesktopChatWithExpert(account3 *auth.Auth, expertID string) (conversationID, requestID string, err error) {
	conversationID = fmt.Sprintf("wb2api-conv-%d", time.Now().UnixNano())
	body := map[string]any{
		"model": "fast-model",
		"messages": []any{
			map[string]any{"role": "system", "content": "You are a helpful assistant. 当前处于中文环境，使用简体中文回答。"},
			map[string]any{"role": "user", "content": "1+1等于几？直接回答。"},
		},
		"agent":          "cli",
		"temperature":    1,
		"stream":         true,
		"stream_options": map[string]any{"include_usage": true},
	}

	raw, err := json.Marshal(body)
	if err != nil {
		return "", "", fmt.Errorf("DesktopChatWithExpert: %w", err)
	}

	req, err := http.NewRequestWithContext(
		context.Background(),
		http.MethodPost,
		c.chatBase(account3)+"/v2/chat/completions",
		bytes.NewReader(raw),
	)
	if err != nil {
		return "", "", fmt.Errorf("DesktopChatWithExpert: %w", err)
	}

	c.setDesktopExpertHeaders(req, account3, conversationID, expertID)

	if os.Getenv("WB2A_DEBUG_CHAT") != "" {
		debugChatRequest(req)
	}

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("DesktopChatWithExpert: %w", err)
	}
	defer resp.Body.Close()

	if os.Getenv("WB2A_DEBUG_CHAT") != "" {
		//nolint:forbidigo // WB2A_DEBUG_CHAT 调试输出，非库内调试残留。
		fmt.Printf("[dbg] status=%d enc=%q\n", resp.StatusCode, resp.Header.Get("Content-Encoding"))
	}

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
		return "", "", fmt.Errorf("chat http %d: %s: %w", resp.StatusCode, b, errDesktopChatWithExpertFailed)
	}
	// 从 SSE 流抓第一个 data.id 作为服务端 requestId（读干流避免残留连接）。
	id, err := readServerRequestID(resp.Body)
	if err != nil {
		return "", "", err
	}

	return conversationID, id, nil
}

// setDesktopExpertHeaders 注入桌面指纹 chat 请求头（可带 X-Expert-Id）。
// 对齐官方桌面端 chat 头组：完整三段 UA + X-Agent-Intent/Purpose/Type + X-Private-Data
// + 会话头族 + traceparent/b3/X-B3-* 链路族 + acp-connection-id。
func (c *Client) setDesktopExpertHeaders(req *http.Request, account5 *auth.Auth, conversationID, expertID string) {
	header := req.Header
	requestID := session.NewMessageID()

	header.Set("Authorization", "Bearer "+account5.AccessTokenValue())
	header.Set("Content-Type", "application/json")
	header.Set("Accept", "text/event-stream")
	header.Set("User-Agent", c.userAgent(account5))
	header.Set("X-Domain", c.desktopDomain(account5))
	header.Set("X-Product", "SaaS")
	injectEnterpriseHeaders(req, account5)
	header.Set("X-User-Id", account5.UID)
	header.Set("X-Conversation-Id", conversationID)
	header.Set("X-Conversation-Request-Id", requestID)
	header.Set("X-Conversation-Message-Id", requestID)
	header.Set("X-Request-ID", requestID)
	header.Set("X-Root-Request-Id", requestID)
	header.Set("Acp-Connection-Id", acpConnectionID(conversationID))
	header.Set("X-Agent-Purpose", "conversation")
	header.Set("X-Agent-Intent", "craft")
	header.Set("X-Agent-Type", "main")
	header.Set("X-Private-Data", "true")
	header.Set("X-Ide-Name", "WorkBuddy")
	header.Set("X-Ide-Type", "WorkBuddy")
	header.Set("X-Ide-Version", c.clientVersionFor(account5))
	header.Set("X-Codebuddy-Request", "1")
	// 链路族（traceparent/b3 单头 + X-B3-*），与 injectConversationHeaders 同口径。
	spanID := requestID[:16]
	parentSpanID := session.NewMessageID()[:16]

	header.Set("X-Trace-Id", requestID)
	header.Set("Traceparent", "00-"+requestID+"-"+spanID+"-01")
	header.Set("B3", requestID+"-"+spanID+"-1-"+parentSpanID)
	header.Set("X-B3-Traceid", requestID)
	header.Set("X-B3-Parentspanid", parentSpanID)
	header.Set("X-B3-Spanid", spanID)
	header.Set("X-B3-Sampled", "1")

	if expertID != "" {
		header.Set("X-Expert-Id", expertID)
	}
}

// debugChatRequest 调试输出请求 URL 与全部请求头（仅 WB2A_DEBUG_CHAT 非空时）。
//
//nolint:forbidigo // 调试专用输出：仅 WB2A_DEBUG_CHAT 显式开启时打印到终端，非库内调试残留。
func debugChatRequest(req *http.Request) {
	fmt.Printf("[dbg] URL=%s\n", req.URL)

	for k := range req.Header {
		fmt.Printf("[dbg] %s: %s\n", k, req.Header.Get(k))
	}
}

// readServerRequestID 从 SSE 流抓第一个形状合法的 data.id 作为服务端 requestId
// （读干流避免残留连接）。返回错误表示未找到。
func readServerRequestID(body io.Reader) (string, error) {
	buf := make([]byte, 0, 1<<20)
	tmp := make([]byte, 8192)
	searchFrom := 0

	for {
		n, rerr := body.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
			debugChatRead(tmp[:n])

			id, matched, next := scanBufferedRequestID(buf, searchFrom)
			if matched {
				return id, nil
			}

			searchFrom = next
		}

		if rerr != nil || len(buf) > 1<<20 {
			break
		}
	}

	return "", errSSERequestID
}

// scanBufferedRequestID 在 buf[searchFrom:] 中定位首个 `"id":"..."`：命中形状合法的 id
// 返回 (id, true, _)；命中但 id 不合法则推进搜索偏移（next）并返回 matched=false，
// 避免同一位置被反复命中。
//
// 从上次搜索位置继续：SSE 里 `"id":"` 可能先出现在消息 id 等字段，若不推进偏移，
// 首个不匹配的 id 会让循环永远重复命中同一位置，读满 1MB 后误报"未找到 requestId"。
func scanBufferedRequestID(buf []byte, searchFrom int) (id string, matched bool, next int) {
	i := bytes.Index(buf[searchFrom:], []byte(`"id":"`))
	if i < 0 {
		return "", false, searchFrom
	}

	abs := searchFrom + i
	rest := buf[abs+6:]

	end := bytes.IndexByte(rest, '"')
	if end <= 0 {
		return "", false, searchFrom
	}

	id = string(rest[:end])
	debugChatID(id)

	if idRegex.MatchString(id) {
		return id, true, searchFrom
	}

	return "", false, abs + 1
}

// debugChatRead 调试输出单次读取的字节（仅 WB2A_DEBUG_CHAT 非空时）。
//
//nolint:forbidigo // 调试专用输出：仅 WB2A_DEBUG_CHAT 显式开启时打印到终端，非库内调试残留。
func debugChatRead(chunk []byte) {
	if os.Getenv("WB2A_DEBUG_CHAT") == "" {
		return
	}

	dbg := string(chunk)
	if len(dbg) > 120 {
		dbg = dbg[:120]
	}

	fmt.Printf("[dbg-rd %d] %q\n", len(chunk), dbg)
}

// debugChatID 调试输出匹配到的 id（仅 WB2A_DEBUG_CHAT 非空时）。
//
//nolint:forbidigo // 调试专用输出：仅 WB2A_DEBUG_CHAT 显式开启时打印到终端，非库内调试残留。
func debugChatID(id string) {
	if os.Getenv("WB2A_DEBUG_CHAT") == "" {
		return
	}

	fmt.Printf("[dbg-id] %q match=%v\n", id, idRegex.MatchString(id))
}

// idRegex 服务端 requestId 形状（cmb- 前缀 32hex 或裸 32hex）。
var idRegex = regexp.MustCompile(`^(cmb-)?[0-9a-f]{32}$`)

// DesktopExpertSummonSequence 构造「召唤平台专家」事件组（expert_summon_click 等），
// 载荷对齐真实抓包样本（Sunny row 2644）。需配合 DesktopChatWithExpert +
// DesktopExpertActualUseEvent 完成一次完整「召唤+使用」。
func DesktopExpertSummonSequence(marketExpert *MarketExpert) []DesktopEvent {
	cat := "expert-all"

	if len(marketExpert.Categories) > 0 {
		if s, ok := marketExpert.Categories[0].(string); ok {
			cat = s
		}
	}

	ver := marketExpert.Version
	if ver == "" {
		ver = "1.0.0"
	}

	return []DesktopEvent{
		{
			"eventCode": "web_element_click", "source": marketExpert.ExpertID, "type": cat, "version": ver,
			"elementId": "expert_summon_click", "elementName": "立即召唤",
			"pageURL": "/C:/Program%20Files/WorkBuddy/resources/app.asar/renderer/index.html",
		},
		{
			"eventCode": "expert_summon_click", "id": marketExpert.ExpertID, "name": marketExpert.DisplayNameZH,
			"expertTitle": marketExpert.ProfessionZH, "type": "expert-all", "position": 0,
			"expertType": marketExpert.ExpertType, "version": ver, "mode": desktopLocal,
		},
		{
			"eventCode": "expert_summoned", "id": marketExpert.ExpertID, "name": marketExpert.DisplayNameZH,
			"expertTitle": marketExpert.ProfessionZH, "type": "expert-all",
		},
	}
}

// DesktopExpertActualUseEvent 构造「专家真实使用」事件（expert_5/Expert_team_use_3 计数）。
// requestID 必须是 DesktopChatWithExpert 返回的服务端 requestId。
func DesktopExpertActualUseEvent(e *MarketExpert, conversationID, requestID string) DesktopEvent {
	ev := desktopExpertActualUse(e, conversationID, requestID)
	ev["mode"] = "craft"

	return ev
}

// DesktopExpertActualUseLocal mode:"LOCAL" 变体（Expert_lighthouse 判据要求 LOCAL，
// 对齐真实样本 Sunny row 868：轻量云专家使用时 mode=LOCAL、type 为空、cost=0）。
func DesktopExpertActualUseLocal(e *MarketExpert, conversationID, requestID string) DesktopEvent {
	ev := desktopExpertActualUse(e, conversationID, requestID)
	ev["mode"] = desktopLocal

	return ev
}

// desktopExpertActualUse expert_actual_use 公共载荷。
func desktopExpertActualUse(marketExpert2 *MarketExpert, conversationID, requestID string) DesktopEvent {
	cat := "expert-all"

	if len(marketExpert2.Categories) > 0 {
		if s, ok := marketExpert2.Categories[0].(string); ok {
			cat = s
		}
	}

	ver := marketExpert2.Version
	if ver == "" {
		ver = "1.0.0"
	}

	return DesktopEvent{
		"eventCode": "expert_actual_use",
		"id":        marketExpert2.ExpertID, "name": marketExpert2.DisplayNameZH, "expertTitle": marketExpert2.ProfessionZH,
		"type": cat, "expertType": marketExpert2.ExpertType, "source": "builtin", "version": ver,
		"cost": 9000, "characterCount": 14,
		"conversationId": conversationID, "requestId": requestID, "messageId": "msg-" + requestID[len(requestID)-8:],
		"requestModelId": "fast-model", "requestModelName": "fast-model",
	}
}
