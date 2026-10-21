// Package cst 固定 UTC+8（北京时间）：上游「自然日」的唯一定义。
//
// WorkBuddy 的额度、签到、每日活跃、夜猫子窗口、积分包到期日全部按北京时间
// 零点切日，而网关可能跑在 Docker / NAS / 海外 VPS 上。上游窗口/日界必须由
// 一个常量决定，不能跟随宿主时区；操作者用量报表仍按本地时区。
package cst

import "time"

// Loc 固定 UTC+8 时区（中国无夏令时，1970 年后恒为 +08:00）。
var Loc = time.FixedZone("CST", 8*60*60)

// Now 当前时刻（带 CST 时区信息）。
func Now() time.Time { return time.Now().In(Loc) }

// Day 返回 t 所属的上游自然日（CST），格式 2006-01-02。
func Day(t time.Time) string { return t.In(Loc).Format("2006-01-02") }
