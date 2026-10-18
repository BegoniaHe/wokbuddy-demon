package pool

import "testing"

// newTestPool 创建带落盘 goroutine 的池，并在测试结束时关闭，避免 goleak 误报。
func newTestPool(t *testing.T, stateFp string) *Pool {
	t.Helper()

	p := New(stateFp)
	t.Cleanup(p.Close)

	return p
}
