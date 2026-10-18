package scheduler

import (
	"testing"

	"go.uber.org/goleak"
)

func TestMain(m *testing.M) {
	// 单测统一关闭账号间限速；需要限速语义的用例在各自 Scheduler 上验证。
	activityAccountDelay = 0
	travelAccountDelay = 0
	adoptReportGap = 0

	goleak.VerifyTestMain(m,
		goleak.IgnoreAnyFunction("net/http.(*persistConn).readLoop"),
		goleak.IgnoreAnyFunction("net/http.(*persistConn).writeLoop"),
	)
}
