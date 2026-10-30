// scheduling.go 并发感知调度（issue #116）：粘性软逃逸、并发压力权重、请求级预扣。
// 这里只放纯计算与预扣原语；选号主流程仍在 pick.go / state.go。
package pool

import (
	"math"
	"math/rand/v2"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	// stickyEscapeRatio 粘性命中的在途软逃逸比例：达到 ceil(ratio×limit) 即解绑，
	// 让同会话的后续子请求提前溢出，而不是等 inFlightFull 才换号。
	stickyEscapeRatio = 0.66
	// stickyEscapeHealth 粘性命中的健康因子下限：低于该值即软逃逸（TTFT/错误压力）。
	stickyEscapeHealth = 0.5
	// StickyDegradeWindow 软逃逸后的会话降级窗口：窗口内不再绑定回原账号。
	StickyDegradeWindow = 20 * time.Second
	// stickyReuseLoadRatio 降级窗口结束后允许回纳的低水位：inFlight <= floor(ratio×limit)。
	stickyReuseLoadRatio = 0.3

	// pressureDeadband 并发压力死区：低于该负载率不降权。
	pressureDeadband = 0.4
	// pressureAlpha 压力降权强度：1/(1+alpha×pressure)，pressure∈[0,1]。
	pressureAlpha = 7.0

	// probeWindow 全冷却兜底单飞窗口：一次兜底选中后，窗口内不再接受第二个并发请求。
	probeWindow = 30 * time.Second

	// reserveDefaultCostPer1K 无本地实测台账、仅目录倍率为收费时的保守预扣单价
	// （credits / 1k token）。宁高勿低：预扣只影响请求前拦截，实际扣费仍按 usage。
	reserveDefaultCostPer1K = 1.0
	// reserveOutputTokens 请求未声明 max_tokens 时的保守输出 token 估算。
	reserveOutputTokens = 2048
)

// stickyEscapedLocked 报告粘性命中是否应软逃逸。调用方需已持 p.mu。
func (p *Pool) stickyEscapedLocked(entry2 *entry, model string, now time.Time) bool {
	if entry2.healthFactor(model, now) < stickyEscapeHealth {
		return true
	}

	limit := p.inFlightLimit(entry2)
	if limit <= 0 {
		return false
	}

	threshold := int(math.Ceil(stickyEscapeRatio * float64(limit)))

	return int(entry2.inFlight.Load()) >= threshold
}

// StickyLowLoad 报告账号是否回落到可重新接收粘性绑定的低水位。
// 供 session 降级窗口结束后的回纳判定；realm 非空时校验域。
func (p *Pool) StickyLowLoad(uid, model, realm string) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()

	entry2, ok := p.byUID[uid]
	if !ok {
		return false
	}

	if realm != "" && entry2.a.Realm() != realm {
		return false
	}

	limit := p.inFlightLimit(entry2)
	if limit <= 0 {
		return true
	}

	return int(entry2.inFlight.Load()) <= int(math.Floor(stickyReuseLoadRatio*float64(limit)))
}

// pressureFactor 并发压力对权重的乘性折扣：limit<=0 或低于死区时 1.0。
// 与 healthFactor 一样只降权，不改可用性判定。
func (p *Pool) pressureFactor(entry3 *entry) float64 {
	limit := p.inFlightLimit(entry3)
	if limit <= 0 {
		return 1
	}

	r := float64(entry3.inFlight.Load()) / float64(limit)
	if r <= pressureDeadband {
		return 1
	}

	x := (r - pressureDeadband) / (1 - pressureDeadband)

	return 1 / (1 + pressureAlpha*x*x)
}

// jitterCooldown 给固定冷却时长叠加 ±10% 有界抖动，打散多账号同相位解封。
// 使用进程级随机源（非测试注入源），不改变冷却语义，只改变蜂拥的到达时刻。
func jitterCooldown(duration time.Duration) time.Duration {
	if duration <= 0 {
		return duration
	}

	delta := int64(float64(duration) * 0.1)
	if delta <= 0 {
		return duration
	}

	return duration - time.Duration(delta) + time.Duration(rand.Int64N(2*delta+1)) //nolint:gosec // 调度抖动，非安全用途
}

// cjkRangeStart CJK 统一表意文字的起始 rune。
const cjkRangeStart = 0x2E80

// EstimatePromptTokens 粗略估算请求的 prompt token：ASCII ≈ 4 字节/token，
// CJK ≈ 1.5 字节/token。只用于请求前预扣，不参与计费或 usage 统计。
//
// 直接在字节切片上解码：`for _, r := range string(body)` 会把整份请求体复制一份
// （几十 KB 的体每次预扣白分配一次），逐 rune 解码零分配且更快。非法字节的
// 处理与 range string 一致（RuneError + 宽度 1）。
func EstimatePromptTokens(body []byte) int {
	ascii, cjk := 0, 0

	for len(body) > 0 {
		r, size := utf8.DecodeRune(body)
		body = body[size:]

		if r >= cjkRangeStart {
			cjk++
		} else {
			ascii++
		}
	}

	return ascii/4 + int(float64(cjk)/1.5)
}

// ReserveForRequest 请求级预扣（issue #116 P1-1）：按实测单价或目录倍率估算本次
// 请求成本，在 credits 中预留。成功返回预留金额；余额不足以覆盖保底时返回
// (0,false)，调用方应跳过该账号。
//
// 免费/未知模型不预扣：与 floorBlockedForRealmModel 的保守口径一致——未知倍率
// 无法判收费，宁可放行由实扣与硬冷却兜底。
func (p *Pool) ReserveForRequest(uid, model, realm string, promptTokens, outputTokens int) (float64, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()

	entry2, ok := p.byUID[uid]
	if !ok {
		return 0, false
	}

	if p.creditFloor <= 0 || model == "" {
		return 0, true
	}

	if outputTokens <= 0 {
		outputTokens = reserveOutputTokens
	}

	price, paid := p.reserveUnitCostLocked(entry2, model, realm, p.now())
	if !paid || price <= 0 {
		return 0, true
	}

	amount := price * float64(promptTokens+outputTokens) / 1000
	if amount <= 0 {
		return 0, true
	}

	available := float64(entry2.credits) - entry2.reservedCredits
	if available-amount < float64(p.creditFloor) {
		return 0, false
	}

	entry2.reservedCredits += amount

	return amount, true
}

// reserveUnitCostLocked 返回预扣单价与是否收费：本地实测台账优先，目录倍率
// 仅用于判收费（金额用保守默认单价）。
func (p *Pool) reserveUnitCostLocked(e *entry, model, realm string, now time.Time) (float64, bool) {
	if mc, ok := e.modelCostOf(model, now); ok {
		return mc.CostPer1k, mc.CostPer1k > 0
	}

	if p.modelRateOf == nil {
		return 0, false
	}

	rate := p.modelRateOf(realm, model)
	if strings.TrimSpace(rate) == "" {
		return 0, false
	}

	v, err := strconv.ParseFloat(rate, 64)
	if err != nil {
		return 0, false
	}

	if v <= 0 {
		return 0, false
	}

	return reserveDefaultCostPer1K, true
}

// ReleaseReservation 释放请求级预扣；幂等且钳到 0。
func (p *Pool) ReleaseReservation(uid string, amount float64) {
	if amount <= 0 {
		return
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	if e, ok := p.byUID[uid]; ok {
		e.reservedCredits -= amount
		if e.reservedCredits < 0 {
			e.reservedCredits = 0
		}
	}
}
