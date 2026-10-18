package pool

import (
	"testing"
	"time"

	"go.uber.org/goleak"
)

func TestMain(m *testing.M) {
	// 测试统一关闭防撞号窗口；需要窗口语义的用例在各自的 Pool 上注入。
	minPickGap = 0
	flushInterval = 20 * time.Millisecond

	goleak.VerifyTestMain(m,
		goleak.IgnoreAnyFunction("net/http.(*persistConn).readLoop"),
		goleak.IgnoreAnyFunction("net/http.(*persistConn).writeLoop"),
	)
}
