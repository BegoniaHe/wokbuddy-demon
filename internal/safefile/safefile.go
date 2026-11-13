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
	"time"

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
	data, _, err := ReadWithFallback(path, valid)

	return data, err
}

// ReadWithFallback 同 Read，并报告内容是否来自 <path>.bak。
//
// 区分来源是必要的：回退取到的是**上一版**，不是当前状态。对凭证/账号状态而言，
// 「静默用旧 refresh token 继续跑」与「文件已损坏」需要不同的处置（至少要留痕），
// 而 Read 的 (data, nil) 让调用方无从分辨。
func ReadWithFallback(path string, valid func([]byte) bool) ([]byte, bool, error) {
	if valid == nil {
		valid = json.Valid
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, false, fmt.Errorf("read %s: %w", path, err)
	}

	if valid(data) {
		return data, false, nil
	}

	bak := path + ".bak"

	bakData, bakErr := os.ReadFile(bak)
	if bakErr == nil && valid(bakData) {
		return bakData, true, nil
	}

	return nil, false, fmt.Errorf("%w: %s (backup %s)", ErrCorrupt, path, filepath.Base(bak))
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

// staleTempAge 同目录残留临时文件的清理阈值。
//
// 进程被 kill -9 / panic 时 CreateTemp 出来的临时文件不会被 defer 删除，而里面
// 是新旧完整的凭证。auth 目录的 glob（workbuddy*.json、*.json）都匹配不到这些
// 以 "." 开头的名字，历史上没人清理，会长期堆积。
const staleTempAge = time.Hour

// resolveWriteTarget 让写入落在符号链接的目标上，而不是把链接替换成普通文件。
//
// 自托管常见做法是把 auths/workbuddy-x.json 软链到仓库外的凭证目录：os.Rename
// 替换的是链接自身，第一次刷新后链接消失、目标文件保持过期，且无任何提示。
func resolveWriteTarget(path string) (string, error) {
	info, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return path, nil
		}

		return "", fmt.Errorf("lstat %s: %w", path, err)
	}

	if info.Mode()&fs.ModeSymlink == 0 {
		return path, nil
	}

	resolved, rerr := filepath.EvalSymlinks(path)
	if rerr != nil {
		return "", fmt.Errorf("resolve symlink %s: %w", path, rerr)
	}

	return resolved, nil
}

// sweepStaleTemps 清理同目录下本函数族遗留的过期临时文件（调用方已解析 target）。
func sweepStaleTemps(target string) {
	matches, err := filepath.Glob(filepath.Join(filepath.Dir(target), "."+filepath.Base(target)+".*"))
	if err != nil {
		return
	}

	cutoff := time.Now().Add(-staleTempAge)

	for _, match := range matches {
		info, statErr := os.Lstat(match)
		if statErr != nil || !info.Mode().IsRegular() || info.ModTime().After(cutoff) {
			continue
		}

		_ = os.Remove(match)
	}
}

// replace 写临时文件 → fsync → rename → fsync 目录。临时文件名由 os.CreateTemp
// 生成，避免多进程共用固定 ".tmp" 时互相截断。
//
// 目录 fsync 是「rename 本身」的持久化：只 sync 文件内容时掉电可能回退到旧文件
// （对 auth 而言就是刚轮换掉的旧 RT）。Windows 不支持对目录 Sync，失败忽略。
func replace(path string, data []byte, perm fs.FileMode) error {
	target, err := resolveWriteTarget(path)
	if err != nil {
		return err
	}

	if target != path {
		path = target
	}

	sweepStaleTemps(path)

	tmp, err := writeTemp(path, data, perm)
	if err != nil {
		return err
	}

	if err := atomicwrite.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp) //nolint:gosec // tmp 由 writeTemp 生成，非外部输入

		return fmt.Errorf("rename %s -> %s: %w", tmp, path, err)
	}

	syncDir(filepath.Dir(path))

	return nil
}

// writeTemp 在 path 同目录写临时文件（chmod → write → fsync → close），返回临时名。
// 失败时自行清理，成功时把删除责任交给调用方（rename 成功后临时名已不存在）。
func writeTemp(path string, data []byte, perm fs.FileMode) (string, error) {
	file, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return "", fmt.Errorf("create temp for %s: %w", path, err)
	}

	tmp := file.Name()

	fail := func(operation string, opErr error) (string, error) {
		_ = file.Close()
		_ = os.Remove(tmp)

		return "", fmt.Errorf("%s %s: %w", operation, tmp, opErr)
	}

	if err := file.Chmod(perm); err != nil {
		return fail("chmod", err)
	}

	if _, err := file.Write(data); err != nil {
		return fail("write", err)
	}

	if err := file.Sync(); err != nil {
		return fail("sync", err)
	}

	if err := file.Close(); err != nil {
		_ = os.Remove(tmp)

		return "", fmt.Errorf("close %s: %w", tmp, err)
	}

	return tmp, nil
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
