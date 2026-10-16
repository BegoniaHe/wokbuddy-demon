// signin 一次性批量签到工具：遍历 ./auths/workbuddy-*.json 全部账号，
// 自动 RefreshToken（过期时），逐个调 daily-checkin，顺手查余额。
package main

import (
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
	"github.com/BegoniaHe/wokbuddy-demon/internal/upstream"
)

type row struct {
	file     string
	uid      string
	nick     string
	status   string // OK | ALREADY | FAIL | AUTH_INVALID | LOAD_ERR
	detail   string
	remain   int64
	hasQuota bool
}

func main() {
	dir := "auths"
	if len(os.Args) > 1 {
		dir = os.Args[1]
	}
	files, err := filepath.Glob(filepath.Join(dir, "workbuddy-*.json"))
	if err != nil || len(files) == 0 {
		fmt.Fprintf(os.Stderr, "no auth files in %s\n", dir)
		os.Exit(1)
	}
	sort.Strings(files)
	up := upstream.New()

	rows, okN, alreadyN, failN := processAccounts(files, up)

	// 报告
	fmt.Printf("uid                                  | nick        | status       | remain | detail\n")
	fmt.Printf("-------------------------------------+-------------+--------------+--------+------------------------------\n")
	for _, r := range rows {
		remain := "-"
		if r.hasQuota {
			remain = strconv.FormatInt(r.remain, 10)
		}
		fmt.Printf("%-36s | %-11s | %-12s | %-6s | %s\n",
			trunc(r.uid, 36), trunc(r.nick, 11), r.status, remain, r.detail)
	}
	fmt.Printf("\ntotal=%d ok=%d already=%d fail=%d\n", len(rows), okN, alreadyN, failN)
}

// processAccounts 逐个账号签到并计数：OK / ALREADY 分别计数，其余状态一律计入 fail。
func processAccounts(files []string, up *upstream.Client) (rows []row, okN, alreadyN, failN int) {
	for _, f := range files {
		r := processAccount(f, up)
		rows = append(rows, r)
		switch r.status {
		case "OK":
			okN++
		case "ALREADY":
			alreadyN++
		default:
			failN++
		}
	}
	return rows, okN, alreadyN, failN
}

// processAccount 处理单个 auth 文件：读取/解析、按需 refresh、签到、顺手查余额。
// 读取/解析/refresh 失败立即返回带状态的 row（与旧行为一致）。
func processAccount(f string, up *upstream.Client) row {
	r := row{file: filepath.Base(f)}
	raw, err := os.ReadFile(f)
	if err != nil {
		r.status, r.detail = "LOAD_ERR", err.Error()
		return r
	}
	a, err := auth.Parse(raw)
	if err != nil {
		r.status, r.detail = "LOAD_ERR", err.Error()
		return r
	}
	a.FilePath = f
	r.uid, r.nick = a.UID, a.Nickname

	// refresh 过期 token
	if err := refreshIfNeeded(a, up); err != nil {
		ue := &upstream.Error{}
		if errors.As(err, &ue) {
			r.status = "AUTH_INVALID"
		} else {
			r.status = "FAIL"
		}
		r.detail = "refresh: " + short(err.Error())
		return r
	}

	err = up.DailyCheckin(a)
	switch err {
	case nil:
		r.status = "OK"
	default:
		// DailyCheckin 已签到返回 code!=0 错误
		if isAlready(err.Error()) {
			r.status = "ALREADY"
		} else {
			r.status = "FAIL"
		}
		r.detail = short(err.Error())
	}
	// 顺手查余额
	if remain, _, qerr := up.UserResource(a); qerr == nil {
		r.remain, r.hasQuota = remain, true
	}
	return r
}

// refreshIfNeeded 在 token 临近过期（2h 内）时刷新并原子落盘；
// 落盘失败仅记日志（与旧行为一致），刷新错误原样上抛供调用方分类。
func refreshIfNeeded(a *auth.Auth, up *upstream.Client) error {
	if !a.NeedsRefresh(2 * 3600) {
		return nil
	}
	if err := up.RefreshToken(a); err != nil {
		return err
	}
	if err := a.SaveAtomic(); err != nil {
		log.Printf("signin %q save: %q", a.UID, err.Error()) //nolint:gosec // 参数经 %q 转义，无注入面
	}
	return nil
}

// 已签判定：code 非 0 且含 "已签到"/"already"/"checkin" 等字样。
func isAlready(msg string) bool {
	s := strings.ToLower(msg)
	return strings.Contains(s, "已签到") ||
		strings.Contains(s, "already") ||
		strings.Contains(s, "checkin") ||
		strings.Contains(s, "code=400")
}

func trunc(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

func short(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > 60 {
		return s[:60]
	}
	return s
}
