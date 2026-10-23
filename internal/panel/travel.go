// travel.go 面板按号旅行入口（issue #257）。
package panel

import "net/http"

// accountTravel POST /panel/api/accounts/{uid}/travel：对单个账号执行一趟旅行巡检。
// 复用调度器门控（disabled/global/enterprise/paused）与既有账号锁。
func (p *Panel) accountTravel(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Scheduler == nil {
		writeErr(w, http.StatusNotImplemented, "scheduler disabled")

		return
	}

	uid := r.PathValue("uid")
	if _, ok := p.cfg.Pool.Status(uid); !ok {
		writeErr(w, http.StatusNotFound, "account not found")

		return
	}

	if !p.tryLockAccount(uid) {
		writeErr(w, http.StatusConflict, "该账号有任务动作正在执行中，请等本轮结束后再试")

		return
	}
	defer p.unlockAccount(uid)

	result := p.cfg.Scheduler.RunTravelNowFor(uid)
	writeJSON(w, http.StatusOK, result)
}
