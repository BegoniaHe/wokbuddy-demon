// migrate-auths 一次性迁移工具：把旧版 snake_case 扁平格式的 auth 文件
// （access_token / refresh_token / expires_at 毫秒等）转换为 internal/auth
// 当前支持的格式，使 auth.LoadDir 能正常载入。
//
// 背景：早期落盘的 auth 文件用 snake_case 键名与毫秒 expires_at，当前 Parse 只认
// 嵌套形与扁平 camelCase 形，导致这些文件被 LoadDir 静默跳过（启动日志
// loaded 0 account(s)）。
//
// 用法：
//
//	go run ./cmd/migrate-auths -dir auths          # dry-run 预览（默认不写盘）
//	go run ./cmd/migrate-auths -dir auths -apply   # 实际迁移，原文件留存 .legacy 副本
//
// 幂等：已是当前格式的文件直接跳过；可重复运行。
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
)

// 行状态。
const (
	statusCurrent  = "CURRENT"       // 已是当前格式，跳过
	statusWould    = "WOULD_MIGRATE" // dry-run：可迁移
	statusMigrated = "MIGRATED"      // 已迁移并落盘
	statusInvalid  = "INVALID"       // 缺必需字段 / realm 非法，需人工处理
	statusFailed   = "FAILED"        // 读写失败
)

var (
	errWriteBackupFailed   = errors.New("writeBackup failed")
	errMissingUID          = errors.New("missing uid")
	errMissingAccessToken  = errors.New("missing access_token")
	errMissingRefreshToken = errors.New("missing refresh_token")
)

type row struct {
	file   string
	uid    string
	nick   string
	status string
	detail string
}

// counts 按状态汇总，供结尾报告与退出码判定。
type counts struct {
	converted int
	current   int
	invalid   int
	failed    int
}

func main() {
	dir := flag.String("dir", "auths", "auth 目录")
	apply := flag.Bool("apply", false, "实际写盘（缺省 dry-run 仅预览）")

	flag.Parse()

	files, err := filepath.Glob(filepath.Join(*dir, "workbuddy*.json"))
	if err != nil {
		failf("glob: %v", err)
	}

	if len(files) == 0 {
		failf("no auth files in %s", *dir)
	}

	slices.Sort(files)

	rows, counts2 := migrateAll(files, *apply)
	printTable(rows)
	report(counts2, len(rows), *apply)

	if counts2.invalid > 0 || counts2.failed > 0 {
		exitFunc(1)
	}

	if *apply && !reportLoadable(*dir, counts2.current+counts2.converted) {
		exitFunc(1)
	}
}

// migrateAll 逐个迁移并汇总状态；重复 UID 按 LoadDir 同口径告警（后者覆盖前者）。
func migrateAll(files []string, apply bool) ([]row, counts) {
	var counts2 counts

	rows := make([]row, 0, len(files))

	seen := make(map[string]string, len(files))
	for _, f := range files {
		r := migrateFile(f, apply)

		rows = append(rows, r)
		switch r.status {
		case statusMigrated, statusWould:
			counts2.converted++
		case statusCurrent:
			counts2.current++
		case statusInvalid:
			counts2.invalid++
		default:
			counts2.failed++
		}

		if r.uid == "" {
			continue
		}

		if prev, ok := seen[r.uid]; ok {
			fmt.Printf("WARN: uid %s 重复（%s 与 %s），加载时后者覆盖前者\n", r.uid, prev, r.file)
		}

		seen[r.uid] = r.file
	}

	return rows, counts2
}

// report 打印汇总与下一步提示。
func report(counts2 counts, total int, apply bool) {
	verb := "可迁移"
	if apply {
		verb = "已迁移"
	}

	fmt.Printf("\ntotal=%d %s=%d current=%d invalid=%d failed=%d\n",
		total, verb, counts2.converted, counts2.current, counts2.invalid, counts2.failed)

	if !apply && counts2.converted > 0 {
		fmt.Println("dry-run：加 -apply 实际写盘（原文件留存 .legacy 副本）")
	}
}

// migrateFile 处理单个 auth 文件：当前格式跳过，旧 snake_case 扁平形转换；
// apply 时先留存 .legacy 副本，再经 SaveAtomic 原子写回。
func migrateFile(path string, apply bool) row {
	r := row{file: filepath.Base(path)}

	raw, err := os.ReadFile(path)
	if err != nil {
		r.status, r.detail = statusFailed, err.Error()
		return r
	}

	if a, perr := auth.Parse(raw); perr == nil {
		r.uid, r.nick = a.UID, a.Nickname
		r.status, r.detail = statusCurrent, "已是当前格式"

		return r
	}

	account, err := convertLegacy(raw)
	if err != nil {
		r.status, r.detail = statusInvalid, err.Error()
		return r
	}

	r.uid, r.nick = account.UID, account.Nickname

	r.detail = fmt.Sprintf("realm=%s expiresAt=%d", account.RealmStored(), account.ExpiresAt)
	if account.NeedsRefresh(0) {
		r.detail += "（token 已过期，启动后自动刷新）"
	}

	if !apply {
		r.status = statusWould
		return r
	}

	if err := writeBackup(path, raw); err != nil {
		r.status, r.detail = statusFailed, "备份 .legacy: "+err.Error()
		return r
	}

	account.FilePath = path
	if err := account.SaveAtomic(); err != nil {
		r.status, r.detail = statusFailed, "写回: "+err.Error()
		return r
	}

	r.status = statusMigrated

	return r
}

// writeBackup 写 .legacy 原始凭证备份。已存在且内容一致视为成功（上次写盘失败后
// 重跑），内容不一致则拒绝覆盖——避免毁掉更早一次迁移留下的原始文件。
func writeBackup(path string, raw []byte) error {
	backup := path + ".legacy"

	existing, err := os.ReadFile(backup)
	switch {
	case err == nil:
		if !bytes.Equal(existing, raw) {
			return fmt.Errorf("%s 已存在且内容不同，拒绝覆盖（确认无用后手动删除）: %w", filepath.Base(backup), errWriteBackupFailed)
		}

		return nil
	case !errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("writeBackup: %w", err)
	}

	if err := os.WriteFile(backup, raw, 0o600); err != nil { //nolint:gosec // 备份路径派生自本机 auth 文件路径
		return fmt.Errorf("write backup: %w", err)
	}

	return nil
}

// convertLegacy 把旧版 snake_case 扁平内容解析为 *auth.Auth：
// access_token→AccessToken、expires_at（毫秒/秒自动识别）→ExpiresAt（秒）、
// enterprise_id→EnterpriseID、device_token 原样保留。realm 缺省时按 domain 推断、
// 以 access token 的 iss claim 兜底（复用 auth.BackfillRealm）。
// 必需字段（uid / access_token / refresh_token）缺失返回 error。
func convertLegacy(raw []byte) (*auth.Auth, error) {
	var item struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresAt    int64  `json:"expires_at"`
		Domain       string `json:"domain"`
		Realm        string `json:"realm"`
		UID          string `json:"uid"`
		EnterpriseID string `json:"enterprise_id"`
		Nickname     string `json:"nickname"`
		DeviceToken  string `json:"device_token"`
	}
	if err := json.Unmarshal(raw, &item); err != nil {
		return nil, fmt.Errorf("legacy json: %w", err)
	}

	switch {
	case strings.TrimSpace(item.UID) == "":
		return nil, errMissingUID
	case strings.TrimSpace(item.AccessToken) == "":
		return nil, errMissingAccessToken
	case strings.TrimSpace(item.RefreshToken) == "":
		return nil, errMissingRefreshToken
	}

	account := &auth.Auth{
		AccessToken:  item.AccessToken,
		RefreshToken: item.RefreshToken,
		ExpiresAt:    normalizeExpiry(item.ExpiresAt),
		Domain:       item.Domain,
		UID:          item.UID,
		EnterpriseID: item.EnterpriseID,
		Nickname:     item.Nickname,
		DeviceToken:  item.DeviceToken,
	}
	if r := strings.TrimSpace(item.Realm); r != "" {
		if _, err := auth.BackfillRealmFor(account, r); err != nil {
			return nil, fmt.Errorf("realm %q: %w", r, err)
		}
	} else {
		account.BackfillRealm()
	}

	return account, nil
}

// epochMillisThreshold 秒/毫秒时间戳分界（1e11 秒 ≈ 公元 5138 年）。
const epochMillisThreshold = 100_000_000_000

// normalizeExpiry 归一化过期时间戳为 Unix 秒：大于 1e11 视为毫秒（旧版工具导出
// 毫秒时间戳；1e11 秒 ≈ 公元 5138 年，不会与合法秒级时间戳冲突）。
func normalizeExpiry(v int64) int64 {
	if v > epochMillisThreshold {
		return v / 1000
	}

	return v
}

// reportLoadable 迁移后按服务同口径复扫，确认迁移产物确实可被载入。
// want 为本轮 current+converted 文件数；LoadDir 静默跳过解析失败的文件，
// 数量不符即说明仍有文件会被服务忽略。
func reportLoadable(dir string, want int) bool {
	list, err := auth.LoadDir(dir)
	if err != nil {
		fmt.Printf("校验失败：auth.LoadDir: %v\n", err)
		return false
	}

	if len(list) != want {
		fmt.Printf("校验失败：auth.LoadDir 载入 %d 个账号，期望 %d（检查被跳过的文件）\n", len(list), want)
		return false
	}

	fmt.Printf("校验：auth.LoadDir 载入 %d 个账号\n", len(list))

	return true
}

func printTable(rows []row) {
	fmt.Printf("%-32s | %-36s | %-11s | %-13s | %s\n", "file", "uid", "nick", "status", "detail")
	fmt.Println(strings.Repeat("-", 120))

	for _, r := range rows {
		fmt.Printf("%-32s | %-36s | %-11s | %-13s | %s\n",
			trunc(r.file, 32), trunc(r.uid, 36), trunc(r.nick, 11), r.status, r.detail)
	}
}

// trunc 按字符截断，避免切断 UTF-8 多字节序列（昵称常为中文）。
func trunc(str string, n int) string {
	r := []rune(str)
	if len(r) > n {
		return string(r[:n])
	}

	return str
}

// exitFunc 供测试替换（默认 os.Exit；与 cmd/login 同款）。
var exitFunc = os.Exit

func failf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "migrate-auths: "+format+"\n", args...)
	exitFunc(1)
}
