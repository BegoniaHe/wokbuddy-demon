// panic_isolation_test.go 钉住调度器侧的 panic 隔离：HTTP 路径有 net/http 的
// per-connection recover 兜底，调度器派生的 goroutine 没有——签到/旅行/成长任务
// 在解析上游响应时一次越界 panic 会带崩整个网关。
package scheduler

import (
	"testing"
)

// TestRunBatchIsolatesHookPanic 成长队列 hook panic 不得穿透 runBatch 的 goroutine。
// 若隔离失效，panic 会直接终止测试进程（这本身就是断言）。
func TestRunBatchIsolatesHookPanic(t *testing.T) {
	t.Parallel()

	called := false

	sched := New(&Config{GrowthHook: func() {
		called = true

		panic("hook boom")
	}})

	sched.runBatch(t.Context(), []taskKind{taskGrowth})

	if !called {
		t.Fatal("成长 hook 未被调用")
	}
}

// TestRunHookGuardedNilSafe 回调为 nil 时不应被调用（hook 挂载前的排程路径）。
func TestRunHookGuardedNilSafe(t *testing.T) {
	t.Parallel()

	sched := New(&Config{})

	// runBatch 对 nil hook 直接跳过；这里验证不 panic 且能正常返回。
	sched.runBatch(t.Context(), []taskKind{taskGrowth})
}
