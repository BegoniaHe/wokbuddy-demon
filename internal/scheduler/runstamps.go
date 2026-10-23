package scheduler

import (
	"encoding/json"
	"log"
	"time"

	"github.com/BegoniaHe/wokbuddy-demon/internal/cst"
	"github.com/BegoniaHe/wokbuddy-demon/internal/safefile"
	"github.com/BegoniaHe/wokbuddy-demon/internal/upstream"
)

// ranToday 报告 kind 今天是否已经补跑过。未配置 RunStampFile 时恒 false。
func (s *Scheduler) ranToday(kind taskKind, now time.Time) bool {
	if s.cfg.RunStampFile == "" {
		return false
	}

	stamps := s.loadStamps()

	return stamps[kind.String()] == runDay(kind, now)
}

// markRan 把 kinds 记成「今天已补跑」。落盘失败只记日志：下次重启多补一轮，
// 比让签到状态机因写盘错误停摆好。
func (s *Scheduler) markRan(kinds []taskKind, now time.Time) {
	if s.cfg.RunStampFile == "" || len(kinds) == 0 {
		return
	}

	stamps := s.loadStamps()
	stamps[stampBoundaryKey] = stampBoundaryValue

	for _, kind := range kinds {
		stamps[kind.String()] = runDay(kind, now)
	}

	raw, err := json.Marshal(stamps)
	if err == nil {
		err = safefile.Write(s.cfg.RunStampFile, raw, 0o600)
	}

	if err != nil {
		log.Printf("scheduler: 补跑标记落盘失败（下次重启会再补一轮）: %v", err)
	}
}

// runDay 返回任务补跑标记使用的日界：所有任务统一按 CST（与排程一致，issue #252）。
// 夜猫子窗口跨午夜：00:00–07:59 的补跑属于前一晚 23:00 开启的窗口，记到前一日。
func runDay(kind taskKind, now time.Time) string {
	now = now.In(cst.Loc)

	if kind == taskBlackcat && now.Hour() < 8 {
		now = now.AddDate(0, 0, -1)
	}

	return now.Format("2006-01-02")
}

// stampBoundaryKey/Value 标记 runstamp 使用的日界版本。旧版文件没有该键
// （宿主本地日界），升级后直接忽略，允许按 CST 重新补跑一轮（issue #252）。
const (
	stampBoundaryKey   = "_boundary"
	stampBoundaryValue = "cst"
)

// loadStamps 读标记；文件不在/坏了都当空表（宁可多补一轮，不因此不跑）。
// 旧版本地日界标记整体作废：升级首次运行按 CST 重算，避免漏跑/重跑。
func (s *Scheduler) loadStamps() map[string]string {
	raw, err := safefile.Read(s.cfg.RunStampFile, nil)
	if err != nil {
		return map[string]string{}
	}

	stamps := map[string]string{}
	if json.Unmarshal(raw, &stamps) != nil {
		return map[string]string{}
	}

	if stamps[stampBoundaryKey] != stampBoundaryValue {
		return map[string]string{}
	}

	return stamps
}

// missedToday 返回「今天的时点已全部过去」的启用任务族（下一次触发已落到别的日子）。
// 夜猫子是跨午夜窗口：now 落在 23:00–08:00 内但今天的 23:00 尚未到时，上一次窗口
// 起点仍可能漏跑，按窗口补一次（ranToday 去重）。
func (s *Scheduler) missedToday(now time.Time) []taskKind {
	s.schedMu.Lock()
	blackcatEnabled := !s.cfg.BlackcatDisabled && len(s.cfg.BlackcatHours) > 0
	s.schedMu.Unlock()

	var out []taskKind

	blackcatQueued := false

	for _, slot := range s.candidateSlots(now) {
		if !s.slotMissed(slot, now) {
			continue
		}

		out = append(out, slot.kind)

		if slot.kind == taskBlackcat {
			blackcatQueued = true
		}
	}

	if blackcatEnabled && !blackcatQueued && upstream.InNightWindow(now) && !s.ranToday(taskBlackcat, now) {
		out = append(out, taskBlackcat)
	}

	return out
}

// slotMissed 报告该任务的时点是否已全部过去（下一次触发落到别的日子）且未补跑。
func (s *Scheduler) slotMissed(slot wakeSlot, now time.Time) bool {
	if slot.at.IsZero() {
		return false
	}

	slotNow := now.In(slot.at.Location())
	year, month, day := slotNow.Date()

	slotYear, slotMonth, slotDay := slot.at.Date()
	if slotYear == year && slotMonth == month && slotDay == day {
		return false // 今天还有时点
	}

	return !s.ranToday(slot.kind, now)
}

// taskKind.String 供 runstamp 键使用。
func (k taskKind) String() string {
	switch k {
	case taskCheckin:
		return "checkin"
	case taskTravel:
		return "travel"
	case taskActivity:
		return "activity"
	case taskKeepalive:
		return "keepalive"
	case taskBlackcat:
		return "blackcat"
	case taskGrowth:
		return "growth"
	default:
		return "unknown"
	}
}
