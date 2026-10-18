package upstream

import (
	"testing"
	"time"

	"go.uber.org/goleak"
)

func TestMain(m *testing.M) {
	// 维护类计费重试在单测中统一用毫秒级间隔；生产默认 2s 不变。
	billingRetryDelay = time.Millisecond

	goleak.VerifyTestMain(m,
		goleak.IgnoreAnyFunction("net/http.(*persistConn).readLoop"),
		goleak.IgnoreAnyFunction("net/http.(*persistConn).writeLoop"),
	)
}
