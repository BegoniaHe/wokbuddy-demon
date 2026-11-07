package main

import (
	"testing"
	"time"

	"github.com/BegoniaHe/wokbuddy-demon/internal/quota"
	"github.com/BegoniaHe/wokbuddy-demon/internal/usage"
)

// TestQuotaWorkerDrainsOnClose 关停前入队的撞线事件必须排空落库，且 Close 幂等
// （此前 queue 从不关闭，worker 进程期永久阻塞，是确凿的 goroutine 泄漏）。
func TestQuotaWorkerDrainsOnClose(t *testing.T) {
	t.Parallel()

	rec := usage.New("")
	store := quota.New("")
	worker := newQuotaWorker(rec, store)

	resetAt := time.Now().UTC().Truncate(time.Second)
	worker.enqueue("u1", "glm-5.2", "cn", resetAt)
	worker.enqueue("u2", "kimi-k2", "cn", resetAt)

	worker.Close()
	worker.Close() // 幂等：重复关停不应 panic 或重复等待

	if events := store.Events(); len(events) != 2 {
		t.Fatalf("关停前入队的事件必须排空落库，got %d events", len(events))
	}
}
