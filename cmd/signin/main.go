// signin 一次性批量签到工具：遍历 ./auths/workbuddy-*.json 全部账号，
// 自动 RefreshToken（过期时），逐个调 daily-checkin，顺手查余额。
package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
	"github.com/BegoniaHe/wokbuddy-demon/internal/clientprofile"
	"github.com/BegoniaHe/wokbuddy-demon/internal/logfmt"
	"github.com/BegoniaHe/wokbuddy-demon/internal/upstream"
)

type row struct {
	file     string
	uid      string
	nick     string
	status   string // OK | ALREADY | INACTIVE | FAIL | AUTH_INVALID | LOAD_ERR
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

	slices.Sort(files)

	profiles, perr := clientprofile.LoadDir("data/client-profiles")
	if perr != nil {
		fmt.Fprintf(os.Stderr, "load client profiles: %v\n", perr)
		os.Exit(1)
	}

	up := upstream.New()
	up.Profile = profiles

	rows, okN, alreadyN, inactiveN, failN := processAccounts(files, up)

	// 报告
	fmt.Printf("uid                                  | nick        | status       | remain | detail\n")
	fmt.Printf("-------------------------------------+-------------+--------------+--------+" +
		"------------------------------\n")

	for _, r := range rows {
		remain := "-"
		if r.hasQuota {
			remain = strconv.FormatInt(r.remain, 10)
		}

		fmt.Printf("%-36s | %-11s | %-12s | %-6s | %s\n",
			trunc(r.uid, 36), trunc(r.nick, 11), r.status, remain, r.detail)
	}

	fmt.Printf("\ntotal=%d ok=%d already=%d inactive=%d fail=%d\n", len(rows), okN, alreadyN, inactiveN, failN)
}

// processAccounts 逐个账号签到并计数：OK / ALREADY 分别计数，其余状态一律计入 fail。
func processAccounts(files []string, up *upstream.Client) (rows []row, okN, alreadyN, inactiveN, failN int) {
	for _, f := range files {
		r := processAccount(f, up)

		rows = append(rows, r)
		switch r.status {
		case "OK":
			okN++
		case "ALREADY":
			alreadyN++
		case "INACTIVE":
			inactiveN++
		default:
			failN++
		}
	}

	return rows, okN, alreadyN, inactiveN, failN
}

// processAccount 处理单个 auth 文件：读取/解析、按需 refresh、签到、顺手查余额。
// 读取/解析/refresh 失败立即返回带状态的 row（与旧行为一致）。
func processAccount(str string, client *upstream.Client) row {
	r := row{file: filepath.Base(str)}

	raw, err := os.ReadFile(str) //nolint:gosec // 路径来自本机 CLI 参数，非远程输入
	if err != nil {
		r.status, r.detail = "LOAD_ERR", err.Error()
		return r
	}

	account, err := auth.Parse(raw)
	if err != nil {
		r.status, r.detail = "LOAD_ERR", err.Error()
		return r
	}

	account.FilePath = str
	r.uid, r.nick = account.UID, account.Nickname

	// profile 是出站指纹的唯一来源：缺对应 realm 时不得发请求。
	if detail := missingProfile(client, account); detail != "" {
		r.status, r.detail = "FAIL", detail

		return r
	}

	// refresh 过期 token
	if status, detail, failed := refreshFailure(account, client); failed {
		r.status, r.detail = status, detail

		return r
	}

	err = client.DailyCheckin(account)
	switch upstream.ClassifyCheckin(err) {
	case upstream.CheckinOK:
		r.status = "OK"
	case upstream.CheckinAlready:
		r.status = "ALREADY"
	case upstream.CheckinInactive:
		r.status = "INACTIVE"
		r.detail = short(err.Error())
	default:
		r.status = "FAIL"
		r.detail = short(err.Error())
	}
	// 顺手查余额
	if remain, _, qerr := client.UserResource(account); qerr == nil {
		r.remain, r.hasQuota = remain, true
	}

	return r
}

// missingProfile 返回出站 profile 缺失说明；齐全时返回空串。
func missingProfile(client *upstream.Client, account *auth.Auth) string {
	if client.Profile == nil || client.Profile.ForRealm(account.Realm()) == nil {
		return "missing client profile for realm " + account.Realm()
	}

	return ""
}

// refreshFailure 按需刷新 token，返回失败状态与说明；未失败时 failed=false。
func refreshFailure(account *auth.Auth, client *upstream.Client) (status, detail string, failed bool) {
	rerr := refreshIfNeeded(account, client)
	if rerr == nil {
		return "", "", false
	}

	status = classifyRefreshFailure(rerr)

	return status, "refresh: " + short(rerr.Error()), true
}

// classifyRefreshFailure 只在凭证确实不可用时报 AUTH_INVALID；上游 5xx/限流/
// 传输层错误只算本次 FAIL，避免把健康账号误导成「需要重新登录」。
func classifyRefreshFailure(err error) string {
	if upstream.IsCredentialDead(err) {
		return "AUTH_INVALID"
	}

	return "FAIL"
}

// refreshIfNeeded 在 token 临近过期（2h 内）时刷新并原子落盘；
// 落盘失败仅记日志（与旧行为一致），刷新错误原样上抛供调用方分类。
func refreshIfNeeded(account *auth.Auth, up *upstream.Client) error {
	if !account.NeedsRefresh(2 * time.Hour) {
		return nil
	}

	if err := up.RefreshTokenIfNeeded(account, 2*time.Hour); err != nil {
		return fmt.Errorf("refreshIfNeeded: %w", err)
	}

	if err := account.SaveAtomicRetry(); err != nil {
		log.Printf("signin %q save: %q", account.UID, err.Error()) //nolint:gosec // 参数经 %q 转义，无注入面
	}

	return nil
}

func trunc(s string, n int) string {
	return logfmt.Truncate(s, n)
}

func short(str2 string) string {
	str2 = strings.ReplaceAll(str2, "\n", " ")

	return logfmt.Truncate(str2, 60)
}
