// blackcat.go 夜猫子任务执行器：23:00–08:00 窗口内对池内账号补足 glm-5.2 对话
// 并上报事件链（black_cat 判据）。窗口外触发则直接跳过（只观测，不报错）。
package scheduler

import (
	"context"
	"log"
	"time"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
	"github.com/BegoniaHe/wokbuddy-demon/internal/logfmt"
	"github.com/BegoniaHe/wokbuddy-demon/internal/upstream"
)

// now 返回窗口判定时钟：Scheduler 可注入固定时刻，零值回退 time.Now。
func (s *Scheduler) now() time.Time {
	if s.nowFn != nil {
		return s.nowFn()
	}

	return time.Now()
}

// RunBlackcatNow 对所有可用账号执行夜猫子对话补足（窗口外跳过）。
// 由 blackcat_hours 排程（默认 [23]）触发；执行前二次校验 InNightWindow。
func (s *Scheduler) RunBlackcatNow() {
	s.runTaskSync(context.Background(), TaskBlackcat, TriggerManual)
}

// blackcatAll 夜猫子实体：返回尝试/成功/失败账号数（窗口外/门控跳过不计数）。
func (s *Scheduler) blackcatAll(ctx context.Context) (total, success, failure int) {
	if !upstream.InNightWindow(s.now()) {
		log.Printf("blackcat: 当前不在 23:00–08:00 计数窗口，跳过")
		return 0, 0, 0
	}

	all := s.cfg.Pool.List()
	targets := make([]batchTarget, 0, len(all))

	for i := range all {
		status := &all[i]
		// 暂停选号（paused）账号跳过：夜猫子是全任务体系中唯一「整任务都是
		// 真实模型对话」的（RunNightChats 逐条 ChatStream 发 glm-5.2 短对话），
		// 与「让位防风控」正面冲突。旅行/成长任务都是纯上报或领奖 RPC，照常跑。
		if status.Disabled || status.Paused {
			continue
		}

		account := s.cfg.Pool.AuthByUID(status.UID)
		if account == nil || account.AccessTokenValue() == "" {
			continue
		}

		if account.IsGlobal() {
			continue // D4 门控：global 无 CN 任务体系，不发起任何上游调用
		}

		if account.IsEnterpriseSession() {
			continue // 企业版 / 企业切换会话门控：无成长体系（growth 403 / claim-gift 400「企业账号不支持该操作」）
		}

		targets = append(targets, batchTarget{uid: account.UID, run: func() bool { return s.blackcatOne(account) }})
	}

	return s.runPacedBatch(ctx, taskBlackcat, activityAccountDelay, targets)
}

// blackcatOne 单账号夜猫子补足：查需补的对话数 → 跑夜间对话；已补足（need<=0）则跳过。
// 返回 false 仅表示上游查询/执行失败；已补足属正常跳过。
func (s *Scheduler) blackcatOne(account *auth.Auth) bool {
	need, err := s.cfg.Upstream.BlackcatNeed(account)
	if err != nil {
		log.Printf("blackcat %s: %v", logfmt.Label(account.UID, account.NicknameValue()), err)
		return false
	}

	if need <= 0 {
		return true
	}

	ok, err := s.cfg.Upstream.RunNightChats(account, int(need))
	if err != nil {
		log.Printf("blackcat %s: %d/%d 完成，中断: %v", logfmt.Label(account.UID, account.NicknameValue()), ok, need, err)
		return false
	}

	log.Printf("blackcat %s: 完成 %d 次夜间对话", logfmt.Label(account.UID, account.NicknameValue()), ok)

	return true
}
