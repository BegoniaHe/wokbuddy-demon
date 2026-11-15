// pacing.go 限速密钥的响应整形账本（issue #448）。
//
// 语义分工：
//   - 准入（Acquire）只回答「现在能不能进」：过期 / IP / 配额 / 并发。
//   - 整形（本文件）回答「已放行的流量该以什么节奏写出」：RPM 请求间隔与
//     TPM token 速率，两条 GCRA 虚拟调度（virtual scheduling，与漏桶等价，
//     见 ITU-T I.371）。
//
// 为什么不再用固定窗口拒绝 RPM/TPM：窗口内打满即 429，串行客户端的重试与
// 窗口清零后的突发把上游账号的请求频率抬到风控会判滥用的水平。把已放行的
// 回复拉长（首帧仍立即放行），等价于把下一发请求推迟出去——降的是打到账号
// 上的请求频率，不是生成速度。
//
// 进程内、单实例：与既有 limitRuntime 同语义，不加依赖、不跨进程同步。
package apikey

import (
	"sync"
	"time"
)

// PaceMaxDelay 单条流（或单次非流式写）的累计整形等待上限，也是 TAT 距当前
// 时刻的上限。到顶后调用方把剩余内容一次放完，而不是无限等待——等待本身占着
// 连接、在途槽与客户端超时，拖太久会让客户端超时重试，频率反向升高。
const PaceMaxDelay = 60 * time.Second

// paceState 单把密钥的两条 GCRA 时间线（Theoretical Arrival Time，理论到达时刻）：
//   - reqTAT：请求间隔时间线。每放行一发 +60/rpm 秒；
//   - tokTAT：token 时间线。每放行一个 token +60/tpm 秒。
//
// TAT 的含义：「按当前速率，最早允许多少字节/帧到达」的时刻。等待时长 =
// TAT - now（过去 = 零等待，即空闲时时间线在过去，不插入任何延迟）。
type paceState struct {
	reqTAT time.Time
	tokTAT time.Time
}

// paceLedger 整形账本：每把密钥至多两条 TAT。与 limitRuntime 共用 Store.limMu
// （同一把锁串行化「读 TAT → 拨 TAT → 返回等待」，并发请求不会读到同一时刻
// 后一起放行）。
type paceLedger struct {
	mu     sync.Mutex
	states map[string]*paceState
}

// newPaceLedger 初始化空账本。
func newPaceLedger() *paceLedger {
	return &paceLedger{states: map[string]*paceState{}}
}

// stateLocked 取/初始化密钥的整形状态（持锁调用）。
func (l *paceLedger) stateLocked(id string) *paceState {
	state := l.states[id]
	if state == nil {
		state = &paceState{}
		l.states[id] = state
	}

	return state
}

// drop 删除一把密钥的整形时间线（客户端断开 / 密钥删除时调用）：挂起的债务
// 不继续占着时间线，后续请求回到零等待基线。
func (l *paceLedger) drop(id string) {
	l.mu.Lock()
	defer l.mu.Unlock()

	delete(l.states, id)
}

// advanceReq 请求间隔调度：TAT = max(now, TAT+60/rpm)，返回距该 TAT 的等待。
// τ（突发容忍）= 0：空闲后时间线在过去，第一发零等待；不在分钟开头留额度，
// 避免一窗打光后的长时间静默。
func (l *paceLedger) advanceReq(id string, now time.Time, rpm int) time.Duration {
	interval := time.Minute / time.Duration(rpm)

	l.mu.Lock()
	defer l.mu.Unlock()

	state := l.stateLocked(id)

	tat := state.reqTAT.Add(interval)
	if now.After(tat) {
		tat = now
	}

	// 每发只允许把自己的间隔槽放进当前 60s 窗口；窗口外的存量债务不能靠
	// 再次调用续期，否则高频请求会把 TAT 永久推到未来。
	state.reqTAT = minTime(tat, now.Add(PaceMaxDelay))

	return max(state.reqTAT.Sub(now), 0)
}

// advanceTokens token 速率调度：TAT = max(now, TAT)+tokens*60/tpm，结果夹到
// now+PaceMaxDelay。只按已吐出的 completion 计，不把 prompt 或 max_tokens 预算
// 扣进来：TPS 衡量 Decode 阶段新增输出，输入上下文不应占用输出速率的时间线。
//
// 基准先归到 now 再累加（与 NoteEmitted 同口径）：tokTAT 的零值在 time.Time 的
// 公元 1 年，直接 `零值 + 间隔` 仍远在过去，会被下面的 `now.After(tat)` 判成
// 「时间线已过去」而整段丢弃——非流式路径（PaceBoth）的首发 TPM 债务就此白送，
// 之后才从 now 起算。RPM 的「空闲后第一发零等待」不能套到 token 维度：TPM 限的
// 是 token 数，每一个都必须占时间线。
func (l *paceLedger) advanceTokens(id string, now time.Time, tpm int, tokens int64) time.Duration {
	if tokens <= 0 {
		return 0
	}

	per := time.Minute / time.Duration(tpm)

	l.mu.Lock()
	defer l.mu.Unlock()

	state := l.stateLocked(id)

	base := state.tokTAT
	if now.After(base) {
		base = now
	}

	tat := base.Add(paceSpan(tokens, per))
	if tat.Before(now) {
		tat = now
	}

	state.tokTAT = minTime(tat, now.Add(PaceMaxDelay))

	return max(state.tokTAT.Sub(now), 0)
}

// waitTAT 只读某条时间线（不拨）：供结束标记判断「还要等多久」。取哪条由
// 调用方的字段选择器给出。
func (l *paceLedger) waitTAT(id string, now time.Time, tat func(*paceState) time.Time) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()

	st := l.states[id]
	if st == nil {
		return 0
	}

	return max(tat(st).Sub(now), 0)
}

// waitReq 只读请求间隔时间线（不拨）。
func (l *paceLedger) waitReq(id string, now time.Time) time.Duration {
	return l.waitTAT(id, now, func(st *paceState) time.Time { return st.reqTAT })
}

// waitTokens 只读 token 时间线（不拨）。
func (l *paceLedger) waitTokens(id string, now time.Time) time.Duration {
	return l.waitTAT(id, now, func(st *paceState) time.Time { return st.tokTAT })
}

// PaceRequest 请求间隔整形（RPM）：返回调用方在写出本发请求前应等待的时长。
// 内部把 TAT 先拨到未来再返回等待，故并发请求各得一个不重叠的区间。
// rpm <= 0 = 不限，恒返回 0。
func (s *Store) PaceRequest(id string, now time.Time) time.Duration {
	if s == nil || id == "" {
		return 0
	}

	limits, _, found := s.stateOf(id)
	if !found || limits.RPM <= 0 {
		return 0
	}

	return min(s.pace.advanceReq(id, now, limits.RPM), PaceMaxDelay)
}

// PaceTokens token 速率整形（TPM）：按 tokens 个已见 token 推进 token 时间线，
// 返回应等待的时长。tpm <= 0 或 tokens <= 0 = 零操作。
func (s *Store) PaceTokens(id string, now time.Time, tokens int64) time.Duration {
	if s == nil || id == "" {
		return 0
	}

	limits, _, found := s.stateOf(id)
	if !found || limits.TPM <= 0 {
		return 0
	}

	return min(s.pace.advanceTokens(id, now, limits.TPM, tokens), PaceMaxDelay)
}

// PaceBoth 同时推进两条时间线并取更晚者：RPM 与 TPM 都配置时，结束时刻取
// 「请求间隔」与「token 还没放完」里更晚的那个（issue #448）。
func (s *Store) PaceBoth(id string, now time.Time, tokens int64) time.Duration {
	if s == nil || id == "" {
		return 0
	}

	limits, _, found := s.stateOf(id)
	if !found {
		return 0
	}

	var reqWait, tokWait time.Duration

	if limits.RPM > 0 {
		reqWait = s.pace.advanceReq(id, now, limits.RPM)
	}

	if limits.TPM > 0 && tokens > 0 {
		tokWait = s.pace.advanceTokens(id, now, limits.TPM, tokens)
	}

	return min(max(reqWait, tokWait), PaceMaxDelay)
}

// paceSpan tokens 个 token 在 per 间隔下摊开的等待，两个方向都夹到 ±PaceMaxDelay
// 以内；按 per<=0 或 tokens==0 给零。
//
// 为什么必须在乘法之前夹：time.Duration 是 int64 纳秒，tokens × per 会溢出成
// 负值，随后「TAT 在过去 → 归到 now」的分支把本该最长 PaceMaxDelay 的债务抹成
// 零等待——实测 tpm=1、tokens=1<<62 时等待为 0（限速整条时间线被白送）。tokens
// 来自上游 usage.completion_tokens 或本地正文估算，不得让一次异常响应关掉限速。
// 负方向同理：NoteEmitted 用负差额退回高估的债务，溢出后会变成「凭空增加」。
func paceSpan(tokens int64, per time.Duration) time.Duration {
	if per <= 0 || tokens == 0 {
		return 0
	}

	limit := int64(PaceMaxDelay / per)
	if tokens > limit {
		return PaceMaxDelay
	}

	if tokens < -limit {
		return -PaceMaxDelay
	}

	return time.Duration(tokens) * per
}

// minTime 返回较早的时刻。TAT 一律夹到 now+PaceMaxDelay：单次查询与长期账本
// 都受同一上限约束，避免一次突发把后续请求永久推到未来。
func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}

	return b
}

// NoteEmitted 调整本请求已吐出的 completion token（TPM 时间线的输入）。
// tokens 可为负：流中先按正文估算入账，末帧拿到精确 usage 后用差额把估算
// 对账掉。与 RecordUsage 分离：后者是请求结束后的持久计数，前者是流中增量，
// 必须在字节写出前调用才能把「已见 token」算进时间线。
//
// 基线是「现在」而不是零值 time：时间线的含义是「按当前速率，最早允许多少
// token 到达」，已过去的 TAT（含从未初始化的零值）一律归到 now，空闲时
// 时间线在过去，第一发零等待。退回量超过当前债务时夹在当前时刻，不能变成
// 负时间线而白送后续突发额度。
func (s *Store) NoteEmitted(id string, now time.Time, tokens int64) {
	if s == nil || id == "" || tokens == 0 {
		return
	}

	interval := paceTokenIntervalFor(s, id)
	if interval <= 0 {
		return
	}

	s.pace.mu.Lock()
	defer s.pace.mu.Unlock()

	state := s.pace.stateLocked(id)

	base := state.tokTAT
	if now.After(base) {
		base = now
	}

	tat := base.Add(paceSpan(tokens, interval))
	if tat.Before(now) {
		tat = now
	}

	state.tokTAT = minTime(tat, now.Add(PaceMaxDelay))
}

// paceTokenIntervalFor 读取单 token 的间隔（60/tpm 秒）；未配置 TPM 返回 0。
func paceTokenIntervalFor(s *Store, id string) time.Duration {
	limits, _, found := s.stateOf(id)
	if !found || limits.TPM <= 0 {
		return 0
	}

	return time.Minute / time.Duration(limits.TPM)
}

// PaceWait 只读两条时间线中更晚的等待（不拨）：供写出侧的逐帧查询。同样受
// PaceMaxDelay 上限约束——到顶后写出侧把剩余内容一次放完，不再无限等待。
func (s *Store) PaceWait(id string, now time.Time) time.Duration {
	if s == nil || id == "" {
		return 0
	}

	limits, _, found := s.stateOf(id)
	if !found {
		return 0
	}

	var reqWait, tokWait time.Duration

	if limits.RPM > 0 {
		reqWait = s.pace.waitReq(id, now)
	}

	if limits.TPM > 0 {
		tokWait = s.pace.waitTokens(id, now)
	}

	return min(max(reqWait, tokWait), PaceMaxDelay)
}

// HasRateLimits 报告密钥是否配置了 RPM 或 TPM：未配置时写出路径完全不进整形
// 账本（issue #448 R1——无速率限制的请求与改造前逐字节、同时序一致）。
func (s *Store) HasRateLimits(id string) bool {
	if s == nil || id == "" {
		return false
	}

	limits, _, found := s.stateOf(id)

	return found && (limits.RPM > 0 || limits.TPM > 0)
}

// DropPacing 删除一把密钥的整形时间线：客户端断开时退还未发出的延迟，
// 密钥删除时清理进程内状态。
func (s *Store) DropPacing(id string) {
	if s == nil || id == "" {
		return
	}

	s.pace.drop(id)
}
