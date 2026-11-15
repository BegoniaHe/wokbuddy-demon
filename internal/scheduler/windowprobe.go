// windowprobe.go 额度窗口探针执行（issue #492）。
//
// pool 只负责「哪个 (账号, 模型) 的上游重置墙钟到点了、还能试几次」；真正把新窗口
// 起点钉住的请求在这里发：一条 max_tokens=1 的最小 chat，成功即出账。失败只记
// 日志、不罚号（不 NoteFailures / 不冷却 / 不喂熔断）——探针失败说明窗口没钉住，
// 与账号健康无关。
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
	"github.com/BegoniaHe/wokbuddy-demon/internal/jsonx"
	"github.com/BegoniaHe/wokbuddy-demon/internal/logfmt"
	"github.com/BegoniaHe/wokbuddy-demon/internal/pool"
	"github.com/BegoniaHe/wokbuddy-demon/internal/upstream"
)

const (
	// windowProbeMaxPerRound 单轮最多执行多少个探针：多个窗口同时到点时也不集中打
	// 上游，剩余窗口留到下一轮（台账里的重试间隔同样生效）。
	windowProbeMaxPerRound = 8
	// windowProbeTimeout 单个探针的时限。探针是最小请求，不该等满模型默认超时。
	windowProbeTimeout = 60 * time.Second
)

// errWindowProbeRejected 探针被上游拒绝（err113：错误串里带脱敏后的上游原文）。
var errWindowProbeRejected = errors.New("window probe rejected")

// StartWindowProbe 后台额度窗口探针循环（独立 ticker，ctx 取消即停）。
// interval<=0 以暂停状态启动并等待 SetWindowProbeInterval 唤醒：启动时关闭
// schedule.window_probe_enabled、运行期再热开也能生效（无需重启）。
//
//nolint:dupl // 与其余周期循环同形：各自的 atomic 间隔与 rearm channel 不可合并。
func (s *Scheduler) StartWindowProbe(ctx context.Context, interval time.Duration) {
	if interval < 0 {
		interval = 0
	}

	s.windowProbeInterval.Store(int64(interval))

	runIntervalLoop(ctx, &s.bgWG, &s.windowProbeInterval, s.rearmWindowProbe, "额度窗口探针", nil, s.runWindowProbeOnce)
}

// SetWindowProbeInterval 热改额度窗口探针间隔；<=0 表示暂停循环。
func (s *Scheduler) SetWindowProbeInterval(interval time.Duration) {
	if interval < 0 {
		interval = 0
	}

	s.windowProbeInterval.Store(int64(interval))
	poke(s.rearmWindowProbe)
}

// runWindowProbeOnce 执行一轮到点窗口探测。
func (s *Scheduler) runWindowProbeOnce(ctx context.Context) {
	if s.cfg.Pool == nil || s.cfg.Upstream == nil {
		return
	}

	probes := s.cfg.Pool.DueWindowProbes(windowProbeMaxPerRound)

	for _, probe := range probes {
		if ctx.Err() != nil {
			return
		}

		s.probeWindow(ctx, probe)
	}
}

// probeWindow 对单个窗口发一条最小请求并交回结果。失败不罚号，只写日志。
func (s *Scheduler) probeWindow(ctx context.Context, probe pool.WindowProbe) {
	account := s.cfg.Pool.AuthByUID(probe.UID)
	if account == nil {
		// 账号已删除：台账一并出清，避免陈旧条目反复触发。
		s.cfg.Pool.WindowProbeDone(probe.UID, probe.Model, pool.WindowProbeGiveUp)

		return
	}

	// 与网关流量共用单号在途名额（同 blackcatOne）：拿不到名额时本轮不探、也不
	// 标记台账（否则会把「没探成功」记成「已钉住新窗口」）。
	if !s.cfg.Pool.Acquire(account.UID) {
		// DueWindowProbes 已把本次尝试记入台账，但请求压根没发：归还这次尝试，
		// 否则「池子忙」会白烧掉 attempts 上限（默认 2 次）让窗口再也钉不住。
		s.cfg.Pool.ReturnWindowProbe(probe.UID, probe.Model)
		s.logger.Printf("额度窗口探针跳过 acct=%s model=%s：在途名额已满",
			logfmt.Label(account.UID, account.NicknameValue()), probe.Model)

		return
	}

	// 租约用 defer 归还：sendWindowProbe 里跑的是上游响应解析，panic 时本循环
	// （runIntervalLoop）没有针对租约的兜底，显式 Release 不会执行——该账号的在途
	// 名额会永久 -1，泄漏 windowProbeMaxPerRound 轮后彻底不可用，只能重启进程。
	result, err := func() (pool.WindowProbeResult, error) {
		defer s.cfg.Pool.Release(account.UID)

		return s.sendWindowProbe(ctx, account, probe.Model)
	}()
	s.cfg.Pool.WindowProbeDone(probe.UID, probe.Model, result)

	label := logfmt.Label(account.UID, account.NicknameValue())
	if err != nil {
		// 错误原文可能带上游 body / URL：与请求路径同口径脱敏后再落日志。
		s.logger.Printf("额度窗口探针失败 acct=%s model=%s（%s）: %s",
			label, probe.Model, windowProbeRetryHint(result),
			upstream.RedactForLog(err.Error(), 160))

		return
	}

	s.logger.Printf("额度窗口探针成功 acct=%s model=%s：新窗口起点已钉住（重置时刻 %s）",
		label, probe.Model, probe.ResetAt.Format(time.RFC3339))
}

// windowProbeRetryHint 失败后的处置文案（限流才按退避重试）。
func windowProbeRetryHint(result pool.WindowProbeResult) string {
	if result == pool.WindowProbeRetry {
		return "限流，不罚号，" + windowProbeRetryBackoffLog + " 后重试"
	}

	return "非限流错误，不罚号，出账不再重试"
}

// windowProbeRetryBackoffLog 与 pool 侧的重试间隔同口径（日志可读文案）。
const windowProbeRetryBackoffLog = "10m"

// sendWindowProbe 发一条 max_tokens=1 的最小 chat，并读到首个有效帧为止。
//
// 只按 HTTP 状态判成功会把 200 + 首帧 error（限流文书 / 额度耗尽）当成「打通」：
// 上游还没真正重置窗口，下一次真实请求继续撞墙。这里用 upstream.ProbeStream 预览
// 首帧，error 帧按失败处理；限流类失败保留台账按退避重试，其余失败快速出账。
func (s *Scheduler) sendWindowProbe(
	ctx context.Context, account *auth.Auth, model string,
) (pool.WindowProbeResult, error) {
	body, err := jsonx.Marshal(map[string]any{
		"model":      model,
		"stream":     true,
		"max_tokens": 1,
		"messages":   []any{map[string]any{"role": "user", "content": "hi"}},
	})
	if err != nil {
		return pool.WindowProbeGiveUp, fmt.Errorf("构造探针请求失败: %w", err)
	}

	probeCtx, cancel := context.WithTimeout(ctx, windowProbeTimeout)
	defer cancel()

	rc, status, respBody, err := s.cfg.Upstream.ChatStreamContext(
		probeCtx, account, body, "", upstream.ChatMeta{},
	)
	if rc != nil {
		defer rc.Close()
	}

	if err != nil {
		return pool.WindowProbeGiveUp, fmt.Errorf("探针请求失败: %w", err)
	}

	if status >= http.StatusBadRequest {
		kind := upstream.Classify(status, string(respBody))

		return windowProbeResultFor(kind), fmt.Errorf("%w: status=%d kind=%s body=%s",
			errWindowProbeRejected, status, kind, upstream.RedactForLog(string(respBody), 160))
	}

	if rc == nil {
		return pool.WindowProbeGiveUp, fmt.Errorf("%w: 上游未返回流", errWindowProbeRejected)
	}

	streamProbe := upstream.ProbeStream(rc)
	defer streamProbe.Release()

	return windowProbeFrameResult(streamProbe)
}

// windowProbeFrameResult 把首帧预览结果映射成台账结论：error 帧按失败处理（限流类
// 保留重试），首个有效帧即视为窗口已钉住。
func windowProbeFrameResult(streamProbe *upstream.StreamProbe) (pool.WindowProbeResult, error) {
	switch {
	case streamProbe.Kind != upstream.ErrNone:
		return windowProbeResultFor(streamProbe.Kind), fmt.Errorf("%w: 首帧错误帧 kind=%s frame=%s",
			errWindowProbeRejected, streamProbe.Kind, upstream.RedactForLog(streamProbe.Frame, 160))
	case streamProbe.SizeLimited:
		return pool.WindowProbeGiveUp, fmt.Errorf("%w: 首帧前单行超长", errWindowProbeRejected)
	case streamProbe.Empty && !streamProbe.SawReasoning:
		return pool.WindowProbeGiveUp, fmt.Errorf("%w: 流在首个有效帧前结束", errWindowProbeRejected)
	default:
		return pool.WindowProbePinned, nil
	}
}

// windowProbeResultFor 限流类失败按退避重试；其余（5xx / 余额 / 4xx）快速出账。
func windowProbeResultFor(kind upstream.ErrKind) pool.WindowProbeResult {
	if kind == upstream.ErrSoftRate {
		return pool.WindowProbeRetry
	}

	return pool.WindowProbeGiveUp
}
