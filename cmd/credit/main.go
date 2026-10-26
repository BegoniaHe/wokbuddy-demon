// credit.go — WorkBuddy 积分查询（全部账号 + 总计），JSON 输出到 stdout。
//
// 用法:
//
//	go run ./cmd/credit        # 或编译后 ./credit
//
// 输出结构:
//
//	{"service":"workbuddy","ts":N,
//	 "total":{"remain":N,"used":N,"size":N,"accounts":N,"ok":N,"failed":N},
//	 "accounts":[{"uid","nickname","remain","used","size","packages","ok","error?"}]}
//
// 接口与聚合逻辑：POST codebuddy.cn/v2/billing/meter/get-user-resource，聚合所有 package 的
// Cycle* 字段，TotalDosage 作 size 下限。
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
	"github.com/BegoniaHe/wokbuddy-demon/internal/logfmt"
	"github.com/BegoniaHe/wokbuddy-demon/internal/upstream"
)

const billingBaseCN = "https://www.codebuddy.cn"

// billingBaseGlobal 旧桌面端国际版计费域。global 账号打 CN 域会得到 401：
// www.codebuddy.cn 不认 workbuddy.ai 的 token（实测 401，workbuddy.ai 同 token 为 code=0）。
const billingBaseGlobal = "https://www.workbuddy.ai"

// billingBaseOfficial 官方国际站（CodeBuddy CLI externalDomain）计费域。
// 国际域两套主机名并存，token 只在签发域可用，不能统一打 workbuddy.ai。
const billingBaseOfficial = "https://www.codebuddy.ai"

var (
	errFetchUserResourceFailed = errors.New("fetchUserResource failed")
	errParseUserResourceFailed = errors.New("parseUserResource failed")
)

// billingBaseFor 按账号 realm 与 domain 选择计费域。判定口径与
// internal/auth.Realm() 一致：显式 realm=global 或 domain 落在 global 域家族
// （workbuddy.ai / codebuddy.ai）都按国际版处理；两个国际域的 token 只在签发域
// 可用，因此 codebuddy.ai 单独走官方国际站，其余 global 回落旧桌面端。
func billingBaseFor(account *auth.Auth) string {
	domain := strings.ToLower(strings.TrimSpace(account.DomainValue()))

	if isCodebuddyAIDomain(domain) {
		return billingBaseOfficial
	}

	if account.IsGlobal() {
		return billingBaseGlobal
	}

	return billingBaseCN
}

// isCodebuddyAIDomain 报告 domain 是否落在官方国际站 codebuddy.ai 家族。
func isCodebuddyAIDomain(domain string) bool {
	return domain == "codebuddy.ai" || strings.HasSuffix(domain, ".codebuddy.ai")
}

type accountResult struct {
	UID      string `json:"uid"`
	Nickname string `json:"nickname"`
	Remain   *int64 `json:"remain"`
	Used     *int64 `json:"used"`
	Size     *int64 `json:"size"`
	Packages int    `json:"packages,omitempty"`
	OK       bool   `json:"ok"`
	Error    string `json:"error,omitempty"`
}

type resourcePackage struct {
	CapacityRemain      int64 `json:"CapacityRemain"`
	CapacityUsed        int64 `json:"CapacityUsed"`
	CapacitySize        int64 `json:"CapacitySize"`
	CycleCapacityRemain int64 `json:"CycleCapacityRemain"`
	CycleCapacityUsed   int64 `json:"CycleCapacityUsed"`
	CycleCapacitySize   int64 `json:"CycleCapacitySize"`
}

// packageRemainUsed 与 billing.go:203-258 一致。
func packageRemainUsed(resourcePackage3 resourcePackage) (remain, used, size int64) {
	if resourcePackage3.CycleCapacitySize > 0 {
		return cycleRemainUsed(resourcePackage3)
	}

	remain = resourcePackage3.CapacityRemain
	used = resourcePackage3.CapacityUsed

	size = resourcePackage3.CapacitySize
	if used == 0 && size > remain {
		used = size - remain
	}

	return remain, used, size
}

// cycleRemainUsed 处理带周期（Cycle）的套餐：以 CycleCapacitySize 为上限，
// remain 夹到 [0,size]，used 取周期用量与 size-remain 的较大值。
func cycleRemainUsed(resourcePackage2 resourcePackage) (remain, used, size int64) {
	remain = resourcePackage2.CycleCapacityRemain
	size = resourcePackage2.CycleCapacitySize

	if remain < 0 {
		remain = 0
	}

	if remain > size {
		remain = size
	}

	used = max(size-remain, resourcePackage2.CycleCapacityUsed)
	if size >= used {
		remain = size - used
	}

	return remain, used, size
}

func fetchUserResource(account *auth.Auth) (remain, used, size int64, packs int, err error) {
	req, err := newUserResourceRequest(account)
	if err != nil {
		return 0, 0, 0, 0, err
	}

	client := &http.Client{Timeout: 20 * time.Second}

	resp, err := client.Do(req) //nolint:gosec // URL 由固定计费域 + 路径拼接，非请求输入
	if err != nil {
		return 0, 0, 0, 0, fmt.Errorf("fetchUserResource: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= http.StatusBadRequest {
		return 0, 0, 0, 0, fmt.Errorf("http %d: %w", resp.StatusCode, errFetchUserResourceFailed)
	}

	return parseUserResource(resp.Body)
}

// newUserResourceRequest 构造 meter/get-user-resource 请求：按 realm 选计费域与路径，
// 并带上账号身份头（X-User-Id / X-Enterprise-Id / X-Tenant-Id / X-Domain）。
func newUserResourceRequest(account *auth.Auth) (*http.Request, error) {
	now := time.Now()

	body, err := json.Marshal(map[string]any{
		"PageNumber":               1,
		"PageSize":                 100,
		"ProductCode":              "p_tcaca",
		"Status":                   []int{0, 3},
		"PackageEndTimeRangeBegin": now.Format("2006-01-02 15:04:05"),
		"PackageEndTimeRangeEnd":   now.Add(365 * 101 * 24 * time.Hour).Format("2006-01-02 15:04:05"),
	})
	if err != nil {
		return nil, fmt.Errorf("newUserResourceRequest: %w", err)
	}

	base := billingBaseFor(account)
	// global 域无 /v2 前缀（与 internal/upstream 的 billingMeterPaths 同口径）。
	path := "/v2/billing/meter/get-user-resource"
	if base != billingBaseCN {
		path = "/billing/meter/get-user-resource"
	}

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, base+path, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("newUserResourceRequest: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+account.AccessTokenValue())
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")

	if account.UID != "" {
		req.Header.Set("X-User-Id", account.UID)
	}

	if account.EnterpriseID != "" {
		req.Header.Set("X-Enterprise-Id", account.EnterpriseID)
		req.Header.Set("X-Tenant-Id", account.EnterpriseID)
	}

	if domain := account.DomainValue(); domain != "" {
		req.Header.Set("X-Domain", domain)
	}

	return req, nil
}

// parseUserResource 解析计费响应信封：聚合各 package 的 Cycle/Capacity 用量，
// 以 TotalDosage 作 size 下限，derived（size-remain）作 used 下限。
func parseUserResource(body io.Reader) (remain, used, size int64, packs int, err error) {
	var env struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			Response struct {
				Data struct {
					TotalDosage int64             `json:"TotalDosage"`
					Accounts    []resourcePackage `json:"Accounts"`
				} `json:"Data"`
			} `json:"Response"`
		} `json:"data"`
	}
	if err := json.NewDecoder(body).Decode(&env); err != nil {
		return 0, 0, 0, 0, fmt.Errorf("parseUserResource: %w", err)
	}

	if env.Code != 0 {
		return 0, 0, 0, 0, fmt.Errorf("code=%d %s: %w", env.Code, env.Msg, errParseUserResourceFailed)
	}

	for _, a := range env.Data.Response.Data.Accounts {
		r, u, s := packageRemainUsed(a)
		remain += r
		used += u
		size += s
	}

	packs = len(env.Data.Response.Data.Accounts)

	if size > 0 {
		if derived := size - remain; derived > used {
			used = derived
		}
	}

	if dosage := env.Data.Response.Data.TotalDosage; dosage > size {
		size = dosage
		if derived := size - remain; derived > used {
			used = derived
		}
	}

	return remain, used, size, packs, nil
}

func main() {
	pretty := len(os.Args) > 1 && os.Args[1] == "-pretty"

	authDir := "./auths"
	if v := os.Getenv("WB2A_AUTH_DIR"); v != "" {
		authDir = v
	}

	files, _ := filepath.Glob(filepath.Join(authDir, "workbuddy-*.json"))
	slices.Sort(files)

	client := upstream.New()
	accounts := collectAccounts(files, client)
	totalRemain, totalUsed, totalSize, okCount := summarize(accounts)

	if pretty {
		printPretty(accounts, totalRemain, totalUsed, totalSize, okCount)
		return
	}

	out := map[string]any{
		"service": "workbuddy",
		"ts":      time.Now().Unix(),
		"total": map[string]any{
			"remain":   totalRemain,
			"used":     totalUsed,
			"size":     totalSize,
			"accounts": len(accounts),
			"ok":       okCount,
			"failed":   len(accounts) - okCount,
		},
		"accounts": accounts,
	}

	raw, err := json.Marshal(out)
	if err != nil {
		fmt.Fprintln(os.Stderr, "marshal output:", err)
		os.Exit(1)
	}

	fmt.Println(string(raw))
}

// collectAccounts 逐个账号拉取余额：读取/解析失败也追加一条错误结果，保证
// total.accounts 与输入文件数一致；请求余额前按需 refresh 并原子写回。
//
//nolint:funlen // 逐文件读取/刷新/查询线性展开。
func collectAccounts(files []string, client *upstream.Client) []accountResult {
	accounts := make([]accountResult, 0, len(files))

	for _, filePath := range files {
		raw, err := os.ReadFile(filePath) //nolint:gosec // 路径来自本机 CLI 参数，非远程输入
		if err != nil {
			accounts = append(accounts, accountResult{
				UID: "", Nickname: "", Remain: nil, Used: nil, Size: nil,
				Packages: 0, OK: false, Error: "load: " + err.Error(),
			})

			continue
		}

		account, err := auth.Parse(raw)
		if err != nil {
			accounts = append(accounts, accountResult{
				UID: "", Nickname: "", Remain: nil, Used: nil, Size: nil,
				Packages: 0, OK: false, Error: "load: " + err.Error(),
			})

			continue
		}

		account.FilePath = filePath

		res := accountResult{
			UID: account.UID, Nickname: account.Nickname,
			Remain: nil, Used: nil, Size: nil, Packages: 0, OK: false, Error: "",
		}
		if refreshErr := refreshAccount(client, account); refreshErr != nil {
			res.Error = "refresh: " + refreshErr.Error()
			accounts = append(accounts, res)

			continue
		}

		remain, used, size, packs, err := fetchUserResource(account)
		if err != nil {
			res.Error = err.Error()
		} else {
			res.Remain = &remain
			res.Used = &used
			res.Size = &size
			res.Packages = packs
			res.OK = true
		}

		accounts = append(accounts, res)

		time.Sleep(200 * time.Millisecond)
	}

	return accounts
}

// refreshAccount 在 token 临期/过期时刷新，成功后原子写回；写回失败只 WARN，
// 不阻断本次余额查询（内存中的新 token 仍可用）。
//
// 没有 refresh token 时跳过刷新：手写扁平 auth 常只带 accessToken、没有
// expiresAt。NeedsRefresh 会把「无过期时间」视为需要刷新，若直接调用
// RefreshTokenIfNeeded 会以 no refresh token 失败，反而丢掉一次本可成功的余额查询
// （issue #294 要求扁平形手写文件可直接使用）。
func refreshAccount(client *upstream.Client, account *auth.Auth) error {
	if account.RefreshTokenValue() == "" {
		return nil
	}

	needed := account.NeedsRefresh(2 * time.Hour)

	if err := client.RefreshTokenIfNeeded(account, 2*time.Hour); err != nil {
		return fmt.Errorf("refreshAccount: %w", err)
	}

	// 未触发刷新时不重写文件，避免每次查询都产生无意义写盘。
	if !needed {
		return nil
	}

	if err := account.SaveAtomicRetry(); err != nil {
		//nolint:gosec // uid 经 UID8 截断，err 只含本地写盘错误。
		log.Printf("WARN: [credit] save refreshed token uid=%s: %v", logfmt.UID8(account.UID), err)
	}

	return nil
}

// summarize 汇总成功账号的余额与成功计数（failed = len(accounts) - ok）。
func summarize(accounts []accountResult) (totalRemain, totalUsed, totalSize int64, okCount int) {
	for _, item := range accounts {
		if !item.OK {
			continue
		}

		okCount++

		if item.Remain != nil {
			totalRemain += *item.Remain
		}

		if item.Used != nil {
			totalUsed += *item.Used
		}

		if item.Size != nil {
			totalSize += *item.Size
		}
	}

	return totalRemain, totalUsed, totalSize, okCount
}

// printPretty 人类可读日报：四行汇总，无账号明细。
func printPretty(accounts []accountResult, totalRemain, _, totalSize int64, _ int) {
	withBalance := 0

	var failed []string

	for _, item := range accounts {
		if item.OK && item.Remain != nil && *item.Remain > 0 {
			withBalance++
		}

		if !item.OK {
			name := item.Nickname
			if name == "" && len(item.UID) >= 8 {
				name = item.UID[:8]
			}

			failed = append(failed, name+" "+item.Error)
		}
	}

	pct := int64(0)
	if totalSize > 0 {
		pct = totalRemain * 100 / totalSize
	}

	fmt.Printf("📊 WorkBuddy 积分日报\n")
	fmt.Printf("账号: %d/%d\n", withBalance, len(accounts))
	fmt.Printf("总计: %d/%d\n", totalRemain, totalSize)
	fmt.Printf("剩余: %d%%\n", pct)

	for _, f := range failed {
		fmt.Printf("⚠️ %s\n", f)
	}
}
