package runlog

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain 兜住 Sink 的常驻刷盘协程：Close 漏调时这里会报泄漏，
// 而不是等到停机才发现最后一批运行日志没落盘。
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m,
		goleak.IgnoreAnyFunction("net/http.(*persistConn).readLoop"),
		goleak.IgnoreAnyFunction("net/http.(*persistConn).writeLoop"),
	)
}
