package main

import (
	"context"
	"testing"
)

// TestWarmModelRatesIsolatesPanic 预热跑在裸协程里（启动处 `go warmModelRates`），
// 任何 panic 都会终止整个进程、连带所有账号的 /v1/*。nil pool 是这条路径上最直接的
// 注入点：进入函数即解引用，panic 同步抛出，不经过 FetchModels 内部已有的 guardProbe。
//
// 该用例没有可断言的返回值：panic 逃逸出本函数就会让测试失败，隔离生效则正常返回。
func TestWarmModelRatesIsolatesPanic(t *testing.T) {
	t.Parallel()

	warmModelRates(context.Background(), nil, nil)
}
