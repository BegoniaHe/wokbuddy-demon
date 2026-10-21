// compensation.go 面板「官方补偿」接口：只读状态 + 逐号/全量领取。
//
// 取数是**显式触发**的（进页面 / 点按钮），不挂 5s 轮询：补偿状态没有 overview 快照
// 可吃，挂进轮询就是每 5 秒对每账号轰炸一次上游。
package panel

import (
	"net/http"

	"github.com/BegoniaHe/wokbuddy-demon/internal/scheduler"
)

// compensationStatus GET /panel/api/compensation —— 全部账号的补偿状态（只读）。
func (p *Panel) compensationStatus(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Scheduler == nil {
		writeErr(w, http.StatusNotImplemented, "scheduler disabled")

		return
	}

	rows := p.cfg.Scheduler.CompensationStatusAll(r.Context())
	writeJSON(w, http.StatusOK, map[string]any{"accounts": rows, "summary": scheduler.SummarizeCompensation(rows)})
}

// accountCompensation POST /panel/api/accounts/{uid}/compensation —— 单账号先查后领。
func (p *Panel) accountCompensation(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Scheduler == nil {
		writeErr(w, http.StatusNotImplemented, "scheduler disabled")

		return
	}

	uid := r.PathValue("uid")
	if uid == "" {
		writeErr(w, http.StatusBadRequest, "uid required")

		return
	}

	row := p.cfg.Scheduler.ClaimCompensationFor(uid)
	writeJSON(w, http.StatusOK, map[string]any{"ok": row.Error == "", "account": row})
}

// compensationAll POST /panel/api/compensation_all —— 全量先查后领。
//
// 返回 summary{claimed,skipped,errored,credit}：**调用方必须先看 errored**，
// 否则上游全挂时会把故障说成「当前没有待领补偿」。
func (p *Panel) compensationAll(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Scheduler == nil {
		writeErr(w, http.StatusNotImplemented, "scheduler disabled")

		return
	}

	rows, summary := p.cfg.Scheduler.ClaimCompensationNow()
	writeJSON(w, http.StatusOK, map[string]any{"accounts": rows, "summary": summary})
}
