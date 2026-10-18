// Package atomicwrite 统一原子替换（tmp + rename）的 rename 一步。
//
// 动机：Windows 上 MoveFileEx 可能因目标文件被杀软、搜索索引器或其他进程短暂持有
// 而返回瞬时错误（ERROR_ACCESS_DENIED / 共享冲突），单次 rename 失败即导致整次写盘
// 失败。本包在 Windows 下做有界退避重试，非 Windows 保持单次 rename 语义（issue #26）。
package atomicwrite

import "time"

// 退避参数：有界（上限次数 + 延迟封顶），避免调用方（如 auth.SaveAtomic 全程持锁）
// 因重试长时间阻塞。测试可临时调小。
var (
	renameMaxAttempts = 50
	renameBaseDelay   = 2 * time.Millisecond
	renameMaxDelay    = 50 * time.Millisecond
)

// Rename 原子替换 oldpath → newpath。非 Windows 等价于单次 os.Rename；
// Windows 对目标被占用导致的瞬时错误做有界退避重试。
func Rename(oldpath, newpath string) error {
	return rename(oldpath, newpath)
}

// renameWithRetry 有界退避重试 doRename：仅在 retryable(err) 为真时重试，成功或
// 不可重试错误立即返回，重试耗尽返回最后一次错误。
//
// 拆出 doRename/retryable 注入点：平台 rename 在 windows/非 windows 两个构建文件里
// 提供，跨平台单测可注入假 rename 覆盖「重试后成功 / 重试耗尽 / 不可重试」。
func renameWithRetry(oldpath, newpath string, doRename func(string, string) error, retryable func(error) bool) error {
	var err error

	delay := renameBaseDelay

	for attempt := range renameMaxAttempts {
		err = doRename(oldpath, newpath)
		if err == nil {
			return nil
		}

		if !retryable(err) || attempt == renameMaxAttempts-1 {
			return err
		}

		time.Sleep(delay)

		if delay < renameMaxDelay {
			delay *= 2
			if delay > renameMaxDelay {
				delay = renameMaxDelay
			}
		}
	}

	return err
}
