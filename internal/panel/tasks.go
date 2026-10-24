// tasks.go 面板「积分任务」接口：查询任务进度、接受任务、领取奖励。
//
// 上游能力（internal/upstream/tasks.go）的三层薄封装；前端表格展示
// current/target 进度与可领取状态，运维点按钮即可，无需外部 Python 脚本。
package panel

import (
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
	"github.com/BegoniaHe/wokbuddy-demon/internal/upstream"
)

// acceptBatchGap 批量接受的批间节流（对齐脚本 1.05s 口径，避免上游风控）。
var acceptBatchGap = 1050 * time.Millisecond

// pendingTaskCodes 收集尚未接受且未完成的任务 code（跳过已领取/已锁定/已接受/已完成）。
func pendingTaskCodes(tasks []upstream.Task) []string {
	var codes []string

	for i := range tasks {
		if t := &tasks[i]; !t.Claimed && !t.Locked && t.AcceptStatus != "accepted" && t.AcceptStatus != "completed" {
			codes = append(codes, t.TaskCode)
		}
	}

	return codes
}

// claimTaskReward 领取任务奖励（mp 专属码走 chat 域 mp 口径，其余 Web 端接口）。
func (p *Panel) claimTaskReward(account *auth.Auth, code string) (int64, int64, error) {
	if isMPTaskCode(code) {
		credit, energy, err := p.cfg.Upstream.ClaimRewardMP(account, code)
		if err != nil {
			return 0, 0, fmt.Errorf("claimTaskReward: %w", err)
		}

		return credit, energy, nil
	}

	credit, energy, err := p.cfg.Upstream.ClaimReward(account, code)
	if err != nil {
		return 0, 0, fmt.Errorf("claimTaskReward: %w", err)
	}

	return credit, energy, nil
}

// decodeTaskCode 解析请求体的 task_code；缺失时写 400 并返回 false。
func decodeTaskCode(w http.ResponseWriter, r *http.Request) (string, bool) {
	var body struct {
		TaskCode string `json:"task_code"`
	}
	if err := decodeJSONBody(r.Body, &body); err != nil || body.TaskCode == "" {
		writeErr(w, http.StatusBadRequest, "task_code required")
		return "", false
	}

	return body.TaskCode, true
}

// taskDenyReason 报告账号是否不具备 CN 成长/任务体系，返回给用户的跳过原因；
// 空串表示可用（issue #108）。集中表达两条门控，避免各单号入口各写一份：
//   - global（国际版）账号没有 CN 成长/旅行/夜猫子任务（scheduler 对 global
//     直接跳过这些任务；签到除外，见 checkinDenyReason）；
//   - 企业版没有个人成长/签到体系（上游 400 code 10001「企业账号不支持该操作」，
//     见 auth.IsEnterprise 注释）。
//
// 与 taskcenter 的扫描/队列门控同口径（IsGlobal() || IsEnterprise()）。
// token_source 只是认证方法，不参与账号身份判定。
func taskDenyReason(account *auth.Auth) string {
	switch {
	case account.IsGlobal():
		return "国际版（global）账号没有 CN 成长/任务体系，已跳过"
	case account.IsEnterprise():
		return "企业版账号没有个人签到/成长任务体系，已跳过"
	default:
		return ""
	}
}

// checkinDenyReason 报告账号是否不具备签到能力。与 taskDenyReason 的区别：
// global 账号支持签到（调度器与 upstream.DailyCheckin 均按 realm 分流），
// 只有企业版没有个人签到体系。
func checkinDenyReason(a *auth.Auth) string {
	if a.IsEnterprise() {
		return "企业版账号没有个人签到体系，已跳过"
	}

	return ""
}

// denyTask 写「账号类型不支持 CN 任务体系」的 409 跳过响应（issue #108）。
// 不发任何上游请求：用户点到不可用入口时得到明确原因，而不是上游错误。
func denyTask(w http.ResponseWriter, reason string) {
	writeJSON(w, http.StatusConflict, map[string]any{
		"ok":      false,
		"skipped": true,
		"error":   reason,
	})
}

// eligibleTaskAccount 取账号并做任务能力门控（issue #108）：不存在写 404、
// global/企业版写 409 跳过，两者都返回 nil。CN 个人号原样返回。
func (p *Panel) eligibleTaskAccount(w http.ResponseWriter, uid string) *auth.Auth {
	account := p.accountByUID(w, uid)
	if account == nil {
		return nil
	}

	if reason := taskDenyReason(account); reason != "" {
		denyTask(w, reason)
		return nil
	}

	return account
}

// accountByUID 取账号凭证；不存在时写 404 并返回 nil。
func (p *Panel) accountByUID(w http.ResponseWriter, uid string) *auth.Auth {
	account := p.cfg.Pool.AuthByUID(uid)
	if account == nil {
		writeErr(w, http.StatusNotFound, "account not found")
		return nil
	}

	return account
}

// accountTasks 查询单账号全量任务（进度/状态/可领取）。
func (p *Panel) accountTasks(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")

	account := p.accountByUID(w, uid)
	if account == nil {
		return
	}

	if reason := taskDenyReason(account); reason != "" {
		// 查询类：返回空列表 + 明确跳过原因（200），前端无需按错误处理。
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": true, "skipped": true, "reason": reason, "tasks": []any{},
		})

		return
	}

	tasks, err := p.cfg.Upstream.ListTasks(account)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "list tasks: "+err.Error())
		return
	}
	// 合并小程序口径任务（school_season / Sequential_Tasks_1 等仅在 mp 头列表下发）。
	// mp 列表是默认口径的超集（实测含常规任务），按 task_code 去重；失败静默。
	if mpTasks, mpErr := p.cfg.Upstream.ListTasksMP(account); mpErr == nil {
		seen := map[string]bool{}
		for i := range tasks {
			seen[tasks[i].TaskCode] = true
		}

		for i := range mpTasks {
			if !seen[mpTasks[i].TaskCode] {
				tasks = append(tasks, mpTasks[i])
				seen[mpTasks[i].TaskCode] = true
			}
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "tasks": tasks})
}

// accountTaskAccept 接受任务（报名；幂等）。
func (p *Panel) accountTaskAccept(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")

	account := p.eligibleTaskAccount(w, uid)
	if account == nil {
		return
	}

	var body struct {
		TaskCodes []string `json:"task_codes"`
	}
	if err := decodeJSONBody(r.Body, &body); err != nil || len(body.TaskCodes) == 0 {
		writeErr(w, http.StatusBadRequest, "task_codes required")
		return
	}

	if err := p.cfg.Upstream.AcceptTasks(account, body.TaskCodes); err != nil {
		writeErr(w, http.StatusBadGateway, "accept: "+err.Error())
		return
	}

	log.Printf("panel: 接受任务 uid=%s codes=%v", uid, body.TaskCodes)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// taskAcceptAll 接受该账号全部尚未接受的任务（跳过已 accepted/claimed 的）。
//
// 为什么值得做：accept 不产生进度（进度靠行为事件点亮），但让状态机规范
// （not_accepted → accepted → completed），也便于后续筛选"我报过名的任务"。
// 上游 scripts 的注释同样建议"先 accept"。
// 批量提交会分片（上游对 task_codes 数组长度无公开上限，保守每批 20 个）。
func (p *Panel) taskAcceptAll(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")

	account := p.eligibleTaskAccount(w, uid)
	if account == nil {
		return
	}

	tasks, err := p.cfg.Upstream.ListTasks(account)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "list tasks: "+err.Error())
		return
	}

	codes := pendingTaskCodes(tasks)
	if len(codes) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "accepted": 0, "message": "所有任务均已接受"})
		return
	}

	accepted, failed := p.acceptCodesInBatches(account, uid, codes)
	// 小程序口径任务单独批量接受（默认列表不含 mp 码，accept 也要求 mp 头）。
	mpAccepted, mpFailed := p.acceptMPPending(account, uid)
	accepted += mpAccepted

	failed = append(failed, mpFailed...)
	log.Printf("panel: 全部接受 uid=%s 接受=%d 失败=%d", uid, accepted, len(failed))

	resp := map[string]any{"ok": true, "accepted": accepted, "failed": failed}
	if len(failed) > 0 {
		resp["message"] = "部分任务接受失败（上游拒绝），可重试"
	}

	writeJSON(w, http.StatusOK, resp)
}

// acceptCodesInBatches 分片接受任务 code（每批 20，批间节流），返回接受数与失败 code。
func (p *Panel) acceptCodesInBatches(account2 *auth.Auth, uid string, codes []string) (int, []string) {
	const batch = 20

	accepted := 0

	var failed []string

	for i := 0; i < len(codes); i += batch {
		end := min(i+batch, len(codes))
		if err := p.cfg.Upstream.AcceptTasks(account2, codes[i:end]); err != nil {
			log.Printf("panel: 批量接受失败 uid=%s codes=%v err=%v", uid, codes[i:end], err)
			failed = append(failed, codes[i:end]...)

			continue
		}

		accepted += end - i

		time.Sleep(acceptBatchGap) // 批间节流（对齐脚本 1.05s 口径）
	}

	return accepted, failed
}

// acceptMPPending 批量接受小程序口径未接受任务；返回接受数与失败 code（列表失败静默）。
func (p *Panel) acceptMPPending(account3 *auth.Auth, uid string) (int, []string) {
	mpTasks, err := p.cfg.Upstream.ListTasksMP(account3)
	if err != nil {
		return 0, nil
	}

	codes := pendingTaskCodes(mpTasks)
	if len(codes) == 0 {
		return 0, nil
	}

	if err := p.cfg.Upstream.AcceptTasksMP(account3, codes); err != nil {
		log.Printf("panel: mp 批量接受失败 uid=%s err=%v", uid, err)
		return 0, codes
	}

	time.Sleep(acceptBatchGap)

	return len(codes), nil
}

// accountTaskClaim 领取任务奖励（未达标时上游返回业务错误，原样透出给前端提示）。
func (p *Panel) accountTaskClaim(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")

	account := p.eligibleTaskAccount(w, uid)
	if account == nil {
		return
	}

	var body struct {
		TaskCode string `json:"task_code"`
	}
	if err := decodeJSONBody(r.Body, &body); err != nil || body.TaskCode == "" {
		writeErr(w, http.StatusBadRequest, "task_code required")
		return
	}
	// 小程序口径任务走 chat 域 mp 头领奖（缺头实测不可领）；其余 Web 端接口。
	credit, energy, err := p.claimTaskReward(account, body.TaskCode)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "claim: "+err.Error())
		return
	}

	if credit == 0 && energy == 0 {
		log.Printf("panel: 领取任务奖励 uid=%s code=%s（已领取过，无新增）", uid, body.TaskCode)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "already_claimed": true, "message": "该奖励此前已领取"})

		return
	}

	log.Printf("panel: 领取任务奖励 uid=%s code=%s +%d分 +%d能", uid, body.TaskCode, credit, energy)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "credit": credit, "energy": energy})
}
