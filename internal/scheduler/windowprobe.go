// windowprobe.go 额度窗口探针执行（issue #492）。
//
// pool 只负责「哪个 (账号, 模型) 的上游重置墙钟到点了、还能试几次」；真正把新窗口
// 起点钉住的请求在这里发：一条 max_tokens=1 的最小 chat，成功即出账。失败只记
// 日志、不罚号（不 NoteFailures / 不冷却 / 不喂熔断）——探针失败说明窗口没钉住，
// 与账号健康无关。
package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
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
		s.cfg.Pool.WindowProbeDone(probe.UID, probe.Model, true)

		return
	}

	// 与网关流量共用单号在途名额（同 blackcatOne）：拿不到名额时本轮不探、也不
	// 标记台账（否则会把「没探成功」记成「已钉住新窗口」）。
	if !s.cfg.Pool.Acquire(account.UID) {
		s.logger.Printf("额度窗口探针跳过 acct=%s model=%s：在途名额已满",
			logfmt.Label(account.UID, account.NicknameValue()), probe.Model)

		return
	}

	// 租约用 defer 归还：sendWindowProbe 里跑的是上游响应解析，panic 时本循环
	// （runIntervalLoop）没有针对租约的兜底，显式 Release 不会执行——该账号的在途
	// 名额会永久 -1，泄漏 windowProbeMaxPerRound 轮后彻底不可用，只能重启进程。
	err := func() error {
		defer s.cfg.Pool.Release(account.UID)

		return s.sendWindowProbe(ctx, account, probe.Model)
	}()
	s.cfg.Pool.WindowProbeDone(probe.UID, probe.Model, err == nil)

	label := logfmt.Label(account.UID, account.NicknameValue())
	if err != nil {
		// 错误原文可能带上游 body / URL：与请求路径同口径脱敏后再落日志。
		s.logger.Printf("额度窗口探针失败 acct=%s model=%s（不罚号，%s 后重试）: %s",
			label, probe.Model, windowProbeRetryBackoffLog,
			upstream.RedactForLog(err.Error(), 160))

		return
	}

	s.logger.Printf("额度窗口探针成功 acct=%s model=%s：新窗口起点已钉住（重置时刻 %s）",
		label, probe.Model, probe.ResetAt.Format(time.RFC3339))
}

// windowProbeRetryBackoffLog 与 pool 侧的重试间隔同口径（日志可读文案）。
const windowProbeRetryBackoffLog = "10m"

// sendWindowProbe 发一条 max_tokens=1 的最小 chat。只判 HTTP 状态：拿到 2xx 即认为
// 请求已被上游承认为新窗口的第一条，不必读完整个流。
func (s *Scheduler) sendWindowProbe(
	ctx context.Context, account *auth.Auth, model string,
) error {
	body, err := json.Marshal(map[string]any{
		"model":      model,
		"stream":     true,
		"max_tokens": 1,
		"messages":   []any{map[string]any{"role": "user", "content": "hi"}},
	})
	if err != nil {
		return fmt.Errorf("构造探针请求失败: %w", err)
	}

	probeCtx, cancel := context.WithTimeout(ctx, windowProbeTimeout)
	defer cancel()

	rc, status, respBody, err := s.cfg.Upstream.ChatStreamContext(
		probeCtx, account, body, "", upstream.ChatMeta{},
	)
	if rc != nil {
		// 读一小段再关：部分实现要等首帧才真正把请求算作已受理。
		_, _ = io.Copy(io.Discard, io.LimitReader(rc, 4<<10))
		rc.Close()
	}

	if err != nil {
		return fmt.Errorf("探针请求失败: %w", err)
	}

	if status >= http.StatusBadRequest {
		return fmt.Errorf("%w: status=%d body=%s", errWindowProbeRejected, status,
			upstream.RedactForLog(string(respBody), 160))
	}

	return nil
}
