package pool

import (
	"math"
	"testing"
	"time"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
)

// TestHealthFactorNeutralWithoutObservation 无观测（含空台账 / model 为空 / 未知模型）因子恒 1。
func TestHealthFactorNeutralWithoutObservation(t *testing.T) {
	e := &entry{}
	now := time.Now()
	if f := e.healthFactor("", now); f != 1 {
		t.Errorf("empty model factor=%v want 1", f)
	}
	if f := e.healthFactor("m1", now); f != 1 {
		t.Errorf("no ledger factor=%v want 1", f)
	}
	e.health = map[string]*modelHealth{"m1": {}}
	if f := e.healthFactor("m2", now); f != 1 {
		t.Errorf("unknown model factor=%v want 1", f)
	}
	if f := e.healthFactor("m1", now); f != 1 {
		t.Errorf("no-observation entry factor=%v want 1", f)
	}
}

// TestHealthFactorSlowTTFTDownweights 慢 TTFT 按比例降权。
func TestHealthFactorSlowTTFTDownweights(t *testing.T) {
	e := &entry{health: map[string]*modelHealth{
		"m1": {ttftMs: 3000, hasTTFT: true, lastSeen: time.Now()},
	}}
	now := time.Now()
	f := e.healthFactor("m1", now)
	want := float64(healthLatencyRefMs) / 3000
	if math.Abs(f-want) > 1e-9 {
		t.Errorf("factor=%v want %v", f, want)
	}
	if f >= 1 {
		t.Errorf("slow TTFT must downweight, factor=%v", f)
	}
}

// TestHealthFactorFloor 极端慢 / 高错误率时因子被钳到下限。
func TestHealthFactorFloor(t *testing.T) {
	e := &entry{health: map[string]*modelHealth{
		"m1": {ttftMs: 1e9, hasTTFT: true, errScore: 1, lastErr: time.Now(), lastSeen: time.Now()},
	}}
	if f := e.healthFactor("m1", time.Now()); f != healthFloor {
		t.Errorf("factor=%v want floor %v", f, healthFloor)
	}
}

// TestRecordModelHealthErrorDecays 错误压力按半衰期衰减：半个半衰期后约减半。
func TestRecordModelHealthErrorDecays(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.RecordModelHealth("u1", "m1", 0, false, false)
	p.mu.RLock()
	mh := p.byUID["u1"].health["m1"]
	p.mu.RUnlock()
	if mh == nil || mh.errScore <= 0 {
		t.Fatalf("error not recorded: %+v", mh)
	}
	base := mh.errScore
	// 把衰减基准回拨半个半衰期：decayedErr 应约为 base/√2。
	mh.lastErr = time.Now().Add(-healthErrHalfLife / 2)
	if got := mh.decayedErr(time.Now()); math.Abs(got-base*math.Sqrt2/2) > 1e-6 {
		t.Errorf("half-life decay got=%v want ~%v", got, base*math.Sqrt2/2)
	}
}

// TestRecordModelHealthIgnoresEmpty 空 uid / model 与未知账号为无操作。
func TestRecordModelHealthIgnoresEmpty(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.RecordModelHealth("", "m1", 100, true, true)
	p.RecordModelHealth("u1", "", 100, true, true)
	p.RecordModelHealth("nope", "m1", 100, true, true)
	if e := p.byUID["u1"]; len(e.health) != 0 {
		t.Errorf("empty/unknown writes must be no-op, got %+v", e.health)
	}
}

// TestWeightOfAppliesHealthFactor 健康因子并入 weightOf：同账号同 credits，慢 TTFT 权重更低。
func TestWeightOfAppliesHealthFactor(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	now := time.Now()
	p.mu.Lock()
	e := p.byUID["u1"]
	wNeutral := p.weightOf(e, 100, "m1", now)
	p.mu.Unlock()

	p.RecordModelHealth("u1", "m1", 6000, true, true) // 慢 TTFT + 成功

	p.mu.Lock()
	wSlow := p.weightOf(e, 100, "m1", now)
	wEmpty := p.weightOf(e, 100, "", now) // 无模型上下文不受影响
	p.mu.Unlock()
	if wSlow >= wNeutral {
		t.Errorf("slow TTFT weight=%v want < neutral %v", wSlow, wNeutral)
	}
	if wEmpty != wNeutral {
		t.Errorf("model-less weight=%v want neutral %v", wEmpty, wNeutral)
	}
}
