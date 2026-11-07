//go:build !windows

package atomicwrite

import "os"

func renamePlatform(oldpath, newpath string) error {
	return renameWithRetry(oldpath, newpath, os.Rename, func(error) bool { return false })
}
