package scheduler

import "time"

// 测试便利入口：只覆盖生产调用 nextWakeDetail 的默认参数组合（丢弃抖动偏移）。
// 生产代码一律走 nextWakeDetail。

// nextWake 返回 now 之后最近的唤醒时刻，以及该时刻需要执行的全部任务。
// 多类任务若配到同一小时（如签到与旅行都含 9），该时刻多类任务需一并执行。
// 排程参数在 schedMu 下快照，与 Reconfigure 的并发写隔离。
func (s *Scheduler) nextWake(now time.Time) (time.Time, []taskKind) {
	earliest, kinds, _ := s.nextWakeDetail(now)

	return earliest, kinds
}

// nextFireIn 是「无抖动」形态的 nextFireFor：仅测试用它断言零窗口下的整点语义。
func nextFireIn(now time.Time, hours []int) time.Time {
	return nextFireFor(now, taskCheckin, hours, nil, 0, "").at
}
