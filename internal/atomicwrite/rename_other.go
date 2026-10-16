//go:build !windows

package atomicwrite

import "os"

// rename 非 Windows：单次 os.Rename（POSIX rename 对已被占用的目标不返回瞬时错误，
// 无需重试；保持既有语义）。
func rename(oldpath, newpath string) error {
	return renameWithRetry(oldpath, newpath, os.Rename, func(error) bool { return false })
}
