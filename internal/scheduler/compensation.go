// compensation.go 官方补偿的「先查后领」链路（排程与面板共用）。
//
// 命题是「不要静默失败」：每个分支要么成功、要么在结果行里显形。
//   - 状态查询：active=false 是「当前无活动」的正常 200，不是错误；
//   - 领取判据只用 upstream.CompensationStatus.Pending()（唯一权威实现，
//     避免同一与门散落多份拷贝）；
//   - 并发查询按**池顺序**写回（各写各自下标，不共享 append），行序稳定；
//   - 门控沿用 streakBonusAccount 同口径：global / enterprise / disabled 跳过，
//     paused **不**跳过（纯领奖 RPC，与「让位防风控」的选号语义无关）。
package scheduler

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
	"github.com/BegoniaHe/wokbuddy-demon/internal/logfmt"
	"github.com/BegoniaHe/wokbuddy-demon/internal/upstream"
)

// compensationWorkers 并发查询/领取的账号上限（与余额刷新同量级）。
const compensationWorkers = 5

// CompensationRow 单个账号的补偿状态行（面板表格与批量汇总共用）。
type CompensationRow struct {
	UID      string `json:"uid"`
	Nickname string `json:"nickname,omitempty"`
	Realm    string `json:"realm,omitempty"`
	// Skipped 门控跳过（global / 企业版 / 禁用）。
	Skipped bool   `json:"skipped,omitempty"`
	Reason  string `json:"reason,omitempty"`
	// Error 查询或领取失败（非空即必须显形——这正是本文件的命题）。
	Error string `json:"error,omitempty"`
	// Active / Claimed / CreditNum / EndTime 来自上游状态。
	Active  bool `json:"active"`
	Claimed bool `json:"claimed"`
	// CreditNum 到账额度；CreditKnown=false 表示上游回执与状态都没给额度（显示「未知」，
	// 不能写成 0，issue #251）。
	CreditNum   int64  `json:"credit_num"`
	CreditKnown bool   `json:"credit_known"`
	EndTime     string `json:"end_time,omitempty"`
}

// Pending 该行是否存在「可领且未领」的补偿（与 upstream.Pending 同口径）。
func (r *CompensationRow) Pending() bool {
	return !r.Skipped && r.Error == "" && r.Active && !r.Claimed && r.CreditNum > 0
}

// dispatchCompensation 取一个并发名额；ctx 已取消（或取名额期间取消）时返回
// false 并归还名额。sem 满时不阻塞主 goroutine，与 RunBalanceRefreshNowCtx 同口径。
//
// select 同时就绪会随机选：所以拿到名额后必须二次确认，覆盖「检查后、取名额前」
// 刚好取消的窗口——否则取消之后仍会派发一次上游请求。
func dispatchCompensation(ctx context.Context, sem chan struct{}) bool {
	if ctx.Err() != nil {
		return false
	}

	select {
	case sem <- struct{}{}:
		if ctx.Err() != nil {
			<-sem

			return false
		}

		return true
	case <-ctx.Done():
		return false
	}
}

// markNotDispatched 标记「本轮未查询上游」的行。
//
// 未派发的行若保持零值，面板/汇总与「已查询、当前无活动」完全不可区分：
// SummarizeCompensation 会把它算成三不靠（既非 claimed 也非 skipped/errored），
// 批量结果看起来一切正常，实际有 N 个账号根本没查——与本文件「不要静默失败」的
// 命题相反。ctx 取消不是上游故障，故记 Skipped + 原因而非 Error。
func markNotDispatched(row *CompensationRow) {
	row.Skipped = true
	row.Reason = "请求已取消，未查询上游"
}

// compensationGate 门控：global / 企业版 / 禁用账号不发任何上游请求。
func compensationGate(acct *auth.Auth, disabled bool) (string, bool) {
	switch {
	case disabled:
		return "账号已禁用", true
	case acct.IsGlobal():
		return "国际版（global）账号无 CN 补偿活动", true
	case acct.IsEnterprise():
		return "企业版账号无个人补偿体系", true
	default:
		return "", false
	}
}

// CompensationStatusAll 并发查询全部账号的补偿状态，按池顺序返回。
//
// ctx 取消后不再派发新的请求（已派发的等其完成）。
func (s *Scheduler) CompensationStatusAll(ctx context.Context) []CompensationRow {
	list := s.cfg.Pool.List()
	rows := make([]CompensationRow, len(list))

	sem := make(chan struct{}, compensationWorkers)

	var wg sync.WaitGroup

	for i := range list {
		status := &list[i]

		rows[i] = CompensationRow{
			UID:      status.UID,
			Nickname: status.Nickname,
			Realm:    status.Realm,
		}

		// 凭证缺失也要显形：留一条空行会被前端当成「没有待领补偿」，与本模块
		// 「不静默失败」的命题相反。
		account := s.cfg.Pool.AuthByUID(status.UID)
		if account == nil {
			rows[i].Error = "账号凭证缺失"

			continue
		}

		if reason, skip := compensationGate(account, status.Disabled); skip {
			rows[i].Skipped = true
			rows[i].Reason = reason

			continue
		}

		if !dispatchCompensation(ctx, sem) {
			markNotDispatched(&rows[i])

			continue
		}

		s.startCompensationStatusFetch(&wg, sem, rows, i, account)
	}

	wg.Wait()

	return rows
}

// startCompensationStatusFetch 派发单账号状态查询：panic 隔离 + 失败必显形。
//
// 独立成方法是为了让 CompensationStatusAll 保持「派发/等待」的主线可读。
// 各写各自下标：不同下标是互不重叠的内存，无需锁。
func (s *Scheduler) startCompensationStatusFetch(
	wg *sync.WaitGroup, sem chan struct{}, rows []CompensationRow, index int, account *auth.Auth,
) {
	wg.Add(1)

	// 必须真起协程：guarded 只做 panic 隔离，同步调用会把「并发查询」退化成
	// 逐账号串行（sem 与 wg 也就形同虚设），面板批量入口按 N × 上游 RTT 阻塞。
	go s.guarded("compensation-status", func() {
		defer wg.Done()
		defer func() { <-sem }()
		// panic 已在 guarded 内隔离，但该行必须显形，不能留成空行（本文件的命题
		// 是「不要静默失败」）。fetchCompensationRow 总会写 UID，仍为空即 panic。
		defer func() {
			if rows[index].UID == "" {
				rows[index].UID = account.UID
				rows[index].Error = "内部错误（已隔离）"
			}
		}()

		s.fetchCompensationRow(account, &rows[index])
	})
}

// fetchCompensationRow 查单账号状态并填行（失败写 Error，不 panic）。
func (s *Scheduler) fetchCompensationRow(account *auth.Auth, row *CompensationRow) {
	status, err := s.cfg.Upstream.GetCompensationStatus(account)
	if err != nil {
		row.Error = fmt.Errorf("补偿状态查询失败（%s）: %w",
			logfmt.Label(row.UID, account.NicknameValue()), err).Error()

		return
	}

	row.Active, row.Claimed, row.CreditNum, row.EndTime =
		status.Active, status.Claimed, status.CreditNum, status.EndTime
}

// ClaimCompensationFor 对单账号执行「先查后领」，返回该账号的结果行。
//
// 三种分支都显形：无活动 / 已领取 → Skipped 语义（Reason 说明）；待领 → 领取并记 credit；
// 查询或领取失败 → Error。
func (s *Scheduler) ClaimCompensationFor(uid string) CompensationRow {
	row := CompensationRow{UID: uid}

	status, ok := s.cfg.Pool.Status(uid)
	if !ok {
		row.Error = "账号不存在"

		return row
	}

	row.Nickname, row.Realm = status.Nickname, status.Realm

	account := s.cfg.Pool.AuthByUID(uid)
	if account == nil {
		row.Error = "账号凭证缺失"

		return row
	}

	if reason, skip := compensationGate(account, status.Disabled); skip {
		row.Skipped = true
		row.Reason = reason

		return row
	}

	comp, err := s.cfg.Upstream.GetCompensationStatus(account)
	if err != nil {
		row.Error = err.Error()

		return row
	}

	row.Active, row.Claimed, row.CreditNum, row.EndTime = comp.Active, comp.Claimed, comp.CreditNum, comp.EndTime

	return s.claimCompensation(&row, account, comp, uid)
}

// claimCompensation 执行「已领/无待领/幂等/领取」分支，返回填好的结果行。
//
//nolint:funlen // 领取/状态更新/结果汇总线性展开。
func (s *Scheduler) claimCompensation(
	row *CompensationRow, account *auth.Auth, comp upstream.CompensationStatus, uid string,
) CompensationRow {
	key := compensationClaimKey(uid, comp)

	if comp.Claimed {
		s.recordClaimed(key)

		row.Skipped = true
		row.Reason = "上游状态已领取"

		return *row
	}

	if !comp.Pending() {
		row.Skipped = true
		row.Reason = "当前无待领补偿"

		return *row
	}

	if !s.beginClaim(key) {
		row.Skipped = true
		row.Reason = "本周期已领取（进程内幂等）"

		return *row
	}

	credit, err := s.cfg.Upstream.ClaimCompensation(account)
	if err != nil {
		s.finishClaim(key, false)
		// 领取失败必须显形：这正是「盲领」时被吞掉的那一类。
		log.Printf("WARN: [scheduler] 补偿领取失败 acct=%s: %v",
			logfmt.Label(uid, account.NicknameValue()), fmt.Errorf("claim: %w", err))
		row.Error = err.Error()

		return *row
	}

	s.finishClaim(key, true)

	credit, known := s.normalizeClaimCredit(account, credit)

	row.Claimed = true
	row.CreditNum = credit
	row.CreditKnown = known

	if known {
		log.Printf("INFO: [scheduler] 补偿领取成功 acct=%s: +%dc", logfmt.Label(uid, account.NicknameValue()), credit)
	} else {
		log.Printf("INFO: [scheduler] 补偿领取成功 acct=%s: 额度未知（回执与状态均为 0）",
			logfmt.Label(uid, account.NicknameValue()))
	}

	return *row
}

// compensationClaimKey 进程内幂等键：账号 + 活动截止时间 + 待领额度。
// 上游没有稳定活动 ID；同账号同截止同额度的重复回调视为同一周期。
func compensationClaimKey(uid string, status upstream.CompensationStatus) string {
	return uid + "|" + status.EndTime + "|" + strconv.FormatInt(status.CreditNum, 10)
}

// beginClaim 占用领取权：已领取或正在领取返回 false。首次调用惰性初始化 map
// （零值 Scheduler 也可用）。
//
// claimedKeys 按 uid 只保留最近一次领取的周期 key：key 内含 EndTime，旧周期不会
// 再次出现，因此无需保留历史；按 key 全量记账会让表随「账号数 × 周期数」无限增长。
func (s *Scheduler) beginClaim(key string) bool {
	uid, _, _ := strings.Cut(key, "|")

	s.claimMu.Lock()
	defer s.claimMu.Unlock()

	if s.claimedKeys == nil {
		s.claimedKeys = make(map[string]string)
	}

	if s.claimingKeys == nil {
		s.claimingKeys = make(map[string]bool)
	}

	if done, ok := s.claimedKeys[uid]; ok && done == key {
		return false
	}

	if s.claimingKeys[key] {
		return false
	}

	s.claimingKeys[key] = true

	return true
}

// finishClaim 释放领取权；成功则写入幂等标记。
func (s *Scheduler) finishClaim(key string, ok bool) {
	uid, _, _ := strings.Cut(key, "|")

	s.claimMu.Lock()
	defer s.claimMu.Unlock()

	delete(s.claimingKeys, key)

	if ok {
		s.claimedKeys[uid] = key
	}
}

// recordClaimed 记录上游已领取状态（重启后首次查询命中 claimed 也走这里）。
func (s *Scheduler) recordClaimed(key string) {
	uid, _, _ := strings.Cut(key, "|")

	s.claimMu.Lock()
	defer s.claimMu.Unlock()

	if s.claimedKeys == nil {
		s.claimedKeys = make(map[string]string)
	}

	s.claimedKeys[uid] = key
}

// normalizeClaimCredit 统一「真实到账」归一：回执 credit<=0 时回落状态额度；
// 两者都为 0 时 known=false（面板/日志显示未知，不写假 0，issue #251）。
func (s *Scheduler) normalizeClaimCredit(account *auth.Auth, receipt int64) (int64, bool) {
	if receipt > 0 {
		return receipt, true
	}

	// 回执不填额度时重新查一次状态（领取后的额度可能已变化），不能拿领取前的
	// 待领额度当作到账值。
	status, err := s.cfg.Upstream.GetCompensationStatus(account)
	if err == nil && status.CreditNum > 0 {
		return status.CreditNum, true
	}

	return 0, false
}

// CompensationSummary 批量领取的汇总。
type CompensationSummary struct {
	Claimed int   `json:"claimed"`
	Skipped int   `json:"skipped"`
	Errored int   `json:"errored"`
	Credit  int64 `json:"credit"`
}

// SummarizeCompensation 汇总结果行。**先看 Errored 再看 Claimed**：
// 上游全挂时每个账号都走 error 分支，若只看 claimed==0 会被说成「当前没有待领补偿」，
// 把故障说成「没活动」——比不说更糟。
func SummarizeCompensation(rows []CompensationRow) CompensationSummary {
	summary := CompensationSummary{}

	for i := range rows {
		row := &rows[i]

		switch {
		case row.Error != "":
			summary.Errored++
		case row.Skipped:
			summary.Skipped++
		case row.Claimed:
			summary.Claimed++
			summary.Credit += row.CreditNum
		}
	}

	return summary
}

// ClaimCompensationNow 全量「先查后领」（面板批量入口）。返回逐账号结果与汇总。
//
// ctx 取消后不再派发新的领取（已派发的等其完成），与 CompensationStatusAll 同口径：
// 面板请求断开或停机时不该继续对剩余账号打上游。
func (s *Scheduler) ClaimCompensationNow(ctx context.Context) ([]CompensationRow, CompensationSummary) {
	list := s.cfg.Pool.List()
	rows := make([]CompensationRow, len(list))

	sem := make(chan struct{}, compensationWorkers)

	var wg sync.WaitGroup

	for i := range list {
		if !dispatchCompensation(ctx, sem) {
			markNotDispatched(&rows[i])

			continue
		}

		wg.Add(1)

		// 同上：guarded 是同步执行，缺 go 就把批量领取压成串行。
		go s.guarded("compensation-claim", func() {
			defer wg.Done()
			defer func() { <-sem }()
			defer func() {
				if rows[i].UID == "" {
					rows[i].UID = list[i].UID
					rows[i].Error = "内部错误（已隔离）"
				}
			}()

			rows[i] = s.ClaimCompensationFor(list[i].UID)
		})
	}

	wg.Wait()

	return rows, SummarizeCompensation(rows)
}

// claimCompensationIfPending 签到链路的「先查后领」（静默：无活动不打印、失败必打印）。
func (s *Scheduler) claimCompensationIfPending(account *auth.Auth) {
	status, err := s.cfg.Upstream.GetCompensationStatus(account)
	if err != nil {
		log.Printf("WARN: [scheduler] 补偿状态查询失败 acct=%s: %v",
			logfmt.Label(account.UID, account.NicknameValue()), err)

		return
	}

	if !status.Pending() {
		return
	}

	key := compensationClaimKey(account.UID, status)
	if !s.beginClaim(key) {
		return
	}

	credit, err := s.cfg.Upstream.ClaimCompensation(account)
	if err != nil {
		s.finishClaim(key, false)
		log.Printf("WARN: [scheduler] 补偿领取失败 acct=%s: %v",
			logfmt.Label(account.UID, account.NicknameValue()), err)

		return
	}

	s.finishClaim(key, true)

	credit, known := s.normalizeClaimCredit(account, credit)
	if known {
		log.Printf("streak-bonus %s: 🎊 补偿领取 +%dc", logfmt.Label(account.UID, account.NicknameValue()), credit)
	} else {
		log.Printf("streak-bonus %s: 🎊 补偿领取成功（额度未知）", logfmt.Label(account.UID, account.NicknameValue()))
	}
}
