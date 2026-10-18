// 选号健康信号（issue #16）：按 (账号, 模型) 记录 TTFT 与错误率的实时 EWMA，
// 在权重上只做降权——不改 healthy / 冷却 / 熔断 / 积分保底等判定语义。
package pool

import (
	"math"
	"time"
)

// 健康信号参数（常量而非配置：当前无运维调参需求，避免开关蔓延）。
const (
	// healthAlpha TTFT EWMA 平滑系数（约 3 次观测收敛，与 NoteModelCost 同风格）。
	healthAlpha = 0.3
	// healthErrHalfLife 错误压力半衰期：历史错误按此衰减，避免永久压权
	// （对齐 pick.go 旧「成功率×3」因子被删的教训：终身累计会永久压权）。
	healthErrHalfLife = 10 * time.Minute
	// healthLatencyRefMs TTFT 基线（ms）：不超过基线不降权，超过则按 基线/实测 比例降权。
	healthLatencyRefMs = 1500
	// healthErrStep 单次错误增加的错误压力（累加后钳 [0,1]）。
	healthErrStep = 0.5
	// healthErrWeight 错误压力对权重的最大降幅系数（err=1 时降为 1-healthErrWeight）。
	healthErrWeight = 0.6
	// healthFloor 健康降权系数下限（避免权重被压到 0 而永不中选）。
	healthFloor = 0.15
	// healthTTL 健康条目过期时长：超时无观测即从台账删除，避免 map 膨胀。
	healthTTL = 30 * time.Minute
)

// modelHealth 单个 (账号, 模型) 的实时健康信号。
type modelHealth struct {
	ttftMs  float64 // TTFT 指数移动平均（ms）
	hasTTFT bool
	// errScore 错误压力 [0,1]；lastErr 是其衰减基准时刻（每次观测先把衰减
	// 物化到当前时刻，保证 errScore 语义为「截至 lastErr 的已衰减值」）。
	errScore float64
	lastErr  time.Time
	lastSeen time.Time
}

// decayedErr 返回截至 now 按半衰期衰减后的错误压力（不修改原值）。
func (mh *modelHealth) decayedErr(now time.Time) float64 {
	if mh.errScore <= 0 || mh.lastErr.IsZero() {
		return 0
	}
	elapsed := now.Sub(mh.lastErr)
	if elapsed <= 0 {
		return mh.errScore
	}
	return mh.errScore * math.Pow(0.5, float64(elapsed)/float64(healthErrHalfLife))
}

// observeError 先把错误压力衰减物化到 now，再按本次结果更新（失败累加，成功仅衰减）。
func (mh *modelHealth) observeError(now time.Time, ok bool) {
	mh.errScore = mh.decayedErr(now)
	mh.lastErr = now
	if ok {
		return
	}
	mh.errScore += healthErrStep
	if mh.errScore > 1 {
		mh.errScore = 1
	}
}

// observeTTFT 并入一次 TTFT 观测（首观测直接落值，其后 EWMA 平滑）。
func (mh *modelHealth) observeTTFT(ttftMs int64) {
	if !mh.hasTTFT {
		mh.ttftMs = float64(ttftMs)
		mh.hasTTFT = true
		return
	}
	mh.ttftMs = mh.ttftMs*(1-healthAlpha) + float64(ttftMs)*healthAlpha
}

// RecordModelHealth 记录一次 (账号, 模型) 的实时健康观测：ok=false 增加错误压力，
// ok=true 仅推进衰减；hasTTFT 时并入 TTFT EWMA。uid/model 为空或账号不存在时忽略。
// 这是健康台账的唯一写入口（server 的 recordAttempt 调用）。
func (p *Pool) RecordModelHealth(uid, model string, ttftMs int64, hasTTFT, ok bool) {
	if uid == "" || model == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	e, found := p.byUID[uid]
	if !found {
		return
	}
	if e.health == nil {
		e.health = make(map[string]*modelHealth)
	}
	now := p.now()
	mh := e.health[model]
	if mh == nil {
		mh = &modelHealth{}
		e.health[model] = mh
	}
	mh.observeError(now, ok)
	if hasTTFT && ttftMs > 0 {
		mh.observeTTFT(ttftMs)
	}
	mh.lastSeen = now
}

// healthFactor 返回该账号在 model 上的健康降权系数 ∈ [healthFloor, 1]。
// 无观测（含 model 为空 / 无台账）→ 1（中性，不惩罚新号）。仅降权，不改判定。
func (e *entry) healthFactor(model string, now time.Time) float64 {
	if model == "" || len(e.health) == 0 {
		return 1
	}
	mh := e.health[model]
	if mh == nil {
		return 1
	}
	f := 1.0
	if mh.hasTTFT && mh.ttftMs > healthLatencyRefMs {
		f *= float64(healthLatencyRefMs) / mh.ttftMs
	}
	if errScore := mh.decayedErr(now); errScore > 0 {
		f *= 1 - healthErrWeight*errScore
	}
	if f < healthFloor {
		f = healthFloor
	}
	if f > 1 {
		f = 1
	}
	return f
}

// pruneExpiredHealth 删除超过 healthTTL 无观测的健康条目（惰性清理，防 map 膨胀）。
// 调用方需已持 p.mu（pick 路径写锁）。
func (e *entry) pruneExpiredHealth(now time.Time) {
	if len(e.health) == 0 {
		return
	}
	for model, mh := range e.health {
		if now.Sub(mh.lastSeen) > healthTTL {
			delete(e.health, model)
		}
	}
}

// TTFTByModel 返回各模型当前 TTFT EWMA 的账号均值（ms）。只统计 healthTTL 内有
// 观测且 hasTTFT 的 (账号, 模型) 条目；无观测的模型不输出（不编造 0）。
// 供 /metrics 导出，读路径不写台账。
func (p *Pool) TTFTByModel() map[string]float64 {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := p.now()
	sum := map[string]float64{}
	count := map[string]int{}
	for _, e := range p.byUID {
		for model, mh := range e.health {
			if !mh.hasTTFT || mh.ttftMs <= 0 || now.Sub(mh.lastSeen) > healthTTL {
				continue
			}
			sum[model] += mh.ttftMs
			count[model]++
		}
	}
	out := make(map[string]float64, len(sum))
	for model, s := range sum {
		out[model] = s / float64(count[model])
	}
	return out
}
