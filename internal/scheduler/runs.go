// runs.go 批量任务的运行级互斥与状态（issue #112）。
//
// 面板的批量入口与调度器共用同一把「运行级」守卫：同一任务族同一时刻最多跑
// 一轮，重叠触发直接跳过（面板返回「运行中」），避免连续点击或「到点派发 +
// 手动触发」并行跑多轮全账号上游任务。
//
// 状态只在进程内（重启丢失，Issue 明确不引入持久化队列）：每轮记录触发来源、
// 开始/结束时间、尝试账号数与成功/失败数，供 GET /panel/api/tasks/runs 查询。
//
// 边界：任务中心「成长任务队列」有独立的 running 语义（panel.queueState），
// 不接入本守卫，也不与这里六个批量任务族互斥；两类任务可同时运行。
package scheduler

import (
	"cmp"
	"context"
	"slices"
	"time"
)

// 批量任务族标识（与排程 kind 解耦：growth 队列有自己的 running 语义，不在此列）。
const (
	TaskCheckin   = "checkin"
	TaskTravel    = "travel"
	TaskActivity  = "activity"
	TaskKeepalive = "keepalive"
	TaskBlackcat  = "blackcat"
	TaskStreak    = "streak"
)

// 触发来源。
const (
	TriggerScheduler = "scheduler"
	TriggerManual    = "manual"
	// TriggerCheckinFollowup 签到轮末尾顺带触发的连登管家：与独立手动/排程区分，
	// 让面板能看出这一轮 streak 是签到闭环的一部分。
	TriggerCheckinFollowup = "checkin"
)

// TaskRun 一轮批量任务的状态快照（当前轮或最近一轮）。
type TaskRun struct {
	Task       string    `json:"task"`
	Trigger    string    `json:"trigger,omitempty"`
	Running    bool      `json:"running"`
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at,omitzero"`
	// Total 本轮实际尝试的账号数（已跳过禁用/global/企业等不适用账号）。
	Total   int `json:"total"`
	Success int `json:"success"`
	Failure int `json:"failure"`
}

// runSlot 内部状态：TaskRun + 守卫。
type runSlot struct {
	TaskRun

	done chan struct{} // 当前轮完成信号（仅 Running 时有效）
}

// initRunsLocked 延迟初始化（Scheduler 可能由零值/测试直接构造）。
func (s *Scheduler) initRunsLocked() {
	if s.runs == nil {
		s.runs = map[string]*runSlot{}
	}
}

// acquireRun 尝试占用任务族守卫；已在运行返回 false。
func (s *Scheduler) acquireRun(task, trigger string) bool {
	s.runMu.Lock()
	defer s.runMu.Unlock()

	s.initRunsLocked()

	slot := s.runs[task]
	if slot != nil && slot.Running {
		return false
	}

	s.runs[task] = &runSlot{
		Task:       task,
		Trigger:    trigger,
		Running:    true,
		StartedAt:  time.Now(),
		FinishedAt: time.Time{},
		Total:      0,
		Success:    0,
		Failure:    0,
		done:       make(chan struct{}),
	}

	return true
}

// releaseRun 结束当前轮并落状态（保留为「最近一轮」供查询）。
func (s *Scheduler) releaseRun(task string, total, success, failure int) {
	s.runMu.Lock()
	defer s.runMu.Unlock()

	slot := s.runs[task]
	if slot == nil {
		return
	}

	slot.Running = false
	slot.FinishedAt = time.Now()
	slot.Total, slot.Success, slot.Failure = total, success, failure
	close(slot.done)
	slot.done = nil
}

// RunTaskStates 返回全部任务族的当前/最近一轮状态（按任务名排序）。
func (s *Scheduler) RunTaskStates() []TaskRun {
	s.runMu.Lock()
	defer s.runMu.Unlock()

	out := make([]TaskRun, 0, len(s.runs))
	for _, slot := range s.runs {
		out = append(out, slot.TaskRun)
	}

	slices.SortFunc(out, func(a, b TaskRun) int { return cmp.Compare(a.Task, b.Task) })

	return out
}

// TaskRunning 报告某任务族当前是否在运行（供调度器/面板做前置判断）。
func (s *Scheduler) TaskRunning(task string) bool {
	s.runMu.Lock()
	defer s.runMu.Unlock()

	slot := s.runs[task]

	return slot != nil && slot.Running
}

// StartTask 异步启动一轮批量任务（面板手动触发入口）。返回 false = 已在运行，
// 本次未启动。go 1 个 goroutine 后立即返回，状态可经 RunTaskStates 查询。
func (s *Scheduler) StartTask(task, trigger string) bool {
	if !s.acquireRun(task, trigger) {
		return false
	}
	go func() {
		total, success, failure := s.runTaskBody(context.Background(), task, trigger)
		s.releaseRun(task, total, success, failure)
	}()

	return true
}

// runTaskSync 同步执行一轮批量任务（调度器到点派发 + 既有同步调用方）。
// 已在运行（如面板手动触发尚未结束）→ 直接跳过，不并行第二轮。
// 返回是否真正占用守卫并执行了本轮；调用方可用它区分「跑完」与「被跳过」。
func (s *Scheduler) runTaskSync(ctx context.Context, task, trigger string) bool {
	if !s.acquireRun(task, trigger) {
		return false
	}

	total, success, failure := s.runTaskBody(ctx, task, trigger)
	s.releaseRun(task, total, success, failure)

	return true
}

// runTaskBody 按任务族执行实体，返回 (Total, Success, Failure)。
// Total 是本轮实际尝试的账号数；跳过（禁用/不适用 realm/无 token）不计入。
func (s *Scheduler) runTaskBody(ctx context.Context, task, trigger string) (total, success, failure int) {
	switch task {
	case TaskCheckin:
		return s.checkinAll(ctx)
	case TaskTravel:
		return s.travelAll(ctx)
	case TaskActivity:
		return s.activityAll(ctx)
	case TaskKeepalive:
		return s.keepaliveAll(trigger)
	case TaskBlackcat:
		return s.blackcatAll(ctx)
	case TaskStreak:
		return s.streakAll(trigger)
	default:
		return 0, 0, 0
	}
}

// runCounters 单轮账号级成功/失败计数。
type runCounters struct {
	total, success, failure int
}

// record 记一个实际尝试的账号结果。
func (c *runCounters) record(ok bool) {
	c.total++
	if ok {
		c.success++
	} else {
		c.failure++
	}
}
