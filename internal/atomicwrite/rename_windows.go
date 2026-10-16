//go:build windows

package atomicwrite

import (
	"errors"
	"io/fs"
	"os"
	"syscall"
)

// Windows MoveFileEx 在目标文件被占用时返回的瞬时错误裸码（无 POSIX 映射）。
const (
	errSharingViolation syscall.Errno = 32 // ERROR_SHARING_VIOLATION
	errLockViolation    syscall.Errno = 33 // ERROR_LOCK_VIOLATION
)

func rename(oldpath, newpath string) error {
	return renameWithRetry(oldpath, newpath, os.Rename, retryableRenameErr)
}

// retryableRenameErr 判定 Windows rename 错误是否瞬时（可重试）：
// ERROR_ACCESS_DENIED（杀软/索引器持有目标文件，映射 fs.ErrPermission）与共享/锁
// 冲突（裸码 32/33），以及 EBUSY/EMFILE/ENFILE 兜底。其余错误（路径不存在、权限
// 永久拒绝等）不重试。
func retryableRenameErr(err error) bool {
	if errors.Is(err, fs.ErrPermission) {
		return true
	}
	var errno syscall.Errno
	if !errors.As(err, &errno) {
		return false
	}
	switch errno {
	case errSharingViolation, errLockViolation, syscall.EBUSY, syscall.EMFILE, syscall.ENFILE:
		return true
	}
	return false
}
