// streak.go 连登管家：签到排程后自动检查连登兑换档位 → 可兑换即兑换 → 按抽奖次数抽奖。
//
// 背景（2026-09-12）：成长中心连登档位（7d/14d/28d）按连续登录天数解锁，兑换发
// credit/energy/补签卡/抽奖次数；抽奖次数只能从兑换获得。兑换按钮在 UI 上恒可点，
// 但未解锁时服务端 403「连续登录天数不足」——所以放在每日签到后跑一遍（幂等），
// 到天数那天自动完成「兑换 → 抽奖」闭环，无需人工盯。
package scheduler

import (
	"context"
	"encoding/json"
	"log"
	"time"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
	"github.com/BegoniaHe/wokbuddy-demon/internal/logfmt"
	"github.com/BegoniaHe/wokbuddy-demon/internal/upstream"
)

// RunStreakBonusNow 对所有可用账号执行连登兑换 + 抽奖（幂等：locked/无次数自动跳过）。
// 由签到排程（RunCheckinNow）末尾调用；也可面板手动触发。
func (s *Scheduler) RunStreakBonusNow() {
	s.runTaskSync(context.Background(), TaskStreak, TriggerManual)
}

// streakAll 连登兑换 + 抽奖实体；返回尝试/成功/失败账号数（门控跳过不计数）。
func (s *Scheduler) streakAll(trigger string) (total, success, failure int) {
	_ = trigger

	var runCounters2 runCounters

	list := s.cfg.Pool.List()
	for i := range list {
		st := &list[i]
		if st.Disabled {
			continue
		}

		account := s.cfg.Pool.AuthByUID(st.UID)
		if account == nil || account.AccessTokenValue() == "" {
			continue
		}

		if account.IsGlobal() {
			continue // D4 门控：global 无 CN 任务体系，不发起任何上游调用
		}

		if account.IsEnterpriseSession() {
			continue // 企业版 / 企业切换会话门控：无成长体系（/activity/growth/streak 上游一律 403）
		}

		runCounters2.record(s.streakBonusAccount(account))
	}

	return runCounters2.total, runCounters2.success, runCounters2.failure
}

// streakBonusAccount 单账号：补签保连登 → 礼包/补偿 → 兑换所有已解锁档位 → 抽完所有 chances。
// 返回 false 仅表示主链路（连登状态/抽奖次数）查询失败；可选的礼包/补签失败不改变结果。
func (s *Scheduler) streakBonusAccount(account3 *auth.Auth) bool {
	// 0. 补签保连登：昨日漏签且有补签卡则补上（连续天数一断就要重攒 7 天）。
	s.makeupYesterday(account3)
	// 0.5 礼包/补偿（每号一次，无则业务错误静默跳过）。
	s.claimStreakGifts(account3)

	full, err := s.cfg.Upstream.GrowthStreakFull(account3)
	if err != nil {
		log.Printf("streak-bonus %s: %v", logfmt.Label(account3.UID, account3.NicknameValue()), err)
		return false
	}

	s.redeemUnlockedTiers(account3, full)

	// 抽奖：按当前 chances 全抽完（兑换刚发的次数已在服务端累加）。
	chances, err := s.cfg.Upstream.LotteryChances(account3)
	if err != nil {
		log.Printf("streak-bonus %s: lottery summary: %v", logfmt.Label(account3.UID, account3.NicknameValue()), err)
		return false
	}

	s.drawAllLottery(account3, chances)

	return true
}

// claimStreakGifts 领取新手礼包与补偿（各一次，业务错误静默跳过）。
func (s *Scheduler) claimStreakGifts(account4 *auth.Auth) {
	if credit, err := s.cfg.Upstream.ClaimGift(account4); err == nil {
		log.Printf("streak-bonus %s: 🎊 新手礼包 +%dc", logfmt.Label(account4.UID, account4.NicknameValue()), credit)
	}

	if credit, err := s.cfg.Upstream.ClaimCompensation(account4); err == nil {
		log.Printf("streak-bonus %s: 🎊 补偿领取 +%dc", logfmt.Label(account4.UID, account4.NicknameValue()), credit)
	}
}

// redeemUnlockedTiers 兑换所有已解锁档位（locked/claimed 跳过；未解锁 403 静默）。
func (s *Scheduler) redeemUnlockedTiers(account *auth.Auth, full *upstream.StreakFull) {
	statuses := map[string]string{
		"7d":  full.RedemptionStatus.Tier7dStatus,
		"14d": full.RedemptionStatus.Tier14dStatus,
		"28d": full.RedemptionStatus.Tier28dStatus,
	}
	for _, tier := range full.RedemptionStatus.Tiers {
		status := statuses[tier.Tier]
		if status == "locked" || status == "claimed" {
			continue
		}

		if err := s.cfg.Upstream.GrowthRedeemTier(account, tier.Tier); err != nil {
			// 未解锁（403）属预期，静默；其余记日志。
			log.Printf("streak-bonus %s: redeem %s: %v", logfmt.Label(account.UID, account.NicknameValue()), tier.Tier, err)
			continue
		}

		log.Printf("streak-bonus %s: ★ 兑换 %s 档（+%dc +%de 卡×%d 抽奖×%d）",
			account.UID, tier.Tier, tier.Credit, tier.Energy, tier.Cards, tier.Chances)
	}
}

// drawAllLottery 按当前抽奖次数全部抽完，遇错即停。
func (s *Scheduler) drawAllLottery(account5 *auth.Auth, chances int) {
	for i := range chances {
		raw, err := s.cfg.Upstream.LotteryDraw(account5)
		if err != nil {
			log.Printf("streak-bonus %s: draw: %v", logfmt.Label(account5.UID, account5.NicknameValue()), err)
			return
		}

		log.Printf("streak-bonus %s: 🎲 第%d抽 %s", logfmt.Label(account5.UID, account5.NicknameValue()), i+1, compactJSON(raw))
	}

	if chances > 0 {
		log.Printf("streak-bonus %s: 抽奖完成 %d 次", logfmt.Label(account5.UID, account5.NicknameValue()), chances)
	}
}

// compactJSON 裁剪奖品载荷（日志单行可读）。
func compactJSON(raw json.RawMessage) string {
	s := string(raw)
	if len(s) > 220 {
		return s[:220] + "…"
	}

	return s
}

// makeupYesterday 昨日漏签且有补签卡时自动补签（保住连登连续天数）。
// 无卡 / 无漏签 / 查询失败均静默（不影响主流程）。
func (s *Scheduler) makeupYesterday(account2 *auth.Auth) {
	missed, err := s.cfg.Upstream.HeatmapYesterdayMissed(account2)
	if err != nil || !missed {
		return
	}

	full, err := s.cfg.Upstream.GrowthStreakFull(account2)
	if err != nil || full.MakeupCards.Balance <= 0 {
		return
	}

	yesterday := time.Now().AddDate(0, 0, -1).Format("2006-01-02")
	if err := s.cfg.Upstream.UseMakeupCard(account2, yesterday); err != nil {
		log.Printf("streak-bonus %s: 补签 %s 失败: %v", logfmt.Label(account2.UID, account2.NicknameValue()), yesterday, err)
		return
	}

	log.Printf("streak-bonus %s: ★ 已用补签卡补签 %s（保连登）", logfmt.Label(account2.UID, account2.NicknameValue()), yesterday)
}
