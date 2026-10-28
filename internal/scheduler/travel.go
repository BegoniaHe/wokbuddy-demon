// travel.go 猫猫旅行巡检状态机：随旅行时点（travel_hours，默认 09 点）对池内每个可用账号单趟推进一次。
// 无猫 → 同意协议 + 领养；有猫 → 按 travel/status 分派 派出 / 领奖 / 跳过。
package scheduler

import (
	"context"
	"log"
	"time"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
	"github.com/BegoniaHe/wokbuddy-demon/internal/cst"
	"github.com/BegoniaHe/wokbuddy-demon/internal/logfmt"
	"github.com/BegoniaHe/wokbuddy-demon/internal/session"
	"github.com/BegoniaHe/wokbuddy-demon/internal/upstream"
)

const (
	// travelLocationID 派出地点固定 4（古镇客栈）：4 个地点收益/时长区间完全相同，无最优解。
	travelLocationID = 4

	// travelStateIdle 空闲可派出；travelStateTraveling 在途；travelStateArrived 到站可领奖。
	travelStateIdle      = "idle"
	travelStateTraveling = "traveling"
	travelStateArrived   = "arrived"
)

// travelAccountDelay 账号间限速：全量账号约 40s，避免上游风控。测试可置 0。
var travelAccountDelay = 800 * time.Millisecond

// activityAccountDelay 活跃上报账号间限速：与旅行同口径，避免上游风控。测试可置 0。
var activityAccountDelay = 800 * time.Millisecond

// adoptReportGap 领养前置上报后的等待：给上游事件处理留时间再发 buddy/first。
// 对齐 scripts/task_first_buddy.py 实测的 1.05s 间隔口径。测试可置 0。
var adoptReportGap = 1050 * time.Millisecond

// travelDay 返回 t 所属的上游自然日（CST），格式 2006-01-02。
func travelDay(t time.Time) string { return cst.Day(t) }

// RunTravelNow 立即对池内所有可用账号执行一趟旅行巡检。
// 禁用账号跳过；暂停选号（paused）账号照常参与——旅行是纯 RPC（状态/派出/
// 领奖 + 领养前置上报），不发模型对话，与「让位防风控」不冲突；
// 401/查询失败只跳过该账号本轮（不强刷 token，交 22:00 keepalive）；
// 账号间限速 travelAccountDelay。
func (s *Scheduler) RunTravelNow() {
	s.runTaskSync(context.Background(), TaskTravel, TriggerManual)
}

// TravelRunResult 单账号旅行执行结果（面板按号入口使用）。
type TravelRunResult struct {
	UID        string `json:"uid"`
	Ran        bool   `json:"ran"`
	OK         bool   `json:"ok"`
	SkipReason string `json:"skip_reason,omitempty"`
}

// RunTravelNowFor 对单个账号执行一趟旅行巡检（issue #257）。门控与全量版一致：
// disabled / global / 企业会话 / 无 RT 跳过；另外暂停选号（paused）也跳过——
// 按号入口是显式操作，不替运维越过暂停语义。
func (s *Scheduler) RunTravelNowFor(uid string) TravelRunResult {
	result := TravelRunResult{UID: uid}

	account, reason := s.travelAccountGate(uid)
	if reason != "" {
		result.SkipReason = reason

		return result
	}

	if !s.acquireRun(TaskTravel, TriggerManual) {
		result.SkipReason = "旅行巡检正在运行"

		return result
	}

	okRun := s.travelOne(account)

	success, failure := 0, 0
	if okRun {
		success = 1
	} else {
		failure = 1
	}

	s.releaseRun(TaskTravel, 1, success, failure)

	result.Ran = true
	result.OK = okRun
	log.Printf("panel: 按号旅行 uid=%s ok=%v", logfmt.Label(uid, account.NicknameValue()), okRun)

	return result
}

// travelAccountGate 按号旅行的账号门控（disabled/global/enterprise/paused/凭证）。
func (s *Scheduler) travelAccountGate(uid string) (*auth.Auth, string) {
	status, ok := s.cfg.Pool.Status(uid)
	if !ok {
		return nil, "账号不存在"
	}

	if status.Disabled {
		return nil, "账号已禁用"
	}

	if status.Paused {
		return nil, "账号已暂停选号"
	}

	account := s.cfg.Pool.AuthByUID(uid)
	if account == nil || account.RefreshTokenValue() == "" {
		return nil, "账号凭证缺失"
	}

	if account.IsGlobal() {
		return nil, "国际版（global）账号无 CN 旅行体系"
	}

	if account.IsEnterprise() {
		return nil, "企业版无个人旅行体系"
	}

	return account, ""
}

// travelAll 旅行巡检实体；返回尝试/成功/失败账号数（跳过不计数）。
func (s *Scheduler) travelAll(ctx context.Context) (total, success, failure int) {
	list := s.cfg.Pool.List()
	targets := make([]batchTarget, 0, len(list))

	for i := range list {
		st := &list[i]
		if st.Disabled {
			continue
		}

		account := s.cfg.Pool.AuthByUID(st.UID)
		if account == nil || account.RefreshTokenValue() == "" {
			continue
		}

		if account.IsGlobal() {
			continue // D4 门控：global 无 CN 任务体系，不发起任何上游调用
		}

		if account.IsEnterprise() {
			continue // 企业版门控：无个人成长体系（/activity/growth/* 上游一律 403），不发起任何上游调用
		}

		targets = append(targets, batchTarget{uid: account.UID, run: func() bool { return s.travelOne(account) }})
	}

	return s.runPacedBatch(ctx, taskTravel, travelAccountDelay, targets)
}

// travelLookupFailed 记录旅行查询失败并通知。
func (s *Scheduler) travelLookupFailed(account *auth.Auth, stage string, err error) {
	log.Printf("travel %s: %s: %v", logfmt.Label(account.UID, account.NicknameValue()), stage, err)
	s.notifyTaskFailed("travel", account.UID, account.NicknameValue(), account.Realm())
}

// travelOne 单账号单趟状态机：查有无猫 + 查状态 + 最多一个动作，不轮询不等待。
// 返回 false 仅表示上游查询/动作真实失败；「已领养/门槛未达/旅行中」属正常跳过。
func (s *Scheduler) travelOne(account3 *auth.Auth) bool {
	buddy, err := s.cfg.Upstream.BuddyInfo(account3)
	if err != nil {
		s.travelLookupFailed(account3, "buddy-info", err)

		return false
	}

	if buddy == nil {
		return s.travelAdopt(account3)
	}

	travelState, err := s.cfg.Upstream.TravelStatus(account3)
	if err != nil {
		s.travelLookupFailed(account3, "status", err)

		return false
	}

	switch travelState.State {
	case travelStateArrived:
		return s.travelClaim(account3, travelState)
	case travelStateIdle:
		return s.travelDepart(account3, travelState)
	case travelStateTraveling:
		log.Printf("travel %s: skip (traveling record=%d)",
			logfmt.Label(account3.UID, account3.NicknameValue()), travelState.RecordID)

		return true
	default:
		log.Printf("travel %s: skip (unknown state %q)",
			logfmt.Label(account3.UID, account3.NicknameValue()), travelState.State)

		return true
	}
}

// travelDepart 空闲且未达当日上限时派出（每日 1 次，自然日 00:00 CST 重置）。
func (s *Scheduler) travelDepart(account2 *auth.Auth, ts *upstream.TravelState) bool {
	if ts.DailyLimitReached {
		log.Printf("travel %s: skip (daily limit reached)", logfmt.Label(account2.UID, account2.NicknameValue()))
		return true
	}

	if err := s.cfg.Upstream.TravelDepart(account2, travelLocationID); err != nil {
		log.Printf("travel %s: depart: %v", logfmt.Label(account2.UID, account2.NicknameValue()), err)
		s.notifyTaskFailed("travel", account2.UID, account2.NicknameValue(), account2.Realm())

		return false
	}

	log.Printf("travel %s: depart ok location=%d",
		logfmt.Label(account2.UID, account2.NicknameValue()), travelLocationID)

	return true
}

// travelClaim 到站领奖。record_id 只作到站判据与 400 兜底，官方请求体为空。
func (s *Scheduler) travelClaim(account *auth.Auth, travelState *upstream.TravelState) bool {
	if travelState.RecordID == 0 {
		log.Printf("travel %s: claim skipped (arrived but no record_id)",
			logfmt.Label(account.UID, account.NicknameValue()))

		return true
	}

	reward, err := s.cfg.Upstream.TravelClaim(account, travelState.RecordID)
	if err != nil {
		log.Printf("travel %s: claim record=%d: %v",
			logfmt.Label(account.UID, account.NicknameValue()), travelState.RecordID, err)
		s.notifyTaskFailed("travel", account.UID, account.NicknameValue(), account.Realm())

		return false
	}

	log.Printf("travel %s: claim ok record=%d reward=%d",
		logfmt.Label(account.UID, account.NicknameValue()), travelState.RecordID, reward)

	return true
}

// travelAdopt 无猫时领养，链路：report → agreement → buddy/first。
//
// report 必须先跑（scripts/task_first_buddy.py 实测）：一条 chat_request_send 上报
// 点亮 growth 连登并**解锁 first_buddy 任务**；未上报时 buddy/first 会返回
// 400 "first_buddy task not completed yet"——该门槛的真实来源是"当日无活跃上报"，
// 不是账号问题（report.go 注释亦明确「解锁 first_buddy 任务（领养前置）」）。
// conversation 门槛未达标仍属预期行为，记一次当日已试后静默跳过，不再重试。
func (s *Scheduler) travelAdopt(account *auth.Auth) bool {
	if s.adoptTriedToday(account.UID) {
		return true
	}
	// 前置：解锁 first_buddy 任务（幂等；失败不阻塞，让 buddy/first 按既有错误路径暴露）。
	if err := s.cfg.Upstream.ReportChatActivity(account, session.NewUUIDv4(), session.NewUUIDv7Hex()); err != nil {
		log.Printf("travel %s: adopt preflight report: %v", logfmt.Label(account.UID, account.NicknameValue()), err)
	} else {
		time.Sleep(adoptReportGap) // 给上游事件处理留时间（对齐脚本实测的 1.05s 间隔口径）
	}

	if err := s.cfg.Upstream.BuddyAgreement(account); err != nil {
		log.Printf("travel %s: agreement: %v", logfmt.Label(account.UID, account.NicknameValue()), err)
		s.notifyTaskFailed("travel", account.UID, account.NicknameValue(), account.Realm())

		return false
	}

	err := s.cfg.Upstream.BuddyFirst(account)
	switch {
	case err == nil:
		log.Printf("travel %s: adopt ok (+300 credits)", logfmt.Label(account.UID, account.NicknameValue()))
		return true
	case upstream.IsBuddyTaskIncomplete(err):
		s.markAdoptTried(account.UID)
		log.Printf(
			"travel %s: adopt skipped (conversation threshold not reached, retry tomorrow)",
			logfmt.Label(account.UID, account.NicknameValue()),
		)

		return true
	default:
		log.Printf("travel %s: adopt: %v", logfmt.Label(account.UID, account.NicknameValue()), err)
		s.notifyTaskFailed("travel", account.UID, account.NicknameValue(), account.Realm())

		return false
	}
}

// adoptTriedToday 该账号当日是否已判定领养门槛未达。
func (s *Scheduler) adoptTriedToday(uid string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.adoptTried[uid] == travelDay(time.Now())
}

// markAdoptTried 记录该账号当日已尝试领养且未过门槛。
func (s *Scheduler) markAdoptTried(uid string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.adoptTried[uid] = travelDay(time.Now())
}
