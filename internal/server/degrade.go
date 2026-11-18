package server

import (
	"log"
	"sync"
	"time"

	"github.com/BegoniaHe/wokbuddy-demon/internal/logfmt"
)

// degradeSource 降级触发源：账号 UID + 入站 system 指纹（issue #591）。
//
// 只用派生值：UID 是池内标识，sysFP 是 sha256 前缀。提示词原文不进 key、不进日志。
type degradeSource struct {
	uid   string
	sysFP string
}

// degradeGate 降级状态机：passthrough/append 模式下请求被上游内容策略拦截时，
// 该**触发源**切换到 Degraded 中性提示词直到次日 00:00 CST 重置。
//
// 定位：误报处理（与 internal/upstream/sanitize.go 同哲学）——内容拦截多为
// system 来源的指纹误报，换最小中性提示词即可绕开；不是对抗框架。
//
// 状态机：进程内存、重启清零（可接受：重启极少触发，且 custom 模式根本
// 不进降级路径）。降级只对命中同一触发源的后续请求生效，其他账号或其他人设
// 的请求保持原文——一次误报不再把整池、全客户端都降到中性提示词。
type degradeGate struct {
	mu sync.Mutex
	// until 按触发源记录的降级截止时刻。过期条目在 Active/Trigger 上顺手清理，
	// 避免长期运行时 map 无界增长（条目数上限 = 账号数 × 客户端人设数）。
	until map[degradeSource]time.Time
	// triggers 进程级触发次数：只用于日志观测，**不**作为「改写所有请求」的开关。
	triggers uint64
}

// Active 报告该触发源当前是否处于降级期（now < until）。
func (g *degradeGate) Active(uid, sysFP string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()

	now := time.Now()
	g.pruneLocked(now)

	return now.Before(g.until[degradeSource{uid: uid, sysFP: sysFP}])
}

// Trigger 触发该源的降级，直到次日 00:00 CST（Asia/Shanghai）。
// 该源已在降级期内则不续期（保持最早触发点的 00:00 重置语义）。
func (g *degradeGate) Trigger(uid, sysFP string) {
	g.mu.Lock()
	defer g.mu.Unlock()

	now := time.Now()
	g.pruneLocked(now)

	key := degradeSource{uid: uid, sysFP: sysFP}
	if !now.Before(g.until[key]) {
		// 已过期/未触发 → 设新的 until；未过期则保持原 until（不续期）。
		if g.until == nil {
			g.until = map[degradeSource]time.Time{}
		}

		g.until[key] = nextMidnightCST(now)
	}

	g.triggers++
	// 脱敏观测：账号只留 uid8，system 侧只留哈希前缀；两条计数分别是「当前生效
	// 的触发源数」与「进程内累计触发次数」。
	log.Printf("INFO: [server] degrade gate armed acct=%s sysfp=%s until=%s sources=%d triggers=%d",
		logfmt.UID8(uid), sysFPLabel(sysFP), g.until[key].Format(time.RFC3339), len(g.until), g.triggers)
}

// pruneLocked 清掉已过期的触发源条目（调用方持锁）。
func (g *degradeGate) pruneLocked(now time.Time) {
	for key, until := range g.until {
		if !now.Before(until) {
			delete(g.until, key)
		}
	}
}

// sysFPLabel 空指纹（入站无 system 文本）在日志里显示为 "-"，避免空字段难辨识。
func sysFPLabel(sysFP string) string {
	if sysFP == "" {
		return "-"
	}

	return sysFP
}

// nextMidnightCST 返回 now 之后最近的 Asia/Shanghai 00:00 时刻（纯函数，方便单测）。
//
// 边界语义：
//   - 23:59 → 次日 00:00（几秒后）
//   - 00:00 → 次日 00:00（刚过 00:00，下个零点是次日）
//
// 用固定 +08:00 偏移计算，避免依赖系统时区配置（容器/宿主机时区不确定）。
func nextMidnightCST(now time.Time) time.Time {
	cst := time.FixedZone("CST", 8*60*60)
	// 把 now 转到 CST 视角取当天 00:00，再加一天；若该 00:00 <= now 则再加一天。
	y, m, d := now.In(cst).Date()

	midnight := time.Date(y, m, d, 0, 0, 0, 0, cst)
	for !midnight.After(now) {
		midnight = midnight.Add(24 * time.Hour)
	}

	return midnight
}
