// Package safefile 原子、掉电安全地替换状态文件，并在文件损坏时回退到上一份好副本。
//
// 为什么 tmp + rename 不够：rename 只保证读者看到完整文件，不保证内容已落盘。
// 掉电时可能留下长度正确但内容全 0 的文件（ext4/XFS 上 rename 的日志先提交、
// 数据块后刷盘）。因此临时文件先 Sync 再 rename，并把替换前的有效内容留成
// <path>.bak —— 坏文件被读成空之后，下一次写入就会把它彻底抹掉，.bak 是唯一退路。
package safefile

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/BegoniaHe/wokbuddy-demon/internal/atomicwrite"
)

// ErrCorrupt 正文损坏且 .bak 不可用。
var ErrCorrupt = errors.New("safefile: file corrupt")

// Write 用同目录唯一临时文件写入、fsync 后 rename。
func Write(path string, data []byte, perm fs.FileMode) error {
	return replace(path, data, perm)
}

// WriteKeep 同 Write，并在替换前把 path 现有的有效内容留成 <path>.bak。
// valid 与 Read 同口径（nil = json.Valid）；现有内容通不过 valid 时不覆盖 .bak
// （避免用「合法 JSON 但调用方无法解析」的内容污染唯一退路）。
func WriteKeep(path string, data []byte, perm fs.FileMode, valid func([]byte) bool) error {
	if valid == nil {
		valid = json.Valid
	}

	if err := keep(path, perm, valid); err != nil {
		return err
	}

	return replace(path, data, perm)
}

// Read 读取 path；内容通不过 valid（nil = json.Valid）时回退 <path>.bak。
// path 不存在返回 fs.ErrNotExist，调用方据此区分首次启动与坏文件。
func Read(path string, valid func([]byte) bool) ([]byte, error) {
	if valid == nil {
		valid = json.Valid
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}

	if valid(data) {
		return data, nil
	}

	bak := path + ".bak"

	bakData, bakErr := os.ReadFile(bak)
	if bakErr == nil && valid(bakData) {
		return bakData, nil
	}

	return nil, fmt.Errorf("%w: %s (backup %s)", ErrCorrupt, path, filepath.Base(bak))
}

func keep(path string, perm fs.FileMode, valid func([]byte) bool) error {
	prev, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}

	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}

	if !valid(prev) {
		return nil
	}

	bak := path + ".bak"

	old, err := os.ReadFile(bak)
	if err == nil && bytes.Equal(old, prev) {
		return nil
	}

	return replace(bak, prev, perm)
}

// replace 写临时文件 → fsync → rename → fsync 目录。临时文件名由 os.CreateTemp
// 生成，避免多进程共用固定 ".tmp" 时互相截断。
//
// 目录 fsync 是「rename 本身」的持久化：只 sync 文件内容时掉电可能回退到旧文件
// （对 auth 而言就是刚轮换掉的旧 RT）。Windows 不支持对目录 Sync，失败忽略。
func replace(path string, data []byte, perm fs.FileMode) error {
	file, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return fmt.Errorf("create temp for %s: %w", path, err)
	}

	tmp := file.Name()

	defer func() {
		if tmp != "" {
			_ = os.Remove(tmp)
		}
	}()

	if err := file.Chmod(perm); err != nil {
		_ = file.Close()
		return fmt.Errorf("chmod %s: %w", tmp, err)
	}

	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return fmt.Errorf("write %s: %w", tmp, err)
	}

	if err := file.Sync(); err != nil {
		_ = file.Close()
		return fmt.Errorf("sync %s: %w", tmp, err)
	}

	if err := file.Close(); err != nil {
		return fmt.Errorf("close %s: %w", tmp, err)
	}

	if err := atomicwrite.Rename(tmp, path); err != nil {
		return fmt.Errorf("rename %s -> %s: %w", tmp, path, err)
	}

	tmp = ""

	syncDir(filepath.Dir(path))

	return nil
}

// syncDir 尽力把目录项刷盘；不支持目录 fsync 的平台（Windows）静默忽略。
func syncDir(dir string) {
	dirFile, err := os.Open(dir)
	if err != nil {
		return
	}

	_ = dirFile.Sync()
	_ = dirFile.Close()
}
