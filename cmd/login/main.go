// login.go — WorkBuddy OAuth 登录（设备授权流程，CN realm；--realm=global 供国际版）。
//
// 两个子命令，由 login.sh 顺序驱动：
//
//	login [--realm=cn|global] url   → POST /v2/plugin/auth/state?platform=<workbuddy|workbuddy-ai>
//	                                  拿 state+authUrl，state 落 /tmp/wb2api-login-state.json，
//	                                  stdout 打印授权 URL
//	login [--realm=cn|global] poll  → 读 state，GET /v2/plugin/auth/token?state= 一次，
//	                                  成功再 GET /v2/plugin/login/account?state= 拿 uid/nickname，
//	                                  stdout 打印完整 token+account JSON（含 realm 键）
//
// --realm 默认 cn。按 realm 切换上游端点与 Origin/Referer：
//
//	cn     → https://copilot.tencent.com（Origin: https://www.codebuddy.cn）
//	global → https://www.workbuddy.ai（Origin: https://www.workbuddy.ai）
//
// state 落盘带 realm，poll 读回校验与命令行 --realm 一致（防混域）。
// 无 PKCE（workbuddy 设备流由服务端签发 state）。
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"os"
	"path/filepath"
	"strings"
	"time"

	auth2 "github.com/BegoniaHe/wokbuddy-demon/internal/auth"
)

// 上游常量：CN → www.workbuddy.cn（Origin 为 codebuddy.cn）；global → www.workbuddy.ai
// （base 与 Origin/Referer 同域）。端点 URL 由 realmConfig 按 realm 动态拼出，不再硬编码。
const (
	upstreamBaseCN      = "https://www.workbuddy.cn"
	upstreamBaseGlobal  = "https://www.workbuddy.ai"
	clientUA            = "CLI/2.63.2 CodeBuddy/2.63.2"
	originRefererCN     = "https://www.codebuddy.cn"
	originRefererGlobal = "https://www.workbuddy.ai"
)

// 登录 state 落盘路径（var 便于测试替换临时文件）
// Portable across OSes: the upstream hardcoded "/tmp/...", which on
// Windows resolves to <drive>:\tmp\... and aborts the OAuth flow with
// "The system cannot find the path specified". os.TempDir() is /tmp on Linux.
var stateFile = filepath.Join(os.TempDir(), "wb2api-login-state.json")

// exitFunc 供测试替换（默认 os.Exit；测试持临时替换为 panic 以进程内捕获 fatal）。
var exitFunc = os.Exit

var (
	errDoJSONFailed             = errors.New("doJSON failed")
	errRealmRequiresAValue      = errors.New("--realm requires a value")
	errParseRealmArgsFailed     = errors.New("parseRealmArgs failed")
	errValidateRealmMatchFailed = errors.New("validateRealmMatch failed")
	errLoginPending             = errors.New("登录未完成（waiting for login）。请确认已在浏览器完成登录再按 y")
)

// realmConfig 按 realm 返回上游 base 与 Origin/Referer origin：global →
// (www.workbuddy.ai, www.workbuddy.ai)；cn/非法/缺省 → (www.workbuddy.cn, codebuddy.cn)。
func realmConfig(realm string) (base, origin string) {
	if realm == realmGlobal {
		return upstreamBaseGlobal, originRefererGlobal
	}

	return upstreamBaseCN, originRefererCN
}

// realmBases 返回该 realm 的 origin 候选顺序。CN 默认主域 + 兜底；显式自定义
// base（测试/自建）只返回该 base。global 单 base。
func realmBases(realm, base string) []string {
	if realm == realmGlobal || base != upstreamBaseCN {
		return []string{base}
	}

	return []string{upstreamBaseCN, "https://copilot.tencent.com", "https://wb.tencentbuddy.com"}
}

// authPlatform 按 realm 返回 auth/state 的 platform 查询参数值。官方桌面端取
// cli/product.json 的 authentication.attributes.platform：cn → "workbuddy"、
// global → "workbuddy-ai"（旧值 "CLI" 与官方不符）。
func authPlatform(realm string) string {
	if realm == realmGlobal {
		return "workbuddy-ai"
	}

	return "workbuddy"
}

// commonHeaders 按 origin 设置通用请求头（Origin/Referer 随 realm 变化）。
// 返回 func(*http.Request)，由调用方按 realm 选定的 origin 构造一次后复用。
func commonHeaders(origin string) func(*http.Request) {
	return func(req *http.Request) {
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/plain, */*")
		req.Header.Set("X-Requested-With", "XMLHttpRequest")
		req.Header.Set("Origin", origin)
		req.Header.Set("Referer", origin+"/")
		req.Header.Set("User-Agent", clientUA)
	}
}

// noAuthHeaders 在 base 头组上叠加设备授权未登录态声明头（官方 fetchAuthState/
// loopGetToken 同款）：显式声明无凭证、无用户/企业/部门上下文。
func noAuthHeaders(base func(*http.Request)) func(*http.Request) {
	return func(req *http.Request) {
		base(req)
		req.Header.Set("X-No-Authorization", "true")
		req.Header.Set("X-No-User-Id", "true")
		req.Header.Set("X-No-Enterprise-Id", "true")
		req.Header.Set("X-No-Department-Info", "true")
	}
}

// apiEnvelope 与 main.go:429-433 一致。
type apiEnvelope struct {
	Code int             `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

// doJSON 与 oauth.go:33-66 一致：{code,msg,data} 信封，code!=0 → error。
func doJSON(client *http.Client, method, fullURL string, headers func(*http.Request), body io.Reader) (json.RawMessage, int, error) {
	req, err := http.NewRequestWithContext(context.Background(), method, fullURL, body)
	if err != nil {
		return nil, 0, fmt.Errorf("doJSON: %w", err)
	}

	if headers != nil {
		headers(req)
	} else {
		// 缺省头：CN origin（与原 commonHeaders() 行为一致，零回归）
		commonHeaders(originRefererCN)(req)
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("doJSON: %w", err)
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, resp.StatusCode, fmt.Errorf("http_error: upstream %d: %w", resp.StatusCode, errDoJSONFailed)
	}

	if resp.StatusCode >= 300 {
		return nil, resp.StatusCode, fmt.Errorf("http_error: upstream redirect %d: %w", resp.StatusCode, errDoJSONFailed)
	}

	var env apiEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, resp.StatusCode, fmt.Errorf("parse failed: %w", err)
	}

	if env.Code != 0 {
		return nil, resp.StatusCode, fmt.Errorf("code=%d msg=%s: %w", env.Code, env.Msg, errDoJSONFailed)
	}

	return env.Data, resp.StatusCode, nil
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "login: "+format+"\n", args...)
	exitFunc(1)
}

type loginState struct {
	State string `json:"state"`
	Realm string `json:"realm,omitempty"` // url 落盘时写回的 realm，poll 读回校验防混域
}

// tokenBundle 是 auth/token 成功返回的凭证包。
type tokenBundle struct {
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
	ExpiresIn    int64  `json:"expiresIn"`
	Domain       string `json:"domain"`
}

// accountInfo 是 login/account 返回的账号标识。
type accountInfo struct {
	UID          string `json:"uid"`
	EnterpriseID string `json:"enterpriseId"`
	Nickname     string `json:"nickname"`
}

// realm 取值枚举（与 internal/auth 的 Realm() 归一化输出一致）。
const (
	realmCN     = "cn"
	realmGlobal = "global"
)

// parseRealmArgs 解析开头的 --realm=cn|global（或分离式 --realm <v>）flag，缺省 cn。
// 大小写不敏感归一化；非法值/缺值报错。桌椅剩余参数（子命令）顺序不变。
func parseRealmArgs(args []string) (realm string, rest []string, err error) {
	realm = realmCN

	for i := 0; i < len(args); i++ {
		str := args[i]
		switch {
		case str == "--realm":
			if i+1 >= len(args) {
				return "", nil, errRealmRequiresAValue
			}

			realmValue := strings.ToLower(strings.TrimSpace(args[i+1])) //nolint:gosec // 上方已校验 i+1 < len(args)
			if realmValue != realmCN && realmValue != realmGlobal {
				return "", nil, fmt.Errorf(
					"invalid --realm %q (want cn|global): %w",
					args[i+1], //nolint:gosec // 上方已校验 i+1 < len(args)
					errParseRealmArgsFailed,
				)
			}

			realm = realmValue
			i++
		case strings.HasPrefix(str, "--realm="):
			v := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(str, "--realm=")))
			if v != realmCN && v != realmGlobal {
				return "", nil, fmt.Errorf("invalid --realm %q (want cn|global): %w", v, errParseRealmArgsFailed)
			}

			realm = v
		default:
			rest = append(rest, str)
		}
	}

	return realm, rest, nil
}

// resolveRealmInput 把交互式选域的一行输入归一化为 realm（纯函数，login.sh 交互分支
// 的核心决策，可测）。规则：
//
//	"1"/"cn"（大小写不敏感）/""（回车默认）→ cn
//	"2"/"global" → global
//	其他 → ("", false)（调用方回默认 cn）
func resolveRealmInput(input string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(input)) {
	case "", "1", "cn":
		return realmCN, true
	case "2", "global":
		return realmGlobal, true
	}

	return "", false
}

// promptRealm 交互式选域：向 out 打印选项提示（out 接 stderr，stdout 留给 realm 本身），
// 从 in 读一行，返回归一化 realm。非法输入警告后回落 cn；EOF（非交互/管道）回落 cn。
func promptRealm(in io.Reader, out io.Writer) string {
	fmt.Fprintln(out, "选择登录版本: 1) 国内版(cn) 2) 国际版(global) [默认 1/cn]: ")

	line, err := bufio.NewReader(in).ReadString('\n')
	if err != nil && line == "" {
		// EOF/非交互 → 回落默认 cn
		return realmCN
	}

	if realm, ok := resolveRealmInput(line); ok {
		return realm
	}

	fmt.Fprintln(out, "无效选择，默认国内版 cn")

	return realmCN
}

// validateRealmMatch 校验 state 文件 realm 与命令行 --realm 一致（防混域）：
// state 无 realm（旧文件）放行；非空且不一致 → error。
func validateRealmMatch(stateRealm, cliRealm string) error {
	if stateRealm != "" && stateRealm != cliRealm {
		return fmt.Errorf(
			"realm mismatch: state file realm=%q, command --realm=%q（url 与 poll 需同一 realm）: %w",
			stateRealm,
			cliRealm,
			errValidateRealmMatchFailed,
		)
	}

	return nil
}

// runURL 执行 url 子命令：向 upstreamBase 的 state 端点 POST 取授权 URL，
// state 落盘（带 realm），stdout 打印 authURL。out 接 stdout；stateFile 为落盘路径
// （可注入临时文件便于测试）。空 realm 视为缺省（调用方已归一）。
func runURL(base, origin, realm, statePath string, client *http.Client, out io.Writer) {
	headers := noAuthHeaders(commonHeaders(origin))

	var data []byte

	var lastErr error

	for _, candidate := range realmBases(realm, base) {
		url := candidate + "/v2/plugin/auth/state?platform=" + authPlatform(realm)

		var err error

		data, _, err = doJSON(client, http.MethodPost, url, headers, bytes.NewReader([]byte("{}")))
		if err == nil {
			lastErr = nil
			break
		}

		lastErr = err
	}

	if lastErr != nil {
		fatalf("auth state failed: %v", lastErr)
	}

	var item struct {
		State   string `json:"state"`
		AuthURL string `json:"authUrl"`
	}
	if err := json.Unmarshal(data, &item); err != nil || item.State == "" || item.AuthURL == "" {
		fatalf("auth state: missing state or authUrl")
	}

	raw, merr := json.Marshal(loginState{State: item.State, Realm: realm})
	if merr != nil {
		fatalf("marshal state: %v", merr)
	}

	if err := os.WriteFile(statePath, raw, 0o600); err != nil {
		fatalf("write state: %v", err)
	}

	fmt.Fprintln(out, item.AuthURL)
}

// runPoll 执行 poll 子命令：读 state 文件（realm 校验），向 upstreamBase 的 token 端点
// GET 一次，成功再 GET login/account（带 Bearer），stdout 打印完整 token+account JSON。
// statePath 可注入临时文件便于测试。
func runPoll(base, origin, realm, statePath string, client *http.Client, out io.Writer) {
	loginState2 := loadLoginState(statePath, realm)
	headers := commonHeaders(origin)
	// handlePollLogin (oauth.go:108-162)：auth/token 是权威登录状态端点，
	// pending 时业务 code 非 0（"login ing"），完成时 code=0 + token bundle
	tok, usedBase, err := pollTokenWithFallback(base, realm, loginState2.State, headers, client)
	if err != nil {
		fatalf("%v", err)
	}
	// login/account 拿 uid/nickname（带 Bearer）
	acct := pollAccount(usedBase, loginState2.State, tok.AccessToken, headers, client)

	oraw, oerr := json.Marshal(buildLoginOutput(tok, realm, acct))
	if oerr != nil {
		fatalf("marshal output: %v", oerr)
	}

	fmt.Fprintln(out, string(oraw))
	os.Remove(statePath)
}

// pollTokenWithFallback 按 origin 候选拉取 token，仅在传输/网关错误时切换。
func pollTokenWithFallback(base, realm, state string, headers func(*http.Request), client *http.Client) (tokenBundle, string, error) {
	var lastErr error

	for _, candidate := range realmBases(realm, base) {
		tok, status, err := pollTokenOnce(candidate, state, noAuthHeaders(headers), client)
		if err == nil {
			return tok, candidate, nil
		}

		lastErr = err

		if status != 0 && status < 500 && status != http.StatusRequestTimeout &&
			status != http.StatusBadGateway && status != http.StatusServiceUnavailable &&
			status != http.StatusGatewayTimeout {
			break
		}
	}

	return tokenBundle{}, "", lastErr
}

// loadLoginState 读取并解析 state 文件，校验 realm 与命令行 --realm 一致（防混域），
// 任一步失败即 fatal（url 与 poll 必须同域）。
func loadLoginState(statePath, realm string) loginState {
	raw, err := os.ReadFile(statePath)
	if err != nil {
		fatalf("read state: %v (先跑 login url)", err)
	}

	var loginState2 loginState
	if err := json.Unmarshal(raw, &loginState2); err != nil {
		fatalf("parse state: %v", err)
	}

	if err := validateRealmMatch(loginState2.Realm, realm); err != nil {
		fatalf("%v", err)
	}

	return loginState2
}

// pollTokenOnce 单 origin 拉取 token；transient 错误返回给调用方决定是否换 origin。
func pollTokenOnce(base, state string, headers func(*http.Request), client *http.Client) (tokenBundle, int, error) {
	tokRaw, status, errTok := doJSON(client, http.MethodGet, base+"/v2/plugin/auth/token?state="+state, headers, nil)
	if errTok != nil {
		if status == 0 || status >= 500 {
			return tokenBundle{}, status, fmt.Errorf("token endpoint error: %w", errTok)
		}

		return tokenBundle{}, status, fmt.Errorf("%w: %w", errLoginPending, errTok)
	}

	var tok tokenBundle
	if err := json.Unmarshal(tokRaw, &tok); err != nil || tok.AccessToken == "" {
		return tokenBundle{}, status, errLoginPending
	}

	return tok, status, nil
}

// pollAccount 带 Bearer 拉取 login/account；失败时返回零值（与旧行为一致，不中断输出）。
func pollAccount(base, state, accessToken string, headers func(*http.Request), client *http.Client) accountInfo {
	var acct accountInfo

	acctHeaders := func(r *http.Request) {
		headers(r)
		r.Header.Set("Authorization", "Bearer "+accessToken)
		// 已带 Bearer：声明无用户/企业/部门上下文（官方 loopGetAccount 头组，不含 X-No-Authorization）。
		r.Header.Set("X-No-User-Id", "true")
		r.Header.Set("X-No-Enterprise-Id", "true")
		r.Header.Set("X-No-Department-Info", "true")
	}
	if acctRaw, _, errAcct := doJSON(
		client,
		http.MethodGet,
		base+"/v2/plugin/login/account?state="+state,
		acctHeaders,
		nil,
	); errAcct == nil {
		_ = json.Unmarshal(acctRaw, &acct)
	}

	return acct
}

// buildLoginOutput 组装 poll 输出的完整 JSON（login.sh 据此落盘 auth 文件）。
// realm 永不空：显式 --realm 优先（ResolveRealm 处理），否则按上游返回的 domain 推断——
// 保证登录落盘的 auth 文件恒带 realm 键。
func buildLoginOutput(tok tokenBundle, realm string, acct accountInfo) map[string]any {
	return map[string]any{
		"access_token":  tok.AccessToken,
		"refresh_token": tok.RefreshToken,
		"expires_in":    tok.ExpiresIn,
		"domain":        tok.Domain,
		"realm":         auth2.ResolveRealm(realm, tok.Domain),
		"uid":           acct.UID,
		"enterprise_id": acct.EnterpriseID,
		"nickname":      acct.Nickname,
	}
}

func main() {
	realm, rest, err := parseRealmArgs(os.Args[1:])
	if err != nil {
		fatalf("%v (usage: login [--realm=cn|global] <url|poll|realm>)", err)
	}

	if len(rest) < 1 {
		fatalf("usage: login [--realm=cn|global] <url|poll>")
	}
	// 每个流程独立 cookie jar（oauth.go:22-29：多账号登录互不串会话）
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Timeout: 30 * time.Second, Jar: jar}

	base, origin := realmConfig(realm)

	switch rest[0] {
	case "url":
		runURL(base, origin, realm, stateFile, client, os.Stdout)

	case "poll":
		runPoll(base, origin, realm, stateFile, client, os.Stdout)

	case "realm":
		// 交互式选域（login.sh 无 --realm 传参且 stdin 为 tty 时调用）。
		// 提示打到 stderr，stdout 只输出归一化 realm，供 $( ) 捕获。
		realm := promptRealm(os.Stdin, os.Stderr)
		fmt.Println(realm)

	default:
		fatalf("unknown subcommand %q (want url|poll|realm)", rest[0])
	}
}
