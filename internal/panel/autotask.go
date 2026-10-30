// autotask.go 面板「一键完成」任务的动作实现。
//
// 设计依据：上游 scripts/task_*.py 实测结论 + 2026-09-12 桌面指纹协议逆向
// （data/desktop-task-protocol.md）——
//   - first_buddy（+300 分）：report（前置解锁）→ agreement → buddy/first
//   - chat_5（+100 分）：累计 5 条 chat_request_send 上报
//   - Model_chat_GLM5.2（+100 分）：accept → 用 glm-5.2 真实对话一次 → 上报（模型字段对齐）
//   - RichMeow_Chat：桌面指纹（workbuddy-desktop）完整对话事件链，纯 API 可点亮（三账号实测）
//   - Buddy_App / Buddy_App_QQ：buddyapp 五连事件，纯 API 可点亮（两账号实测）
//   - automation_1：automated_task_create_suc 事件，纯 API 可点亮（两账号实测）
//   - Library_read：web 域 web_element_click(library_doc_intro_click)（三账号实测）
//   - template_5 / playbook_prompt / create_canvas：asar 逆向出的判据事件
//     （template_used / playbook_prompt_send / wbx_design_canvas_*），纯 API 可点亮（三账号实测）
//   - expert_5 / Expert_team_use_3：真实专家列表 + 召唤链 + 真实 chat（服务端 requestId）
//   - expert_actual_use（三账号实测）
//   - Hp_Appearance：appearance/set + appearance_skin_apply 事件（两账号实测）
//
// 仍未破解：skill_1（疑似要求真实 Skill 工具调用）。
// 不做：Expert_lighthouse（需真实连接器授权）、Expert_Philanthropy（真实捐款）。
//
// 所有动作幂等：已 claimed/已达标的任务直接跳过，不重复消耗上游配额。
package panel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand/v2"
	"net/http"
	"strings"
	"time"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
	"github.com/BegoniaHe/wokbuddy-demon/internal/logfmt"
	"github.com/BegoniaHe/wokbuddy-demon/internal/session"
	"github.com/BegoniaHe/wokbuddy-demon/internal/upstream"
)

var (
	errStatic             = errors.New("任务不存在")
	errRunModelChatFailed = errors.New("runModelChat failed")
	errStatic2            = errors.New("专家市场列表为空")
	errExpertAllFailed    = errors.New("专家召唤+使用全部失败")
)

// autoAction 一个可自动化的任务动作。
type autoAction struct {
	TaskCode string // 目标任务 code
	Desc     string // 展示用说明
	Attempt  bool   // true = 尝试型（上游未证实可脚本化，跑了可能不点亮）
	run      func(p *Panel, a *auth.Auth) (string, error)
	// runWithTask 可选：动作需要执行前任务详情时，直接复用调用方已读取的任务，
	// 避免动作内部再次拉整份列表。动作后回读仍由 autoRunAction 重新请求。
	runWithTask func(p *Panel, a *auth.Auth, before *upstream.Task) (string, error)
}

// autoActions 已实现的任务动作表（顺序即执行顺序：先解锁依赖项）。
// first_buddy 依赖活跃上报解锁，故 chat_5/first_buddy 的执行都自带 report 步骤。
var autoActions = []autoAction{
	{
		TaskCode:    "chat_5",
		Desc:        "上报 5 条对话活跃事件（自动补足差额）",
		run:         runChat5,
		runWithTask: runChat5WithTask,
	},
	{
		TaskCode: "first_buddy",
		Desc:     "上报解锁 → 同意协议 → 领取第一只 Buddy（+300 分）",
		run:      runFirstBuddy,
	},
	{
		TaskCode: "Model_chat_GLM5.2",
		Desc:     "接受任务 → glm-5.2 真实对话一次 → 对齐模型上报",
		run:      runModelChat,
	},
	{
		TaskCode: "RichMeow_Chat",
		Desc:     "桌面指纹事件链上报（已验证：纯 API 可点亮，三账号实测）",
		run:      runRichMeow,
	},
	{
		TaskCode: "Buddy_App",
		Desc:     "上报「进入 Buddy 应用」事件链（已验证：纯 API 可点亮）",
		run:      runBuddyApp,
	},
	{
		TaskCode: "Buddy_App_QQ",
		Desc:     "上报「进入企鹅教师助手」事件链（已验证：纯 API 可点亮）",
		run:      runBuddyApp,
	},
	{
		TaskCode: "automation_1",
		Desc:     "上报「定时任务创建」事件（已验证：纯 API 可点亮）",
		run:      runAutomationCreate,
	},
	{
		TaskCode: "Library_read",
		Desc:     "上报「读资料库介绍」事件（已验证：纯 API 可点亮）",
		run:      runLibraryRead,
	},
	{
		TaskCode: "template_5",
		Desc:     "上报「使用模板创建任务」事件组 ×5（已验证：三账号点亮）",
		run:      runTemplateUse,
	},
	{
		TaskCode: "playbook_prompt",
		Desc:     "上报「灵感案例做同款发送 Prompt」事件组（已验证：三账号点亮）",
		run:      runPlaybookPrompt,
	},
	{
		TaskCode: "create_canvas",
		Desc:     "上报「设计创意画布创建」事件组（已验证：三账号点亮，+300 分）",
		run:      runCreateCanvas,
	},
	{
		TaskCode: "expert_5",
		Desc:     "真实专家召唤+使用链 ×5（专家市场列表+真实 chat，已验证：三账号点亮）",
		run:      runExpertUse,
	},
	{
		TaskCode: "Expert_team_use_3",
		Desc:     "真实专家团召唤+使用链 ×3（已验证：三账号点亮）",
		run:      runExpertTeamUse,
	},
	{
		TaskCode: "Hp_Appearance",
		Desc:     "设置主题 API + 皮肤生效事件（已验证：两账号点亮）",
		run:      runAppearance,
	},
	{
		TaskCode: "skill_1",
		Desc:     "真实对话 + skill_info 技能加载事件（已验证：人杰2 点亮）",
		run:      runSkillFresh,
	},
	{
		TaskCode: "Expert_lighthouse",
		Desc:     "真实轻量云专家召唤+使用链（chat 链带 has_expert，已验证：两账号点亮）",
		run:      runExpertLighthouse,
	},
	{
		TaskCode: "WorkBuddy_Valorant",
		Desc:     "无畏契约拼豆专家召唤+使用链（判据同 Expert_lighthouse，活动截止 2026-11-15）",
		run:      runValorant,
	},
	{
		TaskCode: "black_cat",
		Desc:     "夜猫子：23:00–08:00（CST）窗口内 glm-5.2 对话补足（窗口外提示稍后再试）",
		Attempt:  true,
		run:      runBlackCat,
	},
	{
		TaskCode:    "school_season",
		Desc:        "校园日（小程序口径）：accept → mini 对话+activityId 上报 → 领奖（+100c+5e）",
		run:         runSchoolSeason,
		runWithTask: runMPMiniChatTaskWithTask("school_season", true),
	},
	{
		TaskCode:    "Sequential_Tasks_1",
		Desc:        "小程序首对话（小程序口径）：accept → mini 对话上报 → 领奖（+100c+5e）",
		run:         runSequentialChat,
		runWithTask: runMPMiniChatTaskWithTask("Sequential_Tasks_1", false),
	},
	{
		TaskCode:    "Sequential_Tasks_2",
		Desc:        "小程序选专家对话（小程序口径）：市场专家 id → accept → expert_actual_use 上报 → 领奖（+200c+5e）",
		run:         runMiniExpert,
		runWithTask: runMiniExpertWithTask,
	},
	{
		TaskCode:    "Sequential_Tasks_3",
		Desc:        "小程序五次对话（小程序口径）：accept → mini 对话上报 ×5（自动补差额）→ 领奖（+300c+5e）",
		run:         runSequentialChat5,
		runWithTask: runMPMiniChatTaskWithTask("Sequential_Tasks_3", false),
	},
	{
		TaskCode:    "Sequential_Tasks_4",
		Desc:        "小程序定时任务（预留，每日零点解锁一环）：accept → 定时任务创建事件（PC 同源）→ 领奖（判据待解锁验证）",
		run:         runSequentialAutomation,
		runWithTask: runSequentialAutomationWithTask,
	},
	{
		TaskCode:    "Sequential_Tasks_5",
		Desc:        "小程序使用 GLM5.2（预留）：accept → 带模型字段的 mini 对话上报 → 领奖（判据待解锁验证）",
		run:         runSequentialModelChat,
		runWithTask: runSequentialModelChatWithTask,
	},
	{
		TaskCode:    "Sequential_Tasks_6",
		Desc:        "小程序十次对话（预留）：accept → mini 对话上报 ×target（自动补差额）→ 领奖",
		run:         runSequentialChat10,
		runWithTask: runMPMiniChatTaskWithTask("Sequential_Tasks_6", false),
	},
	{
		TaskCode:    "Sequential_Tasks_7",
		Desc:        "体验灵感功能（预留，疑 PC 口径 +500c+5e）：accept → 灵感事件组（PC+mp 双形态）→ 领奖（判据待解锁验证）",
		run:         runSequentialPlaybook,
		runWithTask: runSequentialPlaybookWithTask,
	},
}

// autoActionFor 查任务对应的动作；无则返回 nil（不可自动化）。
func autoActionFor(code string) *autoAction {
	for i := range autoActions {
		if autoActions[i].TaskCode == strings.TrimSpace(code) {
			return &autoActions[i]
		}
	}

	return nil
}

// unknownAutoActionIndex 未知任务排到最后。
const unknownAutoActionIndex = 1 << 20

// autoActionIndex 任务在 autoActions 中的顺序（队列执行按依赖序排；未知返回大值）。
func autoActionIndex(code string) int {
	for i := range autoActions {
		if autoActions[i].TaskCode == code {
			return i
		}
	}

	return unknownAutoActionIndex
}

// mpTaskCodes 小程序口径专属下发的成长任务：默认（无 mp 头）列表不出现，
// accept/claim 均要求 X-Client-Platform: miniprogram。新任务出现时在此登记。
var mpTaskCodes = map[string]bool{
	"school_season":      true, // 校园日（mini chat + activityId）
	"Sequential_Tasks_1": true, // 小程序首对话（mini chat，无 activityId）
	"Sequential_Tasks_2": true, // 小程序选中专家并完成有效对话（mp 指纹 expert_actual_use）
	"Sequential_Tasks_3": true, // 小程序完成 5 次对话（与 Tasks_1 同形状，target=5 逐条累加）
	// Tasks_4..7 存在性已实测（accept 返回 prerequisite not met 链式依赖；Tasks_8 not
	// found 封顶）。链条每日零点解锁一环（task locked until 次日），判据为 issue #42
	// 描述 + mpsrc 事件形状预置，解锁后逐个实测校正。
	"Sequential_Tasks_4": true,
	"Sequential_Tasks_5": true,
	"Sequential_Tasks_6": true,
	"Sequential_Tasks_7": true,
}

// isMPTaskCode 报告任务是否小程序口径专属（决定回读/接受/领奖走 mp 变体）。
func isMPTaskCode(code string) bool { return mpTaskCodes[code] }

// taskByCode 拉取任务列表并定位单个任务；未找到返回 nil（不视为错误）。
// 双口径：mp 专属任务在默认列表查不到，自动回落 mp 列表（仅对已登记的 mp 码，
// 未知码不多打一次上游）。
func (p *Panel) taskByCode(account22 *auth.Auth, code string) (*upstream.Task, error) {
	tasks, err := p.cfg.Upstream.ListTasks(account22)
	if err != nil {
		return nil, fmt.Errorf("taskByCode: %w", err)
	}

	for i := range tasks {
		if tasks[i].TaskCode == code {
			return &tasks[i], nil
		}
	}

	if isMPTaskCode(code) {
		return p.taskByCodeMP(account22, code)
	}

	return nil, nil //nolint:nilnil // 未找到任务返回 (nil,nil)，调用方以 nil 判定
}

// claimPollAttempts / claimPollGap 达标回读的有界轮询参数。
// 背景：上游计分是**异步**的——行为事件上报后进度要数秒才刷新（实测 Model_chat
// 对话完成后立即回读仍是 0/1，约 5-8 秒后才变 1/1）。一次性回读会误判"未达标"，
// 从而跳过自动领奖。这里最多轮询 N 次、每次间隔 gap，总预算约 12 秒。
var (
	claimPollAttempts = 4
	claimPollGap      = 3 * time.Second
)

// taskByCodeWaiting 回读任务，若未达标则在有界预算内轮询等待（上游异步计分）。
// 已达标（claimable）立即返回；预算耗尽返回最后一次结果（可能仍未达标）。
func (p *Panel) taskByCodeWaiting(account9 *auth.Auth, code string) (*upstream.Task, error) {
	t, err := p.taskByCode(account9, code)
	if err != nil || t == nil {
		return t, err
	}

	if t.Claimable || t.Claimed {
		return t, nil
	}

	for i := 1; i < claimPollAttempts; i++ {
		time.Sleep(claimPollGap)

		task, err2 := p.taskByCode(account9, code)
		if err2 != nil {
			return t, nil //nolint:nilerr // 轮询期间的查询失败不覆盖已拿到的结果
		}

		if task != nil {
			t = task
			if t.Claimable || t.Claimed {
				return t, nil
			}
		}
	}

	return t, nil
}

// taskByCodeMP 以小程序口径拉取任务列表并定位单个任务；未找到返回 nil。
// 小程序限定任务（school_season / Sequential_Tasks_1）在默认口径列表不出现。
func (p *Panel) taskByCodeMP(a *auth.Auth, code string) (*upstream.Task, error) {
	tasks, err := p.cfg.Upstream.ListTasksMP(a)
	if err != nil {
		return nil, fmt.Errorf("taskByCodeMP: %w", err)
	}

	for i := range tasks {
		if tasks[i].TaskCode == code {
			return &tasks[i], nil
		}
	}

	return nil, nil //nolint:nilnil // 未找到任务返回 (nil,nil)，调用方以 nil 判定
}

// acceptWithVerifyMP accept 并回读验证登记生效：上游存在 200+OK 但 accept 未真正
// 登记的形态（此时上报事件全部不归账，任务永远点不亮，上游 task_runner c793ae3
// 实测）——判定以回读 accept_status 为准，未生效重试一次。
func (p *Panel) acceptWithVerifyMP(account12 *auth.Auth, code string) bool {
	for attempt := 1; attempt <= 2; attempt++ {
		if err := p.cfg.Upstream.AcceptTasksMP(account12, []string{code}); err != nil {
			log.Printf("autotask %s %s: accept 尝试%d: %v",
				logfmt.Label(account12.UID, account12.NicknameValue()), code, attempt, err)

			continue
		}

		time.Sleep(mpActionGap)

		t, err := p.taskByCodeMP(account12, code)
		if err == nil && t != nil && t.AcceptStatus != "not_accepted" && t.AcceptStatus != "" {
			return true
		}

		log.Printf(
			"autotask %s %s: accept 尝试%d 未登记生效（回读=%q）",
			logfmt.Label(account12.UID, account12.NicknameValue()),
			code,
			attempt,
			acceptStatusOr(t),
		)
	}

	return false
}

// acceptStatusOr 安全读取任务 accept_status（nil 任务返回 "?"）。
func acceptStatusOr(t *upstream.Task) string {
	if t == nil {
		return "?"
	}

	if t.AcceptStatus == "" {
		return "?"
	}

	return t.AcceptStatus
}

// mpActionGap mp 任务写动作间隔（accept/上报/领奖之间，防频控）。
var mpActionGap = 2 * time.Second

// mpChatEventGap mp 对话事件（chat_request_send）的真人节奏间隔。上游对
// Sequential_Tasks_3「5 次有效对话」有反作弊校验：数秒级连发的事件会先被计入
// 进度（回读 5/5、accept_status 甚至短暂转 completed），随后被判定无效整体回滚
// （进度回落、claim 返回 400 "task not completed"）——2026-09-26 实测 2s 连发
// 4 条全灭，45s 间隔逐条上报全存活且 claim +300c+5e 成功。每条上报前
// sleep gap + 0~10s 抖动；首条也等（上一轮残留进度被回滚后立即重报同样无效）。
var mpChatEventGap = 45 * time.Second

// runMPMiniChatTask growth 域小程序限定任务通用闭环：
// mp 查询 → accept（带登记回读验证）→ mini chat 事件上报（withActivityID 决定
// 是否带开学季 activityId：school_season 必带，Sequential_Tasks_1 不带——服务端按
// source=mini_program 指纹关联）→ 回读 → 达标即领奖。
func (p *Panel) runMPMiniChatTask(account7 *auth.Auth, code string, withActivityID bool) (string, error) {
	t, err := p.taskByCodeMP(account7, code)
	if err != nil {
		return "", err
	}

	return p.runMPMiniChatTaskFrom(account7, code, withActivityID, t)
}

// runMPMiniChatTaskWithTask 使用快照任务开始闭环；快照可能停在 accept 前
// （target=0），先按码回读真实目标再复用闭环主体。
func (p *Panel) runMPMiniChatTaskWithTask(
	account7 *auth.Auth, code string, withActivityID bool, t *upstream.Task,
) (string, error) {
	t, err := p.refreshAcceptedMPTask(account7, code, t)
	if err != nil {
		return "", err
	}

	return p.runMPMiniChatTaskFrom(account7, code, withActivityID, t)
}

// runMPMiniChatTaskFrom 使用调用方已读取的 mp 任务执行闭环；accept 后仍会
// 实时回读以获取真实 target/current。
func (p *Panel) runMPMiniChatTaskFrom(
	account7 *auth.Auth, code string, withActivityID bool, t *upstream.Task,
) (string, error) {
	if t == nil {
		return "mp 口径未下发该任务（活动可能已结束）", nil
	}

	if t.Claimed {
		return "已领取", nil
	}
	// accept 前的任务进度为 null（target 下发 0），兜底 target=1 会少报——
	// Tasks_6 首轮实测：accept 后真实 target=10，只补 1 条就误判达标去领奖
	// （claim 400 task not completed）。接受后回读一次拿真实 target/current。
	t, ok := p.acceptMPAndRefresh(account7, code, t)
	if !ok {
		return "accept 未登记生效（上游 200+OK 但未落账形态），待下次重试", nil
	}
	// 已达标（含 completed 未领）：直接领奖。
	target, msg, done, err := p.mpEarlyClaim(account7, code, t)
	if err != nil {
		return "", err
	}

	if done {
		return msg, nil
	}
	// 判据上报：按差额补 mini chat 事件。每条前 sleep mpChatEventGap+抖动——
	// 连发会被上游反作弊判无效（见 mpChatEventGap 注释），宁可慢不可白报。
	need := target - t.Current
	if i, rerr := p.reportMiniChatEvents(account7, need, withActivityID); rerr != nil {
		return fmt.Sprintf("完成 %d/%d 次上报后中断: %v", i, need, rerr), nil
	}
	// 回读（异步计分，有界轮询复用 claimPoll 预算的紧凑版：两轮各隔 3s）。
	t, _ = p.pollTaskMPFrom(account7, code, target, t, nil)
	notMet := fmt.Sprintf("已上报 %d 次但进度未达 %d/%d（异步计分未归账，下次重试）", need, t.Current, target)

	return p.finishMPTask(account7, code, target, t, notMet)
}

// acceptMPAndRefresh accept（带登记回读验证）并在需要时回读刷新任务，
// 拿真实 target/current；返回 (刷新后任务, 是否登记生效)。
func (p *Panel) acceptMPAndRefresh(account8 *auth.Auth, code string, t *upstream.Task) (*upstream.Task, bool) {
	if t.AcceptStatus != "not_accepted" && t.AcceptStatus != "" {
		return t, true
	}

	if !p.acceptWithVerifyMP(account8, code) {
		return t, false
	}

	if t2, err := p.taskByCodeMP(account8, code); err == nil && t2 != nil {
		return t2, true
	}

	return t, true
}

// acceptMPTaskIfNeeded 任务未接受时 accept 并回读验证；已接受返回 true。
func (p *Panel) acceptMPTaskIfNeeded(a *auth.Auth, code, status string) bool {
	if status != "not_accepted" && status != "" {
		return true
	}

	return p.acceptWithVerifyMP(a, code)
}

// mpEarlyClaim 计算 target 并在任务已达标（含 completed 未领）时直接领奖。
// 返回 target、完成消息、是否已完成、错误。
func (p *Panel) mpEarlyClaim(
	account19 *auth.Auth, code string, t *upstream.Task,
) (target int64, msg string, done bool, err error) {
	target = t.Target
	if target <= 0 {
		target = 1
	}

	if t.Current < target && t.AcceptStatus != "completed" {
		return target, "", false, nil
	}

	credit, energy, cerr := p.cfg.Upstream.ClaimRewardMP(account19, code)
	if cerr != nil {
		return target, "", true, fmt.Errorf("mpEarlyClaim: %w", cerr)
	}

	return target, fmt.Sprintf("已领取奖励（+%dc +%de）", credit, energy), true, nil
}

// reportMiniChatEvents 按差额补 mini chat 事件（每条前按真人节奏 sleep+抖动）。
// 返回中断时的下标与错误；全部成功返回 (need, nil)。
func (p *Panel) reportMiniChatEvents(account20 *auth.Auth, need int64, withActivityID bool) (int64, error) {
	for i := range need {
		time.Sleep(mpChatEventGap + time.Duration(rand.Int64N(int64(10*time.Second)))) //nolint:gosec // 上报节奏抖动，非安全用途

		conv := session.NewUUIDv4()

		var table map[string]any
		if withActivityID {
			table = upstream.SchoolSeasonChatEvent(conv)
		} else {
			table = upstream.SchoolChatTimesEvents(conv)
		}

		if err := p.cfg.Upstream.ReportMPEvent(account20, table); err != nil {
			return i, fmt.Errorf("reportMiniChatEvents: %w", err)
		}
	}

	return need, nil
}

// pollTaskMPFrom 从当前任务出发做达标回读（两轮各隔 claimPollGap）；首轮未达标
// 且 fallback 非空时补报一轮。返回最后一次结果；补报失败返回错误。
func (p *Panel) pollTaskMPFrom(
	a *auth.Auth, code string, target int64, t *upstream.Task, fallback func() error,
) (*upstream.Task, error) {
	for round := range 2 {
		time.Sleep(claimPollGap)

		t2, err2 := p.taskByCodeMP(a, code)
		if err2 != nil || t2 == nil {
			continue
		}

		t = t2
		if t.Claimable || t.Claimed || t.Current >= target {
			break
		}

		if round == 0 && fallback != nil {
			if err := fallback(); err != nil {
				return t, err
			}
		}
	}

	return t, nil
}

// finishMPTask 回读后的收尾：已 claimed / 未达标 / 达标领奖。
func (p *Panel) finishMPTask(
	account14 *auth.Auth, code string, target int64, t *upstream.Task, notMetMsg string,
) (string, error) {
	if t.Claimed {
		return "本轮已入账（claimed）", nil
	}

	if t.Current < target {
		return notMetMsg, nil
	}

	credit, energy, err := p.cfg.Upstream.ClaimRewardMP(account14, code)
	if err != nil {
		return "", fmt.Errorf("finishMPTask: %w", err)
	}

	return fmt.Sprintf("任务点亮并领取奖励（+%dc +%de）", credit, energy), nil
}

// taskByCodeAfterAction 动作后回读任务状态（mp 专属码走 mp 口径，其余有界轮询等待异步计分）。
func (p *Panel) taskByCodeAfterAction(a *auth.Auth, code string) (*upstream.Task, error) {
	if isMPTaskCode(code) {
		return p.taskByCodeMP(a, code)
	}

	return p.taskByCodeWaiting(a, code)
}

// taskVerify 动作后回读并返回进度文本与可领状态（回读失败返回空与 false）。
func (p *Panel) taskVerify(a *auth.Auth, code string) (string, bool) {
	after, err := p.taskByCodeAfterAction(a, code)
	if err != nil || after == nil {
		return "", false
	}

	return taskProgressText(after), after.Claimable
}

// runSchoolSeason 完成 school_season「校园日」（growth 域小程序限定）。
// 判据 = mini chat_request_send + activityId=school_open_day_2026（无 activityId
// 不点亮，与 school 域开学季同活动关联；上游 task_runner e2e 实测 +100c+5e）。
func runSchoolSeason(p *Panel, a *auth.Auth) (string, error) {
	return p.runMPMiniChatTask(a, "school_season", true)
}

// runMPMiniChatTaskWithTask 复用调用方已读取的 mp 任务执行同类闭环。
func runMPMiniChatTaskWithTask(
	code string, withActivityID bool,
) func(*Panel, *auth.Auth, *upstream.Task) (string, error) {
	return func(p *Panel, a *auth.Auth, before *upstream.Task) (string, error) {
		return p.runMPMiniChatTaskWithTask(a, code, withActivityID, before)
	}
}

// runSequentialChat 完成 Sequential_Tasks_1「小程序内完成 1 次有效对话」。
// 判据 = mini chat_request_send（无 activityId，服务端按 source=mini_program
// 指纹关联；上游 task_runner 实测 +100c+5e）。
func runSequentialChat(p *Panel, a *auth.Auth) (string, error) {
	return p.runMPMiniChatTask(a, "Sequential_Tasks_1", false)
}

// runSequentialChat5 完成 Sequential_Tasks_3「在小程序内完成 5 次有效对话」。
// 判据与 Sequential_Tasks_1 同形状（mini 指纹 chat_request_send，无 activityId），
// 仅 target=5——服务端按上报条数累加进度，但**要求真人节奏**：连发事件先计数
// 后被反作弊回滚（claim 400 "task not completed"），由 mpChatEventGap 间隔保证
// （2026-09-26 实测：45s 间隔补满 5/5 → claim +300c+5e 成功，领后 accept_status
// =claimed 稳定不回滚）。
func runSequentialChat5(p *Panel, a *auth.Auth) (string, error) {
	return p.runMPMiniChatTask(a, "Sequential_Tasks_3", false)
}

// runSequentialChat10 完成 Sequential_Tasks_6「在小程序内完成 10 次有效对话」（预留）。
// 判据假定与 Tasks_1/3 同形状（mini chat_request_send），target 由任务自带（回读），
// runMPMiniChatTask 按差额补报——issue #42 称 target=10，以解锁后实际下发为准。
// 真人节奏间隔同样适用（mpChatEventGap）：9 条 × ~50s ≈ 8 分钟/账号，夜间队列可接受。
func runSequentialChat10(p *Panel, a *auth.Auth) (string, error) {
	return p.runMPMiniChatTask(a, "Sequential_Tasks_6", false)
}

// runSequentialEventTask Sequential 链预留任务通用骨架：mp 查询 → accept（带验证）
// → 判据事件上报（primary；未点亮且 fallback 非空时补一轮）→ 回读 → 达标领奖。
// 每日零点解锁一环：locked 期间 accept 不落账，返回等下次调度（无需人工干预）。
func (p *Panel) runSequentialEventTask(
	account23 *auth.Auth, code string, primary, fallback func() error,
) (string, error) {
	t, err := p.taskByCodeMP(account23, code)
	if err != nil {
		return "", err
	}

	return p.runSequentialEventTaskFrom(account23, code, t, primary, fallback)
}

// runSequentialEventTaskWithTask 使用快照任务执行 Sequential 闭环；快照可能停在
// accept 前（target=0），先按码回读真实目标。
func (p *Panel) runSequentialEventTaskWithTask(
	account23 *auth.Auth, code string, t *upstream.Task, primary, fallback func() error,
) (string, error) {
	t, err := p.refreshAcceptedMPTask(account23, code, t)
	if err != nil {
		return "", err
	}

	return p.runSequentialEventTaskFrom(account23, code, t, primary, fallback)
}

// runSequentialEventTaskFrom 使用调用方已读取的 mp 任务执行 Sequential 闭环。
func (p *Panel) runSequentialEventTaskFrom(
	account23 *auth.Auth, code string, t *upstream.Task, primary, fallback func() error,
) (string, error) {
	if t == nil {
		return "mp 口径未下发该任务（前置任务未完成或活动未开始）", nil
	}

	if t.Claimed {
		return "已领取", nil
	}

	target, msg, done, err := p.mpEarlyClaim(account23, code, t)
	if err != nil {
		return "", err
	}

	if done {
		return msg, nil
	}

	if !p.acceptMPTaskIfNeeded(account23, code, t.AcceptStatus) {
		return "accept 未登记生效（任务可能处于每日锁定窗口，等解锁后自动重试）", nil
	}

	if err := primary(); err != nil {
		return fmt.Sprintf("判据上报失败: %v", err), nil
	}
	// 回读（两轮各隔 3s）；未点亮且有 fallback 时补报一轮再读。
	t, perr := p.pollTaskMPFrom(account23, code, target, t, fallback)
	if perr != nil {
		return fmt.Sprintf("备选判据上报失败: %v", perr), nil
	}

	return p.finishMPTask(account23, code, target, t, "已上报但进度未点亮（判据形态待解锁后校正，下次重试）")
}

// runSequentialAutomation 完成 Sequential_Tasks_4「创建定时任务」（预留）。
// mp 源码无 automation 事件发射点 → 判据疑为 PC 口径：复用 automation_1 同源
// 事件（DesktopAutomationCreateEvent，PC 任务三账号实测点亮）。
func runSequentialAutomation(p *Panel, a *auth.Auth) (string, error) {
	return p.runSequentialEventTask(a, "Sequential_Tasks_4",
		func() error {
			return p.cfg.Upstream.ReportDesktopEvent(a, upstream.DesktopAutomationCreateEvent("日常任务提醒"))
		}, nil)
}

// runSequentialAutomationWithTask 复用前置任务快照执行同上闭环。
func runSequentialAutomationWithTask(p *Panel, a *auth.Auth, before *upstream.Task) (string, error) {
	return p.runSequentialEventTaskWithTask(a, "Sequential_Tasks_4", before,
		func() error {
			return p.cfg.Upstream.ReportDesktopEvent(a, upstream.DesktopAutomationCreateEvent("日常任务提醒"))
		}, nil)
}

// runSequentialModelChat 完成 Sequential_Tasks_5「使用 GLM5.2」（预留）。
// primary：mp 对话事件带 requestModelId/requestModelName=glm-5.2（mpsrc main 32904
// 发射点实测形状）；fallback：PC 域模型活跃上报（Model_chat_GLM5.2 同源）。
func runSequentialModelChat(panel2 *Panel, account3 *auth.Auth) (string, error) {
	return panel2.runSequentialEventTask(account3, "Sequential_Tasks_5",
		func() error {
			conv := session.NewUUIDv4()
			return panel2.cfg.Upstream.ReportMPEvent(account3, upstream.MiniChatModelEvent(conv, "glm-5.2", "GLM-5.2"))
		},
		func() error {
			return panel2.cfg.Upstream.ReportChatActivityModel(
				account3,
				session.NewUUIDv4(),
				"",
				"glm-5.2",
				"GLM-5.2",
			)
		})
}

// runSequentialModelChatWithTask 复用前置任务快照执行同上闭环。
func runSequentialModelChatWithTask(panel2 *Panel, account3 *auth.Auth, before *upstream.Task) (string, error) {
	return panel2.runSequentialEventTaskWithTask(account3, "Sequential_Tasks_5", before,
		func() error {
			conv := session.NewUUIDv4()
			return panel2.cfg.Upstream.ReportMPEvent(account3, upstream.MiniChatModelEvent(conv, "glm-5.2", "GLM-5.2"))
		},
		func() error {
			return panel2.cfg.Upstream.ReportChatActivityModel(
				account3,
				session.NewUUIDv4(),
				"",
				"glm-5.2",
				"GLM-5.2",
			)
		})
}

// runSequentialPlaybook 完成 Sequential_Tasks_7「体验灵感功能」（预留，疑 PC 口径）。
// primary：PC 灵感事件组（DesktopPlaybookPromptSequence，playbook_prompt 三账号
// 实测点亮）；fallback：mp 指纹灵感事件组（MiniPlaybookEvents，mpsrc 形状）。
func runSequentialPlaybook(panel4 *Panel, account *auth.Auth) (string, error) {
	return panel4.runSequentialEventTask(account, "Sequential_Tasks_7",
		func() error {
			conv := session.NewUUIDv4()
			req := session.NewUUIDv7Hex()

			return panel4.cfg.Upstream.ReportDesktopEvent(account,
				upstream.DesktopPlaybookPromptSequence(conv, req, "pm-gtm-launch-plan", "新产品上市 GTM 发布计划一页纸")...)
		},
		func() error {
			return panel4.cfg.Upstream.ReportMPEvent(
				account, upstream.MiniPlaybookEvents("pm-gtm-launch-plan", "新产品上市 GTM 发布计划一页纸")...,
			)
		})
}

// runSequentialPlaybookWithTask 复用前置任务快照执行同上闭环。
func runSequentialPlaybookWithTask(
	panel *Panel, account *auth.Auth, before *upstream.Task,
) (string, error) {
	return panel.runSequentialEventTaskWithTask(account, "Sequential_Tasks_7", before,
		func() error {
			conv := session.NewUUIDv4()
			req := session.NewUUIDv7Hex()

			return panel.cfg.Upstream.ReportDesktopEvent(account,
				upstream.DesktopPlaybookPromptSequence(
					conv, req, "pm-gtm-launch-plan", "新产品上市 GTM 发布计划一页纸",
				)...)
		},
		func() error {
			return panel.cfg.Upstream.ReportMPEvent(
				account,
				upstream.MiniPlaybookEvents(
					"pm-gtm-launch-plan", "新产品上市 GTM 发布计划一页纸",
				)...,
			)
		})
}

// runMiniExpert 完成 Sequential_Tasks_2「在小程序内选中专家并完成有效对话」。
// 判据 = mp 指纹 expert_actual_use（**不带** activityId/conversationId、
// extVersion=2.2.8、type=send_message——小程序源码实测形状，与 school 域 expert
// 事件两套口径勿混；上游 task_runner 实测上报即 completed，claim +200c+5e）。
// 专家 id 必须是市场真实 ex_ id（空 id 服务端不入账）→ **accept 之前**先解析市场
// 列表：拉不到就整任务不动作，避免留下「已登记未上报」的半程态（上游 9a26ae7
// 的 ids 前置判定同款）。复用既有 MarketExpertList（expert_5 任务同源，实测可用）。
func runMiniExpert(panel4 *Panel, account4 *auth.Auth) (string, error) {
	const code = "Sequential_Tasks_2"

	t, err := panel4.taskByCodeMP(account4, code)
	if err != nil {
		return "", err
	}

	return runMiniExpertFrom(panel4, account4, t)
}

// runMiniExpertWithTask 使用快照任务执行专家使用闭环；快照可能停在 accept 前
// （target=0），先按码回读真实目标。
func runMiniExpertWithTask(panel4 *Panel, account4 *auth.Auth, t *upstream.Task) (string, error) {
	const code = "Sequential_Tasks_2"

	t, err := panel4.refreshAcceptedMPTask(account4, code, t)
	if err != nil {
		return "", err
	}

	return runMiniExpertFrom(panel4, account4, t)
}

// runMiniExpertFrom 使用调用方已读取的 mp 任务执行专家使用闭环。
func runMiniExpertFrom(panel4 *Panel, account4 *auth.Auth, t *upstream.Task) (string, error) {
	const code = "Sequential_Tasks_2"

	if t == nil {
		return "mp 口径未下发该任务（活动可能已结束）", nil
	}

	if t.Claimed {
		return "已领取", nil
	}
	// 已达标（含 completed 未领）：直接领奖。未 accept 的 mp 任务 progress 为
	// null，target 兜底 1（上游实测）。
	target, msg, done, err := panel4.mpEarlyClaim(account4, code, t)
	if err != nil {
		return "", err
	}

	if done {
		return msg, nil
	}
	// 判据载体前置（accept 之前）：市场真实专家 id。
	marketExpert, name, ok, merr := panel4.marketExpertForMini(account4)
	if !ok {
		return fmt.Sprintf("专家市场不可用（%v），跳过以防半程态", merr), nil
	}

	if !panel4.acceptMPTaskIfNeeded(account4, code, t.AcceptStatus) {
		return "accept 未登记生效（上游 200+OK 但未落账形态），待下次重试", nil
	}

	ev := upstream.MiniExpertUseEvent(marketExpert.ExpertID, name, marketExpert.ExpertType)
	if err := panel4.cfg.Upstream.ReportMPEvent(account4, ev); err != nil {
		return fmt.Sprintf("上报 expert_actual_use 失败: %v", err), nil
	}
	// 回读（异步计分，两轮各隔 3s——与 runMPMiniChatTask 同预算）。
	t, _ = panel4.pollTaskMPFrom(account4, code, target, t, nil)

	return panel4.finishMPTask(account4, code, target, t, "已上报但进度未归账（异步计分，下次重试）")
}

// refreshAcceptedMPTask 批量 accept 后快照仍是 accept 前状态，部分任务 target=0；
// 此时按码回读一次拿真实 target/current，避免按兜底目标少报。已下发目标的任务直接复用。
func (p *Panel) refreshAcceptedMPTask(
	account *auth.Auth, code string, task *upstream.Task,
) (*upstream.Task, error) {
	if task == nil || task.Target > 0 {
		return task, nil
	}

	return p.taskByCodeMP(account, code)
}

// marketExpertForMini 解析市场真实专家（accept 前置，避免半程态）。不可用时 ok
// 为 false，merr 为列表错误（长度为零时可能为 nil）。
func (p *Panel) marketExpertForMini(a *auth.Auth) (upstream.MarketExpert, string, bool, error) {
	experts, err := p.cfg.Upstream.MarketExpertList(a, "")
	if err != nil || len(experts) == 0 {
		if err != nil {
			return upstream.MarketExpert{}, "", false, fmt.Errorf("marketExpertForMini: %w", err)
		}

		return upstream.MarketExpert{}, "", false, nil
	}

	marketExpert := experts[0]

	name := marketExpert.DisplayNameZH
	if name == "" {
		name = marketExpert.ProfessionZH
	}

	return marketExpert, name, true, nil
}

// accountTaskAuto 一键完成单个任务：执行对应动作 → 回读进度 → 汇报结果。
func (p *Panel) accountTaskAuto(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")

	account := p.eligibleTaskAccount(w, uid)
	if account == nil {
		return
	}

	code, ok := decodeTaskCode(w, r)
	if !ok {
		return
	}

	act := autoActionFor(code)
	if act == nil {
		writeErr(w, http.StatusNotImplemented,
			"该任务需要客户端内交互（无对应接口），无法自动完成；请按任务说明在官方客户端操作")

		return
	}
	// per-account 互斥：同账号的任务动作正在跑（单任务或全量）时直接 409，
	// 不并发重跑（动作幂等但 expert/skill 系含真实对话，重跑浪费配额）。
	if !p.tryLockAccount(uid) {
		writeErr(w, http.StatusConflict, "该账号有任务动作正在执行中，请等本轮结束后再试")
		return
	}
	defer p.unlockAccount(uid)
	// 前置读取：已完成的任务直接跳过（幂等，不浪费上游调用）。
	// taskByCode 已双口径（mp 专属码自动回落 mp 列表）。
	before, err := p.taskByCode(account, act.TaskCode)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "list tasks: "+err.Error())
		return
	}

	if before == nil {
		writeErr(w, http.StatusNotFound, "该账号没有此任务")
		return
	}

	if before.Claimed {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "skipped": true, "message": "该任务已领取过奖励"})
		return
	}

	msg, err := act.run(p, account)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "执行失败: "+err.Error())
		return
	}

	p.finishTaskAuto(w, account, uid, act, before, msg)
}

func (p *Panel) finishTaskAuto(
	w http.ResponseWriter,
	account10 *auth.Auth,
	uid string,
	act *autoAction,
	before *upstream.Task,
	msg string,
) {
	// 回读验证：上报 200 ≠ 计分（上游可能静默丢弃 + 计分异步），
	// 用有界轮询等异步计时落定，再决定是否自动领奖（mp 任务同口径回读）。
	progressBefore := taskProgressText(before)
	progressAfter, claimable := p.taskVerify(account10, act.TaskCode)
	resp := map[string]any{
		"ok":               true,
		"message":          msg,
		"progress_before":  progressBefore,
		"progress_after":   progressAfter,
		"claimable":        claimable,
		"attempt":          act.Attempt,
		"verify_supported": true,
	}
	// 达标即自动领奖（Web 端 claim）：把"完成→领奖"收敛成一步，无需用户再点一次。
	if claimable {
		p.autoClaimResponse(account10, act.TaskCode, msg, resp)
	}

	log.Printf("panel: 任务动作 uid=%s code=%s progress %s -> %s claimable=%v claimed=%v",
		uid, act.TaskCode, progressBefore, progressAfter, claimable, resp["claimed"])
	writeJSON(w, http.StatusOK, resp)
}

// autoClaimResponse 达标后自动领奖，把结果写进响应 map（claimed/credit/claim_error）。
//
//nolint:dupl // 与 autoClaimItem 仅输入形态不同，结构对照优先。
func (p *Panel) autoClaimResponse(a *auth.Auth, code, msg string, resp map[string]any) {
	credit, energy, cerr := p.claimTaskReward(a, code)
	if cerr != nil {
		resp["claim_error"] = cerr.Error()
		resp["message"] = msg + "；达标但领奖失败，可在任务列表手动点「领取」重试"

		return
	}

	resp["claimed"] = true
	resp["credit"] = credit

	resp["energy"] = energy
	if credit > 0 || energy > 0 {
		resp["message"] = msg + fmt.Sprintf("；已自动领奖 +%d 分 +%d 能", credit, energy)
	} else {
		resp["message"] = msg + "；奖励此前已领取"
	}
}

// taskProgressText 任务进度的可读表示（回读对比用）。
func taskProgressText(t *upstream.Task) string {
	if t == nil {
		return "?"
	}

	if t.Target > 0 {
		return fmt.Sprintf("%d/%d", t.Current, t.Target)
	}

	if t.Claimed {
		return "claimed"
	}

	return t.AcceptStatus
}

// truncateStr 截断错误文本（避免把上游长响应原样透给前端）。
// 按字节上限截断但回退到 UTF-8 rune 边界，避免中文错误文案被切成半个字符。
func truncateStr(s string, n int) string {
	return logfmt.TruncateEllipsis(s, n)
}

// ---------------------------------------------------------------------------
// 各任务动作实现
// ---------------------------------------------------------------------------

// reportGap 连续上报之间的间隔（对齐上游脚本实测的 1.05s 口径，避免风控）。
var reportGap = 1050 * time.Millisecond

// runChat5 补足 chat_5 的进度：按差额上报 chat_request_send。
func runChat5(panel *Panel, account25 *auth.Auth) (string, error) {
	t, err := panel.taskByCode(account25, "chat_5")
	if err != nil {
		return "", err
	}

	return runChat5WithTask(panel, account25, t)
}

// runChat5WithTask 使用调用方已读取的任务进度执行上报。
func runChat5WithTask(panel *Panel, account25 *auth.Auth, t *upstream.Task) (string, error) {
	if t == nil {
		return "", errStatic
	}

	target := t.Target
	if target <= 0 {
		target = 5
	}

	need := target - t.Current
	if need <= 0 {
		return "进度已达标，无需上报", nil
	}

	for i := range need {
		cid := session.NewUUIDv4()
		if err := panel.cfg.Upstream.ReportChatActivity(account25, cid, session.NewUUIDv7Hex()); err != nil {
			return fmt.Sprintf("上报第 %d/%d 条失败: %v", i+1, need, err), nil
		}

		if i < need-1 {
			time.Sleep(reportGap)
		}
	}

	return fmt.Sprintf("已补报 %d 条对话事件", need), nil
}

// runFirstBuddy 领养：report（解锁前置）→ agreement → first。
func runFirstBuddy(panel2 *Panel, account24 *auth.Auth) (string, error) {
	if err := panel2.cfg.Upstream.ReportChatActivity(
		account24, session.NewUUIDv4(), session.NewUUIDv7Hex(),
	); err != nil {
		return "", fmt.Errorf("前置上报: %w", err)
	}

	time.Sleep(reportGap) // 给上游事件处理留时间（脚本实测口径）

	if err := panel2.cfg.Upstream.BuddyAgreement(account24); err != nil {
		return "", fmt.Errorf("同意协议: %w", err)
	}

	if err := panel2.cfg.Upstream.BuddyFirst(account24); err != nil {
		if upstream.IsBuddyTaskIncomplete(err) {
			return "前置已上报，但领养门槛未过（上游要求当日活跃），请稍后重试", nil
		}

		return "", fmt.Errorf("领取 Buddy: %w", err)
	}

	return "已领取 Buddy（+300 分 +8 能量）", nil
}

// runModelChat 完成 Model_chat_GLM5.2：accept → 真实对话 → 对齐模型上报。
func runModelChat(panel9 *Panel, account18 *auth.Auth) (string, error) {
	const code, modelID, modelName = "Model_chat_GLM5.2", "glm-5.2", "GLM-5.2"
	// 1. accept（报名；失败不阻塞——行为事件才是判据）
	if err := panel9.cfg.Upstream.AcceptTasks(account18, []string{code}); err != nil {
		log.Printf("panel: accept %s: %v（继续走行为链路）", code, err)
	}

	time.Sleep(reportGap)
	// 2. 真实对话一次（判据的最直接证据）
	body, merr := json.Marshal(map[string]any{
		"model": modelID,
		"messages": []map[string]any{
			{"role": "user", "content": "hi，请回复一句话"},
		},
		"stream": true,
	})
	if merr != nil {
		return "", fmt.Errorf("marshal chat body: %w", merr)
	}

	rc, status, respBody, err := panel9.cfg.Upstream.ChatStream(account18, body, "", upstream.ChatMeta{})
	if err != nil {
		return "", fmt.Errorf("对话请求: %w", err)
	}

	if status >= http.StatusBadRequest {
		rc.Close()

		return "", fmt.Errorf("对话失败 http=%d: %s: %w",
			status, truncateStr(string(respBody), 160), errRunModelChatFailed)
	}
	// 读干 SSE（网关对上游强制 flow：不读完会残留连接）
	_, _ = io.Copy(io.Discard, io.LimitReader(rc, 1<<20))
	rc.Close()
	time.Sleep(reportGap)
	// 3. 对齐模型的上报（触发进度）
	cid := session.NewUUIDv4()
	if err := panel9.cfg.Upstream.ReportChatActivityModel(
		account18, cid, session.NewUUIDv7Hex(), modelID, modelName,
	); err != nil {
		return "对话已完成，但进度上报失败：" + err.Error(), nil //nolint:nilerr // 对话已完成，上报失败仅提示不算请求错误
	}

	return "已完成 glm-5.2 对话并上报", nil
}

// runRichMeow 完成 RichMeow_Chat（桌面端对话1次）。
// 2026-09-12 三账号实测验证：以桌面指纹（extName=workbuddy-desktop）向
// chatBase 的 /v2/report 上报完整对话事件链（agent_task_created →
// chat_message_response isSuccessful=true 等 6 事件），纯 API 即可点亮并领奖
// （紫川/人杰2 两账号无桌面客户端登录状态下 3 秒内 0/1 → 1/1）。
// Hp_Appearance 的纯 API set 不计分（需客户端在主题下活跃），区别对待。
func runRichMeow(panel3 *Panel, account *auth.Auth) (string, error) {
	conv := session.NewUUIDv4()
	req := session.NewUUIDv7Hex()
	msg := session.NewUUIDv7Hex()

	events := upstream.DesktopChatSequence(conv, req, msg, "fast-model", "fast-model")
	if err := panel3.cfg.Upstream.ReportDesktopEvent(account, events...); err != nil {
		return "", fmt.Errorf("runRichMeow: %w", err)
	}

	return "已按桌面端指纹上报完整对话事件链（agent_task_created→chat_response）", nil
}

// runBuddyApp 完成 Buddy_App / Buddy_App_QQ（进入 Buddy 应用）。
// 2026-09-12 两账号实测：buddyapp 五连事件（discover→show→enter→auth_confirm→
// bind_skip）以 workbuddy-desktop 指纹上报即点亮，服务端不校验真实授权。
// 用企鹅教师助手（Buddy_App_QQ 判据应用）作载体，同一组事件同时满足
// Buddy_App「进入任一应用」——两个表项共用本 run，幂等由任务状态跳过兜底。
func runBuddyApp(p *Panel, a *auth.Auth) (string, error) {
	events := upstream.DesktopBuddyAppSequence("cb_y5Dy46tPQGGWtueMxXbe", "企鹅教师助手")
	if err := p.cfg.Upstream.ReportDesktopEvent(a, events...); err != nil {
		return "", fmt.Errorf("runBuddyApp: %w", err)
	}

	return "已上报 buddyapp 进入五连事件（同时覆盖 Buddy_App 与 Buddy_App_QQ）", nil
}

// runAutomationCreate 完成 automation_1（设置自动化任务）。
// 2026-09-12 两账号实测：automated_task_create_suc 事件纯 API 上报即点亮，
// 无需真实创建定时任务。
func runAutomationCreate(p *Panel, a *auth.Auth) (string, error) {
	if err := p.cfg.Upstream.ReportDesktopEvent(a,
		upstream.DesktopAutomationCreateEvent("日常任务提醒")); err != nil {
		return "", fmt.Errorf("runAutomationCreate: %w", err)
	}

	return "已上报定时任务创建事件", nil
}

// runLibraryRead 完成 Library_read（体验资料库）。
// 2026-09-12 三账号实测：web 域 /v2/report 上报 web_element_click
// (elementId=library_doc_intro_click) 即点亮（space 的 open/WS/inlong 都不是判据）。
func runLibraryRead(p *Panel, a *auth.Auth) (string, error) {
	const docURL = "https://www.workbuddy.cn/space/d/o0KWYeynteVv06UnAZqIFm"
	if err := p.cfg.Upstream.ReportWebEvent(a, "web_element_click", docURL,
		"library_doc_intro_click", "WorkBuddy资料库介绍"); err != nil {
		return "", fmt.Errorf("runLibraryRead: %w", err)
	}

	return "已上报资料库介绍阅读事件", nil
}

// runBlackCat 完成 black_cat（夜猫子，夜间 23:00–08:00 CST 计数）。
// 判据 = 夜间窗口内 glm-5.2 真实对话 + chat 事件上报（WorkBuddy-Daily 实测口径）。
// 窗口外不做（提示等排程）；网关 blackcat_hours（默认 23 点 CST）排程会自动补足。
func runBlackCat(panel6 *Panel, account13 *auth.Auth) (string, error) {
	if !upstream.InNightWindow(time.Now()) {
		return "当前不在 23:00–08:00（CST）计数窗口，行为不计分；网关会在每日 23 点 CST 自动补足", nil
	}

	need, err := panel6.cfg.Upstream.BlackcatNeed(account13)
	if err != nil {
		return "", fmt.Errorf("runBlackCat: %w", err)
	}

	if need <= 0 {
		return "进度已达标，无需补足", nil
	}

	ok, err := panel6.cfg.Upstream.RunNightChats(account13, int(need))
	if err != nil {
		return fmt.Sprintf("完成 %d/%d 次后中断: %v", ok, need, err), nil
	}

	return fmt.Sprintf("已完成 %d 次夜间对话并上报", ok), nil
}

// runSkillFresh 完成 skill_1（尝鲜热门技能）。
// 2026-09-12 判据（紫川手动完成抓包 row 209）：`skill_info` 事件（桌面指纹）——
// {id:<技能名>, skillId, skillVersion, toolStatus:"success", fileCount,
// source:"workbuddy-desktop"} JOIN 真实会话（conversationId/requestId=服务端
// id）。此前的 skill_request_send/skill_installed/skill_action 全是错误方向。
// 人杰2 实测 0/1 → 1/1 点亮。
func runSkillFresh(panel7 *Panel, account16 *auth.Auth) (string, error) {
	conv, req, err := panel7.cfg.Upstream.DesktopChatWithExpert(account16, "")
	if err != nil {
		return "", fmt.Errorf("真实对话: %w", err)
	}

	msgID := session.NewUUIDv7Hex()

	events := upstream.DesktopChatSequence(conv, req, msgID, "fast-model", "fast-model")
	for _, ev := range events {
		if ev["eventCode"] == "chat_message_response" {
			ev["finishReason"] = "tool_calls" // 模型发起工具调用（技能加载）语义
		}
	}

	events = append(events, upstream.DesktopEvent{
		"eventCode":      "skill_info",
		"id":             "润泽小馆·日报撰写",
		"skillId":        "skill_2097350077599879168",
		"skillVersion":   "1.0.0",
		"toolStatus":     "success",
		"fileCount":      56,
		"source":         "workbuddy-desktop",
		"conversationId": conv, "requestId": req, "messageId": msgID,
		"requestModelId": "fast-model", "requestModelName": "fast-model",
		"traceId": req,
	})
	if err := panel7.cfg.Upstream.ReportDesktopEvent(account16, events...); err != nil {
		return "", fmt.Errorf("skill_info 事件: %w", err)
	}

	return "已上报真实对话 + skill_info 技能加载事件", nil
}

// runValorant 完成 WorkBuddy_Valorant「体验『无畏契约拼豆专家』」（issue #257）。
//
// 动作链复用 Expert_lighthouse：召唤事件组 + 真实桌面 chat（服务端 requestId）+
// expert_actual_use(mode=LOCAL)。专家 id 来自参考实现 530f1df
// （ex_xQHHD1u5Jkua / valorant-perler-expert / 1.2.1）；市场列表命中时以服务端
// 下发的 version/名称等为准。活动截止 2026-11-15，过期后上游不再计数。
func runValorant(panel *Panel, account *auth.Auth) (string, error) {
	return runExpertTask(panel, account, "ex_xQHHD1u5Jkua", "无畏契约拼豆专家", "1.2.1",
		"已上报无畏契约拼豆专家召唤+使用链（真实对话 requestId）")
}

// runExpertLighthouse 完成 Expert_lighthouse（体验「腾讯轻量云」专家）。
// 2026-09-12 判据（真实样本 Sunny row 868）：与 expert_5 同构，但两处差异——
// chat 链的 agent_task_created 需带 has_expert:true + expert_id（我们默认 false），
// expert_actual_use 的 mode 为 "LOCAL"（非 craft）。id 固定为轻量云专家
// ex_2cvvUZQhDyeJ；requestId 必须是真实 chat 的服务端 id。紫川/人杰2 实测点亮。
func runExpertLighthouse(panel *Panel, account *auth.Auth) (string, error) {
	return runExpertTask(panel, account, "ex_2cvvUZQhDyeJ", "腾讯轻量云专家", "1.0.2",
		"已上报轻量云专家召唤+使用链（真实对话 requestId）")
}

// runExpertTask 按固定 id 召唤专家并跑公共事件链；市场列表命中时以服务端下发的
// version/名称等为准。
func runExpertTask(
	panel *Panel, account *auth.Auth, id, nameZh, version, report string,
) (string, error) {
	expert := upstream.MarketExpert{
		ExpertID: id, ExpertType: "agent",
		DisplayNameZH: nameZh, ProfessionZH: nameZh, Version: version,
		Categories: nil,
	}

	if experts, err := panel.cfg.Upstream.MarketExpertList(account, "agent"); err == nil {
		for _, e := range experts {
			if e.ExpertID == id {
				expert = e
				break
			}
		}
	}

	if err := runExpertChain(panel, account, &expert); err != nil {
		return "", err
	}

	return report, nil
}

// runExpertChain 平台专家的「召唤 + 真实对话 + LOCAL 使用」公共事件链。
// 两处判据差异（has_expert / expert_actual_use type+mode）统一在这里对齐。
func runExpertChain(panel *Panel, account *auth.Auth, expert *upstream.MarketExpert) error {
	if err := panel.cfg.Upstream.ReportDesktopEvent(
		account, upstream.DesktopExpertSummonSequence(expert)...,
	); err != nil {
		return fmt.Errorf("召唤链: %w", err)
	}

	conv, req, err := panel.cfg.Upstream.DesktopChatWithExpert(account, expert.ExpertID)
	if err != nil {
		return fmt.Errorf("真实对话: %w", err)
	}

	events := upstream.DesktopChatSequence(conv, req, session.NewUUIDv7Hex(), "fast-model", "fast-model")
	for _, ev := range events {
		if ev["eventCode"] == "agent_task_created" {
			ev["has_expert"] = true
			ev["expert_id"] = expert.ExpertID
			ev["expert_name"] = expert.DisplayNameZH
			ev["expert_industry_id"] = ""
		}
	}

	events = append(events, upstream.DesktopExpertActualUseLocal(expert, conv, req))
	// 对齐真实样本细节：LOCAL 专家 actual_use 的 type 为空、cost=0。
	events[len(events)-1]["type"] = ""
	events[len(events)-1]["cost"] = 0

	if err := panel.cfg.Upstream.ReportDesktopEvent(account, events...); err != nil {
		return fmt.Errorf("使用事件: %w", err)
	}

	return nil
}

// runAppearance 完成 Hp_Appearance（换主题）。
// 2026-09-12 紫川/人杰2 实测（desktopverify12）：判据是 appearance_skin_apply
// 事件 {action:"apply",source:"settings_close",id:<resourceKey>,...}（客户端在
// 主题生效状态下离开设置页时上报）——早期"纯 API set 不计分"的结论不准确，
// 真相是当时只调了 appearance/set 没发事件。组合：set API 留痕 + 事件上报。
func runAppearance(panel3 *Panel, account2 *auth.Auth) (string, error) {
	const themeKey = "theme-tkmw7j" // 和平精英激战金秋（Hp_Appearance 判据主题）
	if err := panel3.cfg.Upstream.SetAppearanceTheme(account2, themeKey); err != nil {
		return "", fmt.Errorf("设置主题: %w", err)
	}

	time.Sleep(2 * time.Second)

	if err := panel3.cfg.Upstream.ReportDesktopEvent(account2, upstream.DesktopEvent{
		"eventCode": "appearance_skin_apply", "action": "apply", "source": "settings_close",
		"id": themeKey, "vipLevel": 0, "series": "", "type": "unknown",
	}); err != nil {
		return "", fmt.Errorf("runAppearance: %w", err)
	}

	return "已设置主题并上报皮肤生效事件", nil
}

// runTemplateUse 完成 template_5（使用 5 个模板创建任务）。
// 2026-09-12 三账号实测：agent_task_created_with_template + template_used 事件组
// （JOIN chat 链）一次上报 5 组即 5/5 点亮。template_id 服务端不校验真实性。
func runTemplateUse(panel5 *Panel, account17 *auth.Auth) (string, error) {
	templates := [][2]string{{"1", "深度研究"}, {"2", "周报生成"}, {"3", "竞品分析"}, {"4", "活动策划"}, {"5", "代码评审"}}
	for i, tp := range templates {
		conv := session.NewUUIDv4()
		req := session.NewUUIDv7Hex()

		events := upstream.DesktopTemplateUseSequence(conv, req, tp[0], tp[1])
		if err := panel5.cfg.Upstream.ReportDesktopEvent(account17, events...); err != nil {
			return fmt.Sprintf("第 %d 组模板事件上报失败: %v", i+1, err), nil
		}

		time.Sleep(300 * time.Millisecond)
	}

	return "已上报 template_used ×5", nil
}

// runPlaybookPrompt 完成 playbook_prompt（灵感案例 Dialog 中发送 Prompt）。
// 判据是 playbook_prompt_send（Dialog 发送）而非卡片曝光/点击——asar 逆向确认。
func runPlaybookPrompt(panel10 *Panel, account4 *auth.Auth) (string, error) {
	conv := session.NewUUIDv4()
	req := session.NewUUIDv7Hex()

	events := upstream.DesktopPlaybookPromptSequence(conv, req, "pm-gtm-launch-plan", "新产品上市 GTM 发布计划一页纸")
	if err := panel10.cfg.Upstream.ReportDesktopEvent(account4, events...); err != nil {
		return "", fmt.Errorf("runPlaybookPrompt: %w", err)
	}

	return "已上报 playbook_cta_click + playbook_prompt_send", nil
}

// runCreateCanvas 完成 create_canvas（设计创意模式创建画布，+300 分）。
// 判据是 wbx_design_canvas_task_create/open（Ardot create_design 工具完成遥测）。
func runCreateCanvas(panel11 *Panel, account3 *auth.Auth) (string, error) {
	conv := session.NewUUIDv4()
	req := session.NewUUIDv7Hex()

	events := upstream.DesktopDesignCanvasSequence(conv, req)
	if err := panel11.cfg.Upstream.ReportDesktopEvent(account3, events...); err != nil {
		return "", fmt.Errorf("runCreateCanvas: %w", err)
	}

	return "已上报 wbx_design_canvas_task_create/open", nil
}

// expertSummonGap 专家召唤链的间隔（真实使用节奏，v11 实测 8s 成功率 100%）。
const expertSummonGap = 6 * time.Second

// runExpertUse 完成 expert_5（使用 5 个平台专家）。
// 2026-09-12 三账号实测公式：真实专家列表（id 必须真实存在）→ 召唤链
// （summon_click/summoned）→ 真实 chat 拿服务端 requestId → expert_actual_use。
// 自造专家 id 或自造 requestId 均不计数。
func runExpertUse(p *Panel, a *auth.Auth) (string, error) {
	return runExpertBatch(p, a, "agent", 5)
}

// runExpertTeamUse 完成 Expert_team_use_3（使用 3 个专家团，expertType=team）。
func runExpertTeamUse(p *Panel, a *auth.Auth) (string, error) {
	return runExpertBatch(p, a, "team", 3)
}

// runExpertOnce 完成一位专家的召唤链、真实 chat 与使用上报。
func runExpertOnce(panelRef *Panel, account *auth.Auth, marketExpert *upstream.MarketExpert) error {
	summonEvents := upstream.DesktopExpertSummonSequence(marketExpert)
	if err := panelRef.cfg.Upstream.ReportDesktopEvent(account, summonEvents...); err != nil {
		return fmt.Errorf("上报召唤链: %w", err)
	}

	conv, req, err := panelRef.cfg.Upstream.DesktopChatWithExpert(account, marketExpert.ExpertID)
	if err != nil {
		return fmt.Errorf("专家真实对话: %w", err)
	}

	events := append(upstream.DesktopChatSequence(conv, req, session.NewUUIDv7Hex(), "fast-model", "fast-model"),
		upstream.DesktopExpertActualUseEvent(marketExpert, conv, req))
	if err := panelRef.cfg.Upstream.ReportDesktopEvent(account, events...); err != nil {
		return fmt.Errorf("上报使用链: %w", err)
	}

	return nil
}

// runExpertBatch 专家召唤+使用的公共实现。失败逐个继续，返回汇总信息。
func runExpertBatch(panel8 *Panel, account15 *auth.Auth, expertType string, count int) (string, error) {
	experts, err := panel8.cfg.Upstream.MarketExpertList(account15, expertType)
	if err != nil {
		return "", fmt.Errorf("拉取专家列表: %w", err)
	}

	if len(experts) == 0 {
		return "", errStatic2
	}

	ok, fail := 0, 0

	var lastErr error

	for numVal, marketExpert := range experts {
		if ok >= count {
			break
		}

		if err := runExpertOnce(panel8, account15, &marketExpert); err != nil {
			fail++

			lastErr = err

			continue
		}

		ok++

		// 已达目标时下一轮会 break，不应再为已完成的批量多等一个间隔。
		if ok < count && numVal < len(experts)-1 {
			time.Sleep(expertSummonGap)
		}
	}

	// 一次都没成功时不能返回 nil：调用方（autoRunAction）会把 nil 当 done，
	// 任务中心就会把「0 位完成」展示成成功。
	if ok == 0 {
		return "", fmt.Errorf("%w（%d 次尝试）: %w", errExpertAllFailed, fail, lastErr)
	}

	msg := fmt.Sprintf("已对 %d 位真实专家完成召唤+使用链（类型 %s）", ok, expertType)
	if fail > 0 {
		msg += fmt.Sprintf("，%d 次尝试失败", fail)
	}

	return msg, nil
}

// ---------------------------------------------------------------------------
// 全量自动完成
// ---------------------------------------------------------------------------

// runAutoAll 对单账号依次执行所有可自动化任务，返回逐项结果。
// 供「一键完成全部可自动任务」使用；单项失败不影响后续项。
//
// 流程：先把所有未接受的任务批量 accept（规范状态机；上游脚本建议"先 accept"），
// 再逐项执行行为链路。accept 不是进度产生的必要条件，但让后续状态流转规范。
func (p *Panel) runAutoAll(account6 *auth.Auth) []map[string]any {
	return p.runAutoAllActions(account6, autoActions)
}

// runAutoAllActions 使用一份执行前任务快照跑指定动作表。测试可传入独立动作表，
// 不修改进程级 autoActions。
func (p *Panel) runAutoAllActions(account6 *auth.Auth, actions []autoAction) []map[string]any {
	snapshot := &autoTaskSnapshot{}
	out := make([]map[string]any, 0, len(actions)+2)
	// 阶段 0/0b：复用执行前快照批量接受尚未接受的任务（失败不阻塞）。
	snapshot.defaultTasks, snapshot.defaultErr = p.cfg.Upstream.ListTasks(account6)
	snapshot.defaultByCode = indexTasksByCode(snapshot.defaultTasks)
	out = append(out, p.autoAcceptBatch(account6, snapshot.defaultTasks)...)
	// mp 列表是默认口径超集：默认批量 accept 后再读，避免同一任务被两套接口
	// 各接受一次（旧实现的两份列表分别读取，天然带有该先后顺序）。
	snapshot.mpTasks, snapshot.mpErr = p.cfg.Upstream.ListTasksMP(account6)
	snapshot.mpByCode = indexTasksByCode(snapshot.mpTasks)
	out = append(out, p.autoAcceptBatchMP(account6, snapshot.mpTasks)...)
	// mp 批量 accept 成功后，上面的列表仍是接受前状态（not_accepted），而
	// acceptMPTaskIfNeeded/acceptMPAndRefresh 会据此重复 accept。批量接受后
	// 再读一次拿真实登记状态；失败时保留旧快照，动作侧仍按码 accept+回读验证。
	if len(pendingTaskCodes(snapshot.mpTasks)) > 0 {
		if mpTasks, err := p.cfg.Upstream.ListTasksMP(account6); err == nil {
			snapshot.mpTasks = mpTasks
			snapshot.mpByCode = indexTasksByCode(mpTasks)
		}
	}

	for _, act := range actions {
		out = append(out, p.autoRunActionWithSnapshot(account6, act, snapshot))
	}

	return out
}

// autoTaskSnapshot 一轮「一键完成」共用的默认/mp 任务列表。
// 动作后回读不复用该快照：上游计分异步，执行后必须重新读取最新状态。
type autoTaskSnapshot struct {
	defaultTasks  []upstream.Task
	defaultErr    error
	mpTasks       []upstream.Task
	mpErr         error
	defaultByCode map[string]upstream.Task
	mpByCode      map[string]upstream.Task
}

// indexTasksByCode 按 task_code 建索引；重复项保留首个，与列表查找语义一致。
func indexTasksByCode(tasks []upstream.Task) map[string]upstream.Task {
	byCode := make(map[string]upstream.Task, len(tasks))
	for i := range tasks {
		if _, exists := byCode[tasks[i].TaskCode]; !exists {
			byCode[tasks[i].TaskCode] = tasks[i]
		}
	}

	return byCode
}

// lookupBeforeAction 从执行前快照定位任务。resolved=false 表示相关列表读取失败，
// 调用方应回退到 taskByCode 的既有逐项查询/错误语义。
func (s *autoTaskSnapshot) lookupBeforeAction(code string) (*upstream.Task, bool) {
	if isMPTaskCode(code) {
		return s.lookupMPBeforeAction(code)
	}

	// 非 mp 任务只有默认列表可判定；读取失败时交回 taskByCode 的既有错误语义。
	if s.defaultErr != nil {
		return nil, false
	}

	if t, ok := taskFromSnapshot(s.defaultByCode, code); ok {
		return t, true
	}

	// 默认列表读取成功但无此任务：可确定列表外，不再逐项查询。
	return nil, true
}

// lookupMPBeforeAction 定位 mp 专属任务。mp 列表是权威口径：两口径同时下发
// 同一 code 时必须取 mp 快照，否则默认列表 accept 前的旧状态会绕过批量接受后
// 的 mp 刷新；仅 mp 列表读取失败时才回退默认快照。
func (s *autoTaskSnapshot) lookupMPBeforeAction(code string) (*upstream.Task, bool) {
	if s.mpErr == nil {
		if t, ok := taskFromSnapshot(s.mpByCode, code); ok {
			return t, true
		}

		return nil, true
	}

	if s.defaultErr != nil {
		return nil, false
	}

	return taskFromSnapshot(s.defaultByCode, code)
}

// taskFromSnapshot 从索引取任务副本；未命中返回 false。
func taskFromSnapshot(byCode map[string]upstream.Task, code string) (*upstream.Task, bool) {
	t, ok := byCode[code]
	if !ok {
		return nil, false
	}

	return &t, true
}

// autoAcceptBatch 批量接受默认口径未接受任务，返回结果条目（列表失败静默）。
//
//nolint:dupl // CN/MP 两套上游接口并排，保持结构对照。
func (p *Panel) autoAcceptBatch(account21 *auth.Auth, tasks []upstream.Task) []map[string]any {
	codes := pendingTaskCodes(tasks)
	if len(codes) == 0 {
		return nil
	}

	if err := p.cfg.Upstream.AcceptTasks(account21, codes); err != nil {
		return []map[string]any{{
			"task_code": "(批量接受)", "status": "error",
			"message": "接受任务失败（不阻塞后续）: " + err.Error(),
		}}
	}

	time.Sleep(reportGap)

	return []map[string]any{{
		"task_code": "(批量接受)", "status": "done",
		"message": fmt.Sprintf("已接受 %d 个任务", len(codes)),
	}}
}

// autoAcceptBatchMP 批量接受小程序口径未接受任务，返回结果条目（列表失败静默）。
//
//nolint:dupl // 同 autoAcceptBatch：两套接口结构对照。
func (p *Panel) autoAcceptBatchMP(account11 *auth.Auth, mpTasks []upstream.Task) []map[string]any {
	codes := pendingTaskCodes(mpTasks)
	if len(codes) == 0 {
		return nil
	}

	if err := p.cfg.Upstream.AcceptTasksMP(account11, codes); err != nil {
		return []map[string]any{{
			"task_code": "(批量接受-mp)", "status": "error",
			"message": "接受小程序任务失败（不阻塞后续）: " + err.Error(),
		}}
	}

	time.Sleep(reportGap)

	return []map[string]any{{
		"task_code": "(批量接受-mp)", "status": "done",
		"message": fmt.Sprintf("已接受 %d 个小程序任务", len(codes)),
	}}
}

// autoRunAction 执行单个任务动作并组装结果条目（单项失败不影响后续项）。
func (p *Panel) autoRunAction(account5 *auth.Auth, act autoAction) map[string]any {
	return p.autoRunActionWithSnapshot(account5, act, nil)
}

// autoRunActionWithSnapshot 优先使用一轮共享快照做执行前检查；快照未覆盖时
// 回退 taskByCode。动作后仍通过 taskByCodeAfterAction 读取最新异步进度。
//
//nolint:funlen // 动作分派/结果组装线性展开。
func (p *Panel) autoRunActionWithSnapshot(
	account5 *auth.Auth, act autoAction, snapshot *autoTaskSnapshot,
) map[string]any {
	item := map[string]any{"task_code": act.TaskCode, "desc": act.Desc}

	before, err := p.taskForAutoAction(account5, act.TaskCode, snapshot)
	if err != nil {
		item["status"] = "error"
		item["message"] = "查询失败: " + err.Error()

		return item
	}

	if before == nil {
		item["status"] = "skipped"
		item["message"] = "该账号无此任务"

		return item
	}

	if before.Claimed {
		item["status"] = "skipped"
		item["message"] = "已领取（" + taskProgressText(before) + "）"

		return item
	}
	// 已达标未领：先补领再跳过。此前这里直接标 skipped，奖励就一直躺在那里
	// （issue #174）。判据只用 progress（不依赖 accept_status=="completed"，
	// 上游该字段在部分任务上不更新）。
	if before.Target > 0 && before.Current >= before.Target {
		item["status"] = "skipped"
		// autoClaimItem 会把 claimed/credit/energy/claim_error 写回条目，并把 message
		// 覆写为「已完成（…）；已自动领奖 +N 分 +M 能」（失败则带可读 claim_error）。
		p.autoClaimItem(account5, act.TaskCode, "已完成（"+taskProgressText(before)+"）", item)

		return item
	}

	msg, err := p.executeAutoAction(account5, act, before, snapshot)
	if err != nil {
		item["status"] = "error"
		item["message"] = err.Error()

		return item
	}

	after, _ := p.taskByCodeAfterAction(account5, act.TaskCode)
	item["status"] = "done"
	item["message"] = msg
	item["progress_after"] = taskProgressText(after)
	// 进度达标即自动领奖（mp 任务走 chat 域 mp 口径，其余 Web 端接口）。
	// 领奖失败不掩盖主流程结果：status 仍为 done，附加 claim_error 供前端提示。
	if after != nil && after.Claimable {
		item["claimable"] = true
		p.autoClaimItem(account5, act.TaskCode, msg, item)
	}

	time.Sleep(reportGap) // 项间节流（仅成功路径，与既有语义一致）

	return item
}

// taskForAutoAction 优先从共享快照定位任务；相关口径读取失败时逐项回退查询。
func (p *Panel) taskForAutoAction(
	account *auth.Auth, code string, snapshot *autoTaskSnapshot,
) (*upstream.Task, error) {
	if snapshot != nil {
		if before, resolved := snapshot.lookupBeforeAction(code); resolved {
			return before, nil
		}
	}

	return p.taskByCode(account, code)
}

// executeAutoAction 在共享快照路径优先复用已定位任务，避免动作内部重复拉列表。
func (p *Panel) executeAutoAction(
	account *auth.Auth, act autoAction, before *upstream.Task, snapshot *autoTaskSnapshot,
) (string, error) {
	if snapshot != nil && act.runWithTask != nil {
		return act.runWithTask(p, account, before)
	}

	return act.run(p, account)
}

// autoClaimItem 达标自动领奖，把结果写进结果条目（claim_error 供前端提示）。
//
//nolint:dupl // 同 autoClaimResponse：输入形态不同但流程一致。
func (p *Panel) autoClaimItem(a *auth.Auth, code, msg string, item map[string]any) {
	credit, energy, cerr := p.claimTaskReward(a, code)
	if cerr != nil {
		item["claim_error"] = cerr.Error()
		item["message"] = msg + "；达标但领奖失败（可在列表手动重试）"

		return
	}

	item["claimed"] = true
	item["credit"] = credit

	item["energy"] = energy
	if credit > 0 || energy > 0 {
		item["message"] = msg + fmt.Sprintf("；已自动领奖 +%d 分 +%d 能", credit, energy)
	} else {
		item["message"] = msg + "；奖励此前已领取"
	}
}

// accountTaskAutoAll 一键完成该账号全部可自动任务。
func (p *Panel) accountTaskAutoAll(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")

	account := p.eligibleTaskAccount(w, uid)
	if account == nil {
		return
	}
	// per-account 互斥（与单任务动作共用一把锁）：重复点击 409。
	if !p.tryLockAccount(uid) {
		writeErr(w, http.StatusConflict, "该账号有任务动作正在执行中，请等本轮结束后再试")
		return
	}
	// 用 context 兜底超时（多项任务串联 + 每项含真实对话，可能耗时较长）。
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()

	done := make(chan []map[string]any, 1)

	go func() {
		defer p.unlockAccount(uid) // 流水线真正结束（而非 HTTP 超时返回）才放锁

		done <- p.runAutoAll(account)
	}()

	select {
	case results := <-done:
		log.Printf("panel: 一键完成可自动任务 uid=%s 共 %d 项", uid, len(results))
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "results": results})
	case <-ctx.Done():
		// HTTP 侧超时返回，但后台流水线仍在跑——锁在流水线 goroutine 内释放，
		// 期间重复点击会被 409 挡住，不会出现两轮并发。
		writeErr(w, http.StatusGatewayTimeout, "执行超时（任务仍在后台继续）")
	}
}
