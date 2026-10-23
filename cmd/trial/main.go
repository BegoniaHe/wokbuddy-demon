// trial 一次性批量领取 global trial 加油包：遍历 auths 下全部账号，
// 仅对 global 账号执行 /billing/ide/trial（CN 无此端点，明确提示不适用）。
//
// 用法：
//
//	# 本地：在项目根目录（需 config.json + auths/ + data/）直接 run
//	go run ./cmd/trial
//
//	# 容器内：先 cp 进去再 exec
//	docker cp trial wokbuddy-demon:/tmp/trial
//	docker exec -w /app wokbuddy-demon /tmp/trial
//
// 结果逐账号输出到 stdout：
//
//	uid | nick | status | detail
//	----+------+--------+-------
//	... | GLOBAL | OK | trial granted
//	... | GLOBAL | ALREADY | 已领取过（幂等，不算失败）
//	... | CN     | N/A   | not applicable
package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
	"github.com/BegoniaHe/wokbuddy-demon/internal/clientprofile"
	"github.com/BegoniaHe/wokbuddy-demon/internal/upstream"
)

// errGlobalProfileMissing trial 缺少 global 客户端指纹。
var errGlobalProfileMissing = errors.New("缺少 data/client-profiles/global.json：trial 需要 global 客户端指纹")

// classifyTrial 归一化 ClaimTrial 结果（纯函数，供 main 循环与测试直接断言）：
// err → FAIL；claimed → OK；否则（幂等码已领）→ ALREADY。
func classifyTrial(claimed bool, err error) (trialStatus, string) {
	switch {
	case err != nil:
		return trialFailed, err.Error()
	case claimed:
		return trialOK, "trial granted"
	default:
		return trialAlready, "already claimed (idempotent)"
	}
}

// trialStatus 单账号 trial 领取结果状态。
type trialStatus string

const (
	trialOK      trialStatus = "OK"
	trialAlready trialStatus = "ALREADY"
	trialNotApp  trialStatus = "N/A" // CN 账号不适用
	trialFailed  trialStatus = "FAIL"
)

type trialRow struct {
	uid    string
	nick   string
	status trialStatus
	detail string
}

func main() {
	authDir := "auths"
	if len(os.Args) > 1 {
		authDir = os.Args[1]
	}

	files, err := filepath.Glob(filepath.Join(authDir, "workbuddy-*.json"))
	if err != nil || len(files) == 0 {
		fmt.Fprintf(os.Stderr, "no auth files in %s\n", authDir)
		os.Exit(1)
	}

	sort.Strings(files)

	profiles, perr := loadProfiles()
	if perr != nil {
		fmt.Fprintf(os.Stderr, "%v\n", perr)
		os.Exit(1)
	}

	client := upstream.New()
	client.Profile = profiles
	// trial 是 global 专属端点：必须开启 global realm 路由，否则 upstream.New() 的
	// GlobalEnabled 零值 false 会把请求路由到 CN base（codebuddy.cn）而必然失败。
	client.GlobalEnabled = true

	rows := collectTrialRows(files, client)

	var okN, alreadyN, notAppN, failN int

	fmt.Printf("uid                                  | nick        | status  | detail\n")
	fmt.Printf("-------------------------------------+-------------+---------+------------------------------\n")

	for _, r := range rows {
		fmt.Printf("%-36s | %-11s | %-7s | %s\n",
			trunc(r.uid, 36), trunc(r.nick, 11), r.status, r.detail)

		switch r.status {
		case trialOK:
			okN++
		case trialAlready:
			alreadyN++
		case trialNotApp:
			notAppN++
		default:
			failN++
		}
	}

	fmt.Printf("\ntotal=%d ok=%d already=%d na=%d fail=%d\n",
		len(rows), okN, alreadyN, notAppN, failN)
}

// loadProfiles 加载部署 profile。global 缺失不在启动期硬失败：CN 账号仍应
// 输出 N/A，只有真正要发请求的 global 账号才在 trialForFile 里报 FAIL。
func loadProfiles() (*clientprofile.Set, error) {
	profiles, err := clientprofile.LoadDir("data/client-profiles")
	if err != nil {
		return nil, fmt.Errorf("load client profiles: %w", err)
	}

	return profiles, nil
}

// collectTrialRows 逐个 auth 文件领取 trial，返回每账号结果行（顺序与 files 一致）。
func collectTrialRows(files []string, up *upstream.Client) []trialRow {
	rows := make([]trialRow, 0, len(files))
	for _, f := range files {
		rows = append(rows, trialForFile(f, up))
	}

	return rows
}

// trialForFile 处理单个账号：读取/解析失败记 FAIL；非 global 账号明确不适用（不发请求）；
// 其余调用 ClaimTrial 归一化结果。
func trialForFile(str string, client *upstream.Client) trialRow {
	r := trialRow{uid: filepath.Base(str)}

	raw, err := os.ReadFile(str)
	if err != nil {
		r.status, r.detail = trialFailed, "load: "+err.Error()
		return r
	}

	account, err := auth.Parse(raw)
	if err != nil {
		r.status, r.detail = trialFailed, "parse: "+err.Error()
		return r
	}

	account.FilePath = str
	r.uid, r.nick = account.UID, account.Nickname

	// 仅 global 账号适用：CN 明确提示不适用，不发任何请求。
	if !account.IsGlobal() {
		r.status, r.detail = trialNotApp, "CN account not applicable"
		return r
	}

	if client.Profile == nil || client.Profile.ForRealm("global") == nil {
		r.status, r.detail = trialFailed, errGlobalProfileMissing.Error()
		return r
	}

	r.status, r.detail = classifyTrial(client.ClaimTrial(account))

	return r
}

func trunc(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}

	return s
}
