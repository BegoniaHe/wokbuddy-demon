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

// maxNightChatsPerRound 单账号单轮夜猫子对话上限（clampNightChats）。
//
// need = target-current 直接来自上游任务列表，而每次对话是一条真实 chat + 4s
// 间隔：一次异常或被篡改的响应就能把账号的在途租约占住数天、并对同一账号连打
// 上万次上游请求（与抽奖次数同一类放大面）。正常任务是个位数量级，超过即视为
// 上游数据异常，本轮做到上限即停并留痕。
const maxNightChatsPerRound = 20

// clampNightChats 把上游返回的待补对话数钳到 [0, maxNightChatsPerRound]；
// truncated=true 表示被钳制，调用方据此打一条 WARN。
func clampNightChats(need int64) (clamped int, truncated bool) {
	if need <= 0 {
		return 0, false
	}

	if need > maxNightChatsPerRound {
		return maxNightChatsPerRound, true
	}

	return int(need), false
}

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

		if account.IsEnterprise() {
			continue // 企业版门控：无个人成长体系（growth 403 / claim-gift 400「企业账号不支持该操作」）
		}

		targets = append(targets, batchTarget{
			uid: account.UID,
			run: func() bool { return s.blackcatOne(ctx, account) },
		})
	}

	return s.runPacedBatch(ctx, taskBlackcat, activityAccountDelay, targets)
}

// blackcatOne 单账号夜猫子补足：查需补的对话数 → 跑夜间对话；已补足（need<=0）则跳过。
// 返回 false 仅表示上游查询/执行失败；已补足属正常跳过。
func (s *Scheduler) blackcatOne(ctx context.Context, account *auth.Auth) bool {
	// 在途名额与网关流量同源（max_in_flight / max_in_flight_global）：夜猫子会对
	// 全部账号跑真实对话，不取租约就可能在同号上叠加超出上限的上游并发，正是
	// 分档要避免的形态。拿不到名额属正常跳过（下一轮再试），不算失败。
	if !s.cfg.Pool.Acquire(account.UID) {
		log.Printf("blackcat %s: 在途名额已满，本轮跳过",
			logfmt.Label(account.UID, account.NicknameValue()))

		return true
	}

	defer s.cfg.Pool.Release(account.UID)

	need, err := s.cfg.Upstream.BlackcatNeed(account)
	if err != nil {
		log.Printf("blackcat %s: %v", logfmt.Label(account.UID, account.NicknameValue()), err)
		return false
	}

	if need <= 0 {
		return true
	}

	want, truncated := clampNightChats(need)
	if truncated {
		log.Printf("WARN: blackcat %s: 上游返回待补对话数 %d 超过单轮上限 %d，本轮只做 %d 次",
			logfmt.Label(account.UID, account.NicknameValue()), need, want, want)
	}

	ok, err := s.cfg.Upstream.RunNightChats(ctx, account, want)
	if err != nil {
		log.Printf("blackcat %s: %d/%d 完成，中断: %v", logfmt.Label(account.UID, account.NicknameValue()), ok, need, err)
		return false
	}

	log.Printf("blackcat %s: 完成 %d 次夜间对话", logfmt.Label(account.UID, account.NicknameValue()), ok)

	return true
}
