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

	tasks, err := p.claimableTasks(account)
	if err != nil {
		stats.Failed = 1

		log.Printf("WARN: [panel] 任务补领清扫 acct=%s: 拉取任务列表失败: %v",
			logfmt.Label(account.UID, account.NicknameValue()), err)

		return stats
	}

	stats.Scanned = 1
	stats.Claimed, stats.Credit, stats.Energy, stats.Failed = p.claimAll(account, tasks, ctx.Err)

	if stats.Claimed > 0 {
		log.Printf("INFO: [panel] 任务补领清扫 acct=%s: 补领 %d 项 +%d 分 +%d 能",
			logfmt.Label(account.UID, account.NicknameValue()), stats.Claimed, stats.Credit, stats.Energy)
	}

	return stats
}

// claimAll 逐项补领（账号内串行，ctx 取消后不再继续）。
func (p *Panel) claimAll(account *auth.Auth, tasks []upstream.Task, canceled func() error) (claimed int, credit, energy int64, failed int) {
	for i := range tasks {
		if canceled() != nil {
			return claimed, credit, energy, failed
		}

		taskCredit, taskEnergy, err := p.claimTaskReward(account, tasks[i].TaskCode)
		if err != nil {
			failed++

			log.Printf("WARN: [panel] 任务补领清扫 acct=%s task=%s: 补领失败: %v",
				logfmt.Label(account.UID, account.NicknameValue()), tasks[i].TaskCode, err)

			continue
		}

		claimed++

		credit += taskCredit
		energy += taskEnergy
	}

	return claimed, credit, energy, failed
}

// claimableTasks 返回该账号「已达标未领」的任务：默认口径与小程序口径各拉一次，
// 按 task_code 去重合并（mp 列表是默认口径的超集，且 mp 专属任务只在 mp 口径下发）。
// 单口径失败不致命：另一口径仍可照常补领。
func (p *Panel) claimableTasks(account *auth.Auth) ([]upstream.Task, error) {
	primary, primaryErr := p.cfg.Upstream.ListTasks(account)
	if primaryErr != nil {
		primary = nil
	}

	secondary, mpErr := p.cfg.Upstream.ListTasksMP(account)
	if mpErr != nil {
		secondary = nil
	}

	if primaryErr != nil && mpErr != nil {
		return nil, fmt.Errorf("claimableTasks: %w", primaryErr)
	}

	merged := make([]upstream.Task, 0, len(primary)+len(secondary))

	seen := make(map[string]bool, len(primary)+len(secondary))

	for _, list := range [][]upstream.Task{primary, secondary} {
		for i := range list {
			task := &list[i]
			// 先判 Claimable 再去重：两口径对同一 task_code 的进度/领取状态可能
			// 瞬时不一致，先出现的「不可领」条目不能把后出现的「可领」条目吃掉。
			if task.TaskCode == "" || !task.Claimable || seen[task.TaskCode] {
				continue
			}

			seen[task.TaskCode] = true

			merged = append(merged, *task)
		}
	}

	return merged, nil
}
