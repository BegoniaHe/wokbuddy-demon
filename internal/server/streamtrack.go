// streamtrack.go 在途 SSE 长流注册表（issue #19）：让优雅关闭能显式等待活跃流排空
// （有上限）并记录未排空数，而不是按 HTTP 生命周期一刀切后静默丢弃。
//
// 配置热更不重启进程（livecfg 快照 + 热字段），在途流天然不受影响——本注册表只服务
// 关闭路径的可观测与有界等待，不介入热更。
package server

import (
	"context"
	"sync"
)

// streamTracker 计数在途 SSE 流，并提供带 ctx 上限的排空等待。
type streamTracker struct {
	mu     sync.Mutex
	active int
	// zero 在 active 归零时被关闭并替换（广播）；等待者捕获旧 channel 后阻塞，
	// 归零即被唤醒并重新判定。
	zero chan struct{}
}

func newStreamTracker() *streamTracker {
	return &streamTracker{zero: make(chan struct{})}
}

// enter 标记一个流开始。
func (t *streamTracker) enter() {
	t.mu.Lock()
	t.active++
	t.mu.Unlock()
}

// leave 标记一个流结束；归零时广播唤醒等待者。
func (t *streamTracker) leave() {
	t.mu.Lock()

	t.active--
	if t.active <= 0 {
		t.active = 0
		close(t.zero)
		t.zero = make(chan struct{})
	}
	t.mu.Unlock()
}

// Active 返回当前在途流数。
func (t *streamTracker) Active() int {
	t.mu.Lock()
	defer t.mu.Unlock()

	return t.active
}

// WaitDrained 阻塞到在途流归零或 ctx 结束，返回是否已排空。
func (t *streamTracker) WaitDrained(ctx context.Context) bool {
	for {
		t.mu.Lock()
		if t.active == 0 {
			t.mu.Unlock()
			return true
		}

		ch := t.zero
		t.mu.Unlock()

		select {
		case <-ch:
		case <-ctx.Done():
			return false
		}
	}
}
