// guarded.go 面板后台协程的 panic 隔离。
//
// net/http 的 recover 只覆盖 handler 自身协程：面板派生的批量执行、补领清扫、
// 导入、昵称补齐等协程里跑的是上游响应解析、map 索引与类型断言，一次 panic 就是
// 进程级终止，连带所有账号的 /v1/* 一起中断。调度器侧已有 runTaskGuarded /
// refreshBalanceGuarded / runHookGuarded，这里补上同一口径。
package panel

import (
	"log"
	"runtime/debug"
)

// goGuarded 启动一个带 panic 隔离的后台协程。name 只用于日志定位。
func (*Panel) goGuarded(name string, fn func()) {
	go func() {
		defer recoverGuarded(name)

		fn()
	}()
}

// recoverGuarded 在 defer 中隔离 panic，供自带 WaitGroup/信号量的扇出直接使用
// （goGuarded 不返回句柄，无法配合 wg.Add/Done 与 sem 归还）。
func recoverGuarded(name string) {
	if rec := recover(); rec != nil {
		log.Printf("ERR: [panel] %s panic（已隔离）: %v\n%s", name, rec, debug.Stack())
	}
}
