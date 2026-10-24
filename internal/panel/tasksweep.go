// tasksweep.go 周期幂等补领「已达标未领」的成长任务奖励（上游同步窗口 issue #196，
// 对应上游 issue #174 的主体链路）。
//
// 与 #195 的关系：#195 修的是「一键完成」入口遇到已达标未领时的补领；本文件把同一口径
// 做成周期任务，不再依赖人工点按钮。两者共用 claimTaskReward（mp/Web 分流）。
package panel

import (
	"context"
	"fmt"
	"log"
	"sync"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
	"github.com/BegoniaHe/wokbuddy-demon/internal/logfmt"
	"github.com/BegoniaHe/wokbuddy-demon/internal/pool"
	"github.com/BegoniaHe/wokbuddy-demon/internal/upstream"
)

// taskSweepWorkers 账号间并发上限。取 3：清扫是低频后台任务（默认 15 分钟一轮），
// 不需要像余额刷新那样 5 并发；压低上限也降低与手动任务的撞车概率。
const taskSweepWorkers = 3

// TaskSweepStats 一轮清扫的汇总（调度器日志与面板观测用）。
type TaskSweepStats struct {
	Accounts int   `json:"accounts"` // 参与扫描的账号数（已排除 global/企业版/禁用）
	Busy     int   `json:"busy"`     // 忙账号整轮跳过数
	Scanned  int   `json:"scanned"`  // 实际拉过任务列表的账号数
	Accepted int   `json:"accepted"` // 本轮接受成功的任务数
	Claimed  int   `json:"claimed"`  // 补领成功次数
	Failed   int   `json:"failed"`   // 补领/拉列表失败次数
	Credit   int64 `json:"credit"`   // 本轮补领积分合计
	Energy   int64 `json:"energy"`   // 本轮补领能量合计
}

// sweepAccumulator 并发汇总（账号间并发写同一份统计）。
type sweepAccumulator struct {
	mu    sync.Mutex
	stats TaskSweepStats
}

// add 累加一份账号级统计。
func (a *sweepAccumulator) add(delta TaskSweepStats) {
	a.mu.Lock()
	defer a.mu.Unlock()

	a.stats.Busy += delta.Busy
	a.stats.Scanned += delta.Scanned
	a.stats.Accepted += delta.Accepted
	a.stats.Claimed += delta.Claimed
	a.stats.Failed += delta.Failed
	a.stats.Credit += delta.Credit
	a.stats.Energy += delta.Energy
}

// RunTaskSweep 扫全部非禁用账号，幂等补领「已达标未领」的奖励：
//   - 账号间并发（上限 taskSweepWorkers），账号内串行；
//   - global / 企业版账号零上游请求（CN 成长体系门控，与单号入口同口径 taskDenyReason）；
//   - 忙账号（有手动任务动作在执行）整轮跳过，不与手动操作抢同一账号；
//   - 幂等：只领 Claimable（进度达标且未领取）的任务，第二轮因已领取而零 claim。
func (p *Panel) RunTaskSweep(ctx context.Context) TaskSweepStats {
	list := p.cfg.Pool.List()

	var (
		wg    sync.WaitGroup
		accum sweepAccumulator
		sem   = make(chan struct{}, taskSweepWorkers)
	)

	accounts := 0

loop:
	for i := range list {
		account, ok := p.sweepCandidate(&list[i])
		if !ok {
			continue
		}

		// 先取名额再起 goroutine：账号多时不在信号量上堆积阻塞协程。
		select {
		case sem <- struct{}{}:
			if ctx.Err() != nil {
				<-sem

				break loop
			}
		case <-ctx.Done():
			break loop
		}

		accounts++

		wg.Add(1)

		go func(acct *auth.Auth) {
			defer wg.Done()
			defer func() { <-sem }()

			accum.add(p.sweepAccount(ctx, acct))
		}(account)
	}

	wg.Wait()

	stats := accum.stats
	stats.Accounts = accounts

	return stats
}

// sweepCandidate 判定账号是否参与本轮清扫（禁用 / 无 CN 成长体系 / 已删除 → 跳过）。
func (p *Panel) sweepCandidate(status *pool.Status) (*auth.Auth, bool) {
	if status.Disabled {
		return nil, false
	}

	account := p.cfg.Pool.AuthByUID(status.UID)
	if account == nil {
		return nil, false
	}

	// 没有 CN 成长体系的账号（global / 企业版）零上游请求。
	if taskDenyReason(account) != "" {
		return nil, false
	}

	return account, true
}

// sweepAccount 单账号清扫（账号内串行）：忙账号整轮跳过，其余拉双口径任务列表后补领。
func (p *Panel) sweepAccount(ctx context.Context, account *auth.Auth) TaskSweepStats {
	stats := TaskSweepStats{}

	// 账号级互斥：与手动「一键完成」/单任务入口共用同一把锁，抢不到就整轮跳过。
	if !p.tryLockAccount(account.UID) {
		stats.Busy = 1

		return stats
	}
	defer p.unlockAccount(account.UID)

	tasks, err := p.sweepTasks(account)
	if err != nil {
		stats.Failed = 1

		log.Printf("WARN: [panel] 任务补领清扫 acct=%s: 拉取任务列表失败: %v",
			logfmt.Label(account.UID, account.NicknameValue()), err)

		return stats
	}

	stats.Scanned = 1
	// 先接受未接受任务：进度已达标但未接受的任务，直接 claim 会被上游拒绝
	// （task not accepted），奖励仍然领不到。接受不是「代做任务」。
	stats.Accepted, stats.Failed = p.acceptSweepTasks(account, tasks)

	var failed int

	stats.Claimed, stats.Credit, stats.Energy, failed = p.claimAll(account, tasks, ctx.Err)
	stats.Failed += failed

	if stats.Claimed > 0 {
		log.Printf("INFO: [panel] 任务补领清扫 acct=%s: 补领 %d 项 +%d 分 +%d 能",
			logfmt.Label(account.UID, account.NicknameValue()), stats.Claimed, stats.Credit, stats.Energy)
	}

	return stats
}

// claimAll 逐项补领（账号内串行，ctx 取消后不再继续）。
func (p *Panel) claimAll(
	account *auth.Auth, tasks []sweepTask, canceled func() error,
) (claimed int, credit, energy int64, failed int) {
	for i := range tasks {
		if canceled() != nil {
			return claimed, credit, energy, failed
		}

		task := &tasks[i]
		if !task.Task.Claimable || task.Task.Claimed || task.Task.Locked {
			continue
		}

		taskCredit, taskEnergy, err := p.claimSweepTask(account, task)
		if err != nil {
			failed++

			log.Printf("WARN: [panel] 任务补领清扫 acct=%s task=%s: 补领失败: %v",
				logfmt.Label(account.UID, account.NicknameValue()), task.Task.TaskCode, err)

			continue
		}

		claimed++

		credit += taskCredit
		energy += taskEnergy
	}

	return claimed, credit, energy, failed
}

// claimSweepTask 按任务来源（默认/mp 列表）选择领奖端点。mp 列表下发的任务必须
// 带 mp 头，否则上游返回 task not found；共享任务仍走 Web 端点。
func (p *Panel) claimSweepTask(account *auth.Auth, task *sweepTask) (int64, int64, error) {
	if task.MP || isMPTaskCode(task.Task.TaskCode) {
		credit, energy, err := p.cfg.Upstream.ClaimRewardMP(account, task.Task.TaskCode)
		if err != nil {
			return 0, 0, fmt.Errorf("claimSweepTask: %w", err)
		}

		return credit, energy, nil
	}

	credit, energy, err := p.cfg.Upstream.ClaimReward(account, task.Task.TaskCode)
	if err != nil {
		return 0, 0, fmt.Errorf("claimSweepTask: %w", err)
	}

	return credit, energy, nil
}

// shouldAcceptTask 报告任务是否需要先接受：未领、未锁定、且尚未 accepted/completed。
func shouldAcceptTask(task *upstream.Task) bool {
	if task.TaskCode == "" || task.Claimed || task.Locked {
		return false
	}

	return task.AcceptStatus != "accepted" && task.AcceptStatus != "completed"
}

// acceptSweepTasks 接受尚未接受的任务（默认/mp 口径分开提交，端标记不可混用）。
// 失败只记日志并计入 Failed；接受失败时仍继续尝试领取（既已接受的任务不受影响）。
func (p *Panel) acceptSweepTasks(account *auth.Auth, tasks []sweepTask) (accepted, failed int) {
	var webCodes, mpCodes []string

	for i := range tasks {
		if !shouldAcceptTask(&tasks[i].Task) {
			continue
		}

		if tasks[i].MP || isMPTaskCode(tasks[i].Task.TaskCode) {
			mpCodes = append(mpCodes, tasks[i].Task.TaskCode)
		} else {
			webCodes = append(webCodes, tasks[i].Task.TaskCode)
		}
	}

	acceptedWeb, failedWeb := p.acceptCodes(account, webCodes, false)
	acceptedMP, failedMP := p.acceptCodes(account, mpCodes, true)

	return acceptedWeb + acceptedMP, failedWeb + failedMP
}

// acceptCodes 提交一批同端任务的接受请求；返回成功条数与失败次数（0/1）。
func (p *Panel) acceptCodes(account *auth.Auth, codes []string, isMP bool) (accepted, failed int) {
	if len(codes) == 0 {
		return 0, 0
	}

	var err error
	if isMP {
		err = p.cfg.Upstream.AcceptTasksMP(account, codes)
	} else {
		err = p.cfg.Upstream.AcceptTasks(account, codes)
	}

	if err != nil {
		log.Printf("WARN: [panel] 任务补领清扫 acct=%s: 接受任务失败（mp=%v）: %v",
			logfmt.Label(account.UID, account.NicknameValue()), isMP, err)

		return 0, 1
	}

	return len(codes), 0
}

// sweepTask 清扫任务及其来源口径：mp 列表下发的任务 accept/claim 都要带 mp 头。
type sweepTask struct {
	Task upstream.Task
	MP   bool
}

// sweepTasks 返回该账号的默认 + 小程序两口径任务（按 task_code 去重合并）。
// 合并时优先保留「可领」的同名条目（两口径的进度/领取状态可能瞬时不一致）；共享
// 任务保留默认口径的来源标记，只有 mp 列表下发的任务标记 MP。
// 单口径失败不致命：另一口径仍可照常接受/补领。
func (p *Panel) sweepTasks(account *auth.Auth) ([]sweepTask, error) {
	primary, primaryErr := p.cfg.Upstream.ListTasks(account)
	if primaryErr != nil {
		primary = nil
	}

	secondary, mpErr := p.cfg.Upstream.ListTasksMP(account)
	if mpErr != nil {
		secondary = nil
	}

	if primaryErr != nil && mpErr != nil {
		return nil, fmt.Errorf("sweepTasks: %w", primaryErr)
	}

	return mergeSweepTasks(primary, secondary), nil
}

// mergeSweepTasks 合并默认/mp 两口径任务：按 task_code 去重，优先保留「可领」的
// 同名条目；共享任务保留默认口径的来源标记（领奖走 Web 端点），只有 mp 列表
// 下发的任务标记 MP（accept/claim 必须带 mp 头）。
func mergeSweepTasks(primary, secondary []upstream.Task) []sweepTask {
	merged := make([]sweepTask, 0, len(primary)+len(secondary))

	index := make(map[string]int, len(primary)+len(secondary))

	for _, list := range []struct {
		tasks []upstream.Task
		mp    bool
	}{{primary, false}, {secondary, true}} {
		for i := range list.tasks {
			task := &list.tasks[i]
			if task.TaskCode == "" {
				continue
			}

			if idx, ok := index[task.TaskCode]; ok {
				if !merged[idx].Task.Claimable && task.Claimable {
					merged[idx].Task = *task
				}

				continue
			}

			index[task.TaskCode] = len(merged)

			merged = append(merged, sweepTask{Task: *task, MP: list.mp})
		}
	}

	return merged
}
