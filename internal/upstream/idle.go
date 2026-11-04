// idle.go 聊天 SSE 流中空闲监控：活跃吐数据续命不掐，静默超过阈值才断流（释放租约）。
package upstream

import (
	"context"
	"fmt"
	"io"
	"sync"
	"time"
)

// idleMonitoringBody 包在聊天 SSE body 外层：
// 每次读到底层数据（n>0）就刷新 lastRead；后台 goroutine 周期检查，
// 静默超过 idle 就 cancel 请求 context，中断阻塞中的 Read。
type idleMonitoringBody struct {
	rc       io.ReadCloser
	mu       sync.Mutex
	lastRead time.Time
	// readMu 串行化对底流的 Read：http 响应体不支持并发读，Close 的读尾与
	// keepalive 收尾时仍停在底流上的读 goroutine 必须互斥。
	readMu   sync.Mutex
	stopOnce sync.Once
	stopCh   chan struct{}
	cancel   context.CancelFunc
	// closeOnce/closeErr 让 Close 幂等：透传链上多层收尾可能重复关同一个 body。
	closeOnce sync.Once
	closeErr  error
}

func (b *idleMonitoringBody) Read(p []byte) (int, error) {
	b.readMu.Lock()
	n, err := b.rc.Read(p)
	b.readMu.Unlock()

	if n > 0 {
		b.mu.Lock()
		b.lastRead = time.Now()
		b.mu.Unlock()
	}

	if err != nil {
		return n, fmt.Errorf("Read: %w", err)
	}

	return n, nil
}

// Close 停掉后台 goroutine，先限时读掉流尾再取消请求 context、关闭底流。
//
// 上游是 chunked 响应：`[DONE]` 之后还有结束块，而读循环在 `[DONE]` 处就停了。
// Go 的 Transport 只在 body 读到 EOF 时才把连接放回空闲池；先 cancel/Close 会
// 走 earlyClose 丢掉 persistConn，每个流式请求都要重新 TCP+TLS（经代理还要
// 重建隧道）。所以先 drain：读到 EOF 才 cancel + Close。
//
// 幂等：只执行一次并缓存首次结果。
func (b *idleMonitoringBody) Close() error {
	b.closeOnce.Do(func() {
		b.stopOnce.Do(func() { close(b.stopCh) })
		b.closeErr = b.drainTailThenClose()
	})

	return b.closeErr
}

const (
	// closeDrainLimit Close 前最多读掉的流尾字节数：只覆盖 chunked 结束块等收尾
	// 字节，不把一个还没结束的流读成完整响应。
	closeDrainLimit = 4 << 10
	// closeDrainTimeout 等待流尾 EOF 的上限。
	closeDrainTimeout = 100 * time.Millisecond
)

// drainTailThenClose 限时读掉流尾：读到 EOF 才 cancel + Close，让连接回池。
// 超时说明流还没结束（客户端中途断开/换号），取消 context 并关闭底流以解除
// 阻塞中的 Read，读尾 goroutine 随之退出。
//
// 读尾不能与在途 Read 并发：keepalive 收尾（streamWithKeepalive）会先 Close
// 解阻塞，此刻它的读 goroutine 仍停在同一个底流上，而 http 响应体不支持并发
// Read。争不到读锁就按旧路径立即取消/关闭，让那个 Read 返回。
func (b *idleMonitoringBody) drainTailThenClose() error {
	if !b.readMu.TryLock() {
		b.cancel()

		return b.rc.Close()
	}

	done := make(chan struct{})
	stop := make(chan struct{})

	go func() {
		defer close(done)
		defer b.readMu.Unlock()

		buf := make([]byte, closeDrainLimit)

		for remaining := closeDrainLimit; remaining > 0; {
			// 直接读底流：不经过 b.Read，避免把流尾计入活跃时间。
			n, err := b.rc.Read(buf[:remaining])
			remaining -= n

			if err != nil {
				return
			}
			// 超时后不再继续读：底流的 Read 不保证能被 cancel/Close 打断
			// （非 http body），只能在每次读返回后从出口退出。
			select {
			case <-stop:
				return
			default:
			}
		}
	}()

	timer := time.NewTimer(closeDrainTimeout)
	defer timer.Stop()

	select {
	case <-done:
	case <-timer.C:
		close(stop)
	}

	// 读到 EOF 时 Transport 已把连接放回空闲池；超时则取消 context 让阻塞中的
	// Read 返回，读尾 goroutine 随之落到出口。两种情况都要关底流。
	b.cancel()

	return b.rc.Close()
}

func (b *idleMonitoringBody) idleFor() time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()

	return time.Since(b.lastRead)
}

// monitorBody 若 idle<=0 直接返回原底流（禁用空闲监控）；
// 否则包上流中空闲监控。计时从返回 body 之后开始——首字节阶段由
// Transport.ResponseHeaderTimeout 管，这里不抢跑。
func monitorBody(rc io.ReadCloser, idle time.Duration, cancel context.CancelFunc) io.ReadCloser {
	if idle <= 0 {
		return rc
	}

	idleMonitoringBody2 := &idleMonitoringBody{
		rc:       rc,
		lastRead: time.Now(),
		stopCh:   make(chan struct{}),
		cancel:   cancel,
	}

	go func() {
		t := time.NewTicker(idleTick(idle))
		defer t.Stop()

		for {
			select {
			case <-idleMonitoringBody2.stopCh:
				return
			case <-t.C:
				if idleMonitoringBody2.idleFor() > idle {
					cancel()
					return
				}
			}
		}
	}()

	return idleMonitoringBody2
}

// idleTick 返回监控周期：idle/4，钳在 [10ms, 1s]。小 idle 也能快速发现，大小值避免空转。
func idleTick(idle time.Duration) time.Duration {
	return max(min(idle/4, time.Second), 10*time.Millisecond)
}
