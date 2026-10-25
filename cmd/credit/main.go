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
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

const billingBaseCN = "https://www.codebuddy.cn"

// billingBaseGlobal 国际版计费域。global 账号打 CN 域会得到 401：
// www.codebuddy.cn 不认 workbuddy.ai 的 token（实测 401，workbuddy.ai 同 token 为 code=0）。
const billingBaseGlobal = "https://www.workbuddy.ai"

var (
	errFetchUserResourceFailed = errors.New("fetchUserResource failed")
	errParseUserResourceFailed = errors.New("parseUserResource failed")
)

// billingBaseFor 按账号 realm 选择计费域。
//
// 判定口径与 internal/auth.Realm() 一致：显式 realm=global 或 domain 落在
// global 域家族（workbuddy.ai / codebuddy.ai），都按国际版处理。cmd/credit
// 早先对所有账号硬编码 CN 域，导致 global 账号余额查询恒返回 401（面板显示的
// 是池内缓存值，不是实时的）。
func billingBaseFor(af *authFile) string {
	if strings.EqualFold(strings.TrimSpace(af.realm()), "global") || isGlobalDomain(af.domain()) {
		return billingBaseGlobal
	}

	return billingBaseCN
}

// isGlobalDomain 与 internal/auth 的判定同口径：workbuddy.ai 与 codebuddy.ai
// 家族为国际版。不能因为显式 realm=cn 就短路 domain 判定——服务端 Realm()
// 同样是 domain 优先。
func isGlobalDomain(domain string) bool {
	d := strings.ToLower(strings.TrimSpace(domain))

	for _, root := range []string{"workbuddy.ai", "codebuddy.ai"} {
		if d == root || strings.HasSuffix(d, "."+root) {
			return true
		}
	}

	return false
}

// authFile 兼容 internal/auth 支持的两种磁盘形态：嵌套形（插件 OAuth）与
// 扁平形（手写/旧版）。扁平字段用 Flat 后缀，避免与嵌套结构同名字段冲突。
type authFile struct {
	Auth struct {
		AccessToken string `json:"accessToken"`
		Domain      string `json:"domain"`
		Realm       string `json:"realm"`
	} `json:"auth"`
	Account struct {
		UID          string `json:"uid"`
		EnterpriseID string `json:"enterpriseId"`
		Nickname     string `json:"nickname"`
	} `json:"account"`

	FlatAccessToken  string `json:"accessToken"`
	FlatDomain       string `json:"domain"`
	FlatRealm        string `json:"realm"`
	FlatUID          string `json:"uid"`
	FlatEnterpriseID string `json:"enterpriseId"`
	FlatNickname     string `json:"nickname"`
}

// accessToken/domain/realm/uid/enterpriseID/nickname 优先取嵌套形，缺失时回落
// 扁平形；与 internal/auth.Parse 的取值口径一致。
func (a *authFile) accessToken() string {
	if a.Auth.AccessToken != "" {
		return a.Auth.AccessToken
	}

	return a.FlatAccessToken
}

func (a *authFile) domain() string {
	if a.Auth.Domain != "" {
		return a.Auth.Domain
	}

	return a.FlatDomain
}

func (a *authFile) realm() string {
	if a.Auth.Realm != "" {
		return a.Auth.Realm
	}

	return a.FlatRealm
}

func (a *authFile) uid() string {
	if a.Account.UID != "" {
		return a.Account.UID
	}

	return a.FlatUID
}

func (a *authFile) enterpriseID() string {
	if a.Account.EnterpriseID != "" {
		return a.Account.EnterpriseID
	}

	return a.FlatEnterpriseID
}

func (a *authFile) nickname() string {
	if a.Account.Nickname != "" {
		return a.Account.Nickname
	}

	return a.FlatNickname
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

func fetchUserResource(af *authFile) (remain, used, size int64, packs int, err error) {
	req, err := newUserResourceRequest(af)
	if err != nil {
		return 0, 0, 0, 0, err
	}

	client := &http.Client{Timeout: 20 * time.Second}

	resp, err := client.Do(req)
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
func newUserResourceRequest(authFile2 *authFile) (*http.Request, error) {
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

	base := billingBaseFor(authFile2)
	// global 域无 /v2 前缀（与 internal/upstream 的 billingMeterPaths 同口径）。
	path := "/v2/billing/meter/get-user-resource"
	if base == billingBaseGlobal {
		path = "/billing/meter/get-user-resource"
	}

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, base+path, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("newUserResourceRequest: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+authFile2.accessToken())
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")

	if uid := authFile2.uid(); uid != "" {
		req.Header.Set("X-User-Id", uid)
	}

	if enterpriseID := authFile2.enterpriseID(); enterpriseID != "" {
		req.Header.Set("X-Enterprise-Id", enterpriseID)
		req.Header.Set("X-Tenant-Id", enterpriseID)
	}

	if domain := authFile2.domain(); domain != "" {
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

	accounts := collectAccounts(files)
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

// collectAccounts 逐个账号拉取余额：读取/解析失败静默跳过（与旧行为一致），
// 无 accessToken 记为错误，成功则填 remain/used/size/packages。每账号间隔 200ms。
func collectAccounts(files []string) []accountResult {
	accounts := make([]accountResult, 0, len(files))
	for _, f := range files {
		var authFile2 authFile

		raw, err := os.ReadFile(f) //nolint:gosec // 路径来自本机 CLI 参数，非远程输入
		if err != nil || json.Unmarshal(raw, &authFile2) != nil {
			continue
		}

		res := accountResult{
			UID:      authFile2.uid(),
			Nickname: authFile2.nickname(),
			Remain:   nil,
			Used:     nil,
			Size:     nil,
			Packages: 0,
			OK:       false,
			Error:    "",
		}
		if authFile2.accessToken() == "" {
			res.Error = "no accessToken"
			accounts = append(accounts, res)

			continue
		}

		remain, used, size, packs, err := fetchUserResource(&authFile2)
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
