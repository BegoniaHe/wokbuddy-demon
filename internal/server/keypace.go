// keypace.go 命名密钥的响应整形（issue #448）：把 apikey 的 GCRA 账本接到写出
// 路径上。准入（Acquire）只回答「能不能进」，整形回答「已放行的流量以什么节奏
// 写出」——rpm/tpm 不再 429，改成把回复拉长：首帧仍立即（TTFT 等于上游自己的
// TTFT），后续帧与结束标记按账本节奏推迟，等价于把下一发请求推迟出去。
package server

import (
	"context"
	"time"

	"github.com/BegoniaHe/wokbuddy-demon/internal/apikey"
	"github.com/BegoniaHe/wokbuddy-demon/internal/pool"
	"github.com/BegoniaHe/wokbuddy-demon/internal/upstream"
)

// keyPacer 实现 upstream.ResponsePacer：流式每帧写出前由 streamWriter 查询一次
// （首帧不查询，见 upstream.streamWriter.paceFrame）。账本推进分两路：
//   - 请求间隔（RPM）：构造时拨一次（PaceBoth），本发请求占一个间隔槽；连发的
//     后续请求依次排队，空闲后第一发零等待；
//   - token 速率（TPM）：prompt 估算构造时入账，之后随已见正文增量拨
//     （NoteEmitted）。
//
// 查询只读两条时间线取更晚者（PaceWait）：等待随墙上时间自然消耗，不需要归还；
// 客户端断开时由 withClientAuth 的 DropPacing 整条删除。
type keyPacer struct {
	keys *apikey.Store
	id   string
	// emitted 已拨进 token 时间线的 completion token 估算（不含 prompt——
	// prompt 估算由 newKeyPacer 一次性拨入）。只有本请求的写出 goroutine 读写，
	// 无需加锁。
	emitted int64
	// seen 返回已见正文的 token 估算（chatStatsReader 增量累计，只含 completion）。
	seen func() int64
}

// newKeyPacer 构造流式整形器并拨出本发请求的请求间隔与 prompt token。
// 返回的初始债务由第一次查询（第二帧）经 PaceWait 读出，首帧不受影响。
func newKeyPacer(ku *keyUsage, promptTokens int64, seen func() int64) *keyPacer {
	ku.keys.PaceBoth(ku.id, time.Now(), promptTokens)

	return &keyPacer{keys: ku.keys, id: ku.id, seen: seen}
}

// WaitBeforeWrite 返回本帧写出前的等待（issue #448）。先把新看到的正文 token
// 拨进 token 时间线，再读两条时间线中更晚的等待；0 = 时间线在过去，不插延迟。
func (p *keyPacer) WaitBeforeWrite() time.Duration {
	now := time.Now()

	if p.seen != nil {
		if delta := p.seen() - p.emitted; delta > 0 {
			p.keys.NoteEmitted(p.id, now, delta)
			p.emitted += delta
		}
	}

	return p.keys.PaceWait(p.id, now)
}

// pacesResponse 报告本请求是否需要响应整形：命名密钥且配了 RPM/TPM。默认密钥与
// 未配限速的命名密钥不进这条路径（R1：与改造前逐字节、同时序一致）。
func (f *chatFlow) pacesResponse() bool {
	return f.keyUsage != nil && f.keyUsage.keys != nil && f.keyUsage.keys.HasRateLimits(f.keyUsage.id)
}

// attachStreamPacer 把整形器挂到响应编码器（issue #448）。未配限速、或编码器
// 不实现 streamPacerSetter（测试替身 / 本地缓冲出口）时无操作。
func (f *chatFlow) attachStreamPacer(stats *chatStatsReader) {
	if !f.pacesResponse() {
		return
	}

	setter, ok := f.out.(streamPacerSetter)
	if !ok {
		return
	}

	setter.setStreamPacer(newKeyPacer(
		f.keyUsage, int64(pool.EstimatePromptTokens(f.body)), stats.paceEmittedTokens,
	))
}

// dropKeyPacing 客户端断开（ctx 取消）时退还一把密钥的整形债务（issue #448 验收
// 7）：正常结束必须保留时间线，后续请求的 RPM/TPM 间隔就靠它跨请求累加；人已走
// 则不该让未兑现的延迟继续占着时间线。
func (h *Handler) dropKeyPacing(ctx context.Context, id string) {
	if ctx.Err() != nil {
		h.cfg.Keys.DropPacing(id)
	}
}

// dropPacingIfClientGone 客户端断开时退还本密钥的整形债务（issue #448 验收 7）：
// 人已走，未兑现的延迟不该继续占着时间线。并发槽由 withClientAuth 的 release 释放。
func (f *chatFlow) dropPacingIfClientGone(sErr error) {
	if f.keyUsage == nil || f.keyUsage.keys == nil || !upstream.IsClientWriteError(sErr) {
		return
	}

	f.keyUsage.keys.DropPacing(f.keyUsage.id)
}

// paceNonStreamWrite 非流式写出前的整形（issue #448 R8）：整笔账一次拨完
// （prompt 估算 + 实测 completion token）后睡到点，再让调用方写响应体。只推迟
// 写出，状态码仍是 200、中途没有任何 429。
func (f *chatFlow) paceNonStreamWrite(ctx context.Context, resp map[string]any) {
	if !f.pacesResponse() {
		return
	}

	wait := f.keyUsage.keys.PaceBoth(f.keyUsage.id, time.Now(), paceTokensFor(f.body, resp))
	if wait <= 0 {
		return
	}
	// 客户端断开（ctx 取消）不等整段拖延：在途槽与连接都不值得为已走的客户端占着。
	timer := time.NewTimer(wait)
	defer timer.Stop()

	select {
	case <-ctx.Done():
	case <-timer.C:
	}
}

// paceTokensFor 本次请求的 token 账：prompt 估算 + 聚合响应里的实测 completion。
// usage 缺失时只算 prompt 估算（不臆造输出量）。
func paceTokensFor(body []byte, resp map[string]any) int64 {
	tokens := int64(pool.EstimatePromptTokens(body))

	if u, ok := resp["usage"].(map[string]any); ok {
		if n, ok := readUsageInt(u, "completion_tokens"); ok {
			tokens += n
		}
	}

	return tokens
}
