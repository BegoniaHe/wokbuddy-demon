// login.go — WorkBuddy OAuth 登录（设备授权流程，CN realm；--realm=global 供国际版）。
//
// 两个子命令，由 login.sh 顺序驱动：
//
//	login [--realm=cn|global] url   → POST /v2/plugin/auth/state?platform=<workbuddy|workbuddy-ai>
//	                                  同时带部署 profile 的 version；CN 站另外现场生成 loginSessionId
//	                                  （官方 appendHostLoginJourneyParams 只在 platform=workbuddy 时追加）
//	                                  → 拿 state+authUrl，state 落 /tmp/wb2api-login-state.json，
//	                                  stdout 打印授权 URL
//	login [--realm=cn|global] poll  → 读 state，GET /v2/plugin/auth/token?state= 一次，
//	                                  成功再 GET /v2/plugin/login/account?state= 拿 uid/nickname，
//	                                  stdout 打印完整 token+account JSON（含 realm 键）
//
// --realm 默认 cn。按 realm 切换上游端点与 Origin/Referer：
//
//	cn     → https://www.workbuddy.cn（Origin: https://www.codebuddy.cn），
//	         首 token 前可回落 copilot.tencent.com / wb.tencentbuddy.com
//	global → https://www.workbuddy.ai（Origin: https://www.workbuddy.ai）
//
// state 落盘带 realm，poll 读回校验与命令行 --realm 一致（防混域）。
// 无 PKCE（workbuddy 设备流由服务端签发 state）。
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	auth2 "github.com/BegoniaHe/wokbuddy-demon/internal/auth"
	"github.com/BegoniaHe/wokbuddy-demon/internal/clientprofile"
	"github.com/BegoniaHe/wokbuddy-demon/internal/jsonx"
	"github.com/BegoniaHe/wokbuddy-demon/internal/logincode"
	"github.com/BegoniaHe/wokbuddy-demon/internal/outbound"
	"github.com/BegoniaHe/wokbuddy-demon/internal/safefile"
	"github.com/BegoniaHe/wokbuddy-demon/internal/session"
)

// loginStateURL 拼 auth/state 地址：platform 恒有；version 来自部署 profile（空则不补）；
// loginSessionId 只在 CN 站（platform=workbuddy）追加——与官方
// appendHostLoginJourneyParams 的 platform!=='workbuddy' → return 同口径。
func loginStateURL(base string, identity loginIdentity) string {
	query := url.Values{}
	query.Set("platform", identity.platform)

	if identity.version != "" {
		query.Set("version", identity.version)
	}

	if identity.platform == "workbuddy" && identity.sessionID != "" {
		query.Set("loginSessionId", identity.sessionID)
	}

	return base + "/v2/plugin/auth/state?" + query.Encode()
}

// 上游常量：CN → www.workbuddy.cn（Origin 为 codebuddy.cn）；global → www.workbuddy.ai
// （base 与 Origin/Referer 同域）。端点 URL 由 realmConfig 按 realm 动态拼出，不再硬编码。
const (
	upstreamBaseCN      = "https://www.workbuddy.cn"
	upstreamBaseGlobal  = "https://www.workbuddy.ai"
	originRefererCN     = "https://www.codebuddy.cn"
	originRefererGlobal = "https://www.workbuddy.ai"
	// loginProductSaaS 是官方 REST 拦截器在请求未自带 X-Product 时补的
	// deploymentType。两版 cli/product.json 都是 SaaS；本仓不走那层客户端。
	loginProductSaaS = "SaaS"
)

// 登录 state 落盘路径（var 便于测试替换临时文件）
// Portable across OSes: the upstream hardcoded "/tmp/...", which on
// Windows resolves to <drive>:\tmp\... and aborts the OAuth flow with
// "The system cannot find the path specified". os.TempDir() is /tmp on Linux.
var stateFile = filepath.Join(os.TempDir(), "wb2api-login-state.json")

// loginConfigPath CLI 读取登录出口配置的路径（与 server 同目录同文件）；
// var 便于测试替换临时文件。
var loginConfigPath = "config.json"

// exitFunc 供测试替换（默认 os.Exit；测试持临时替换为 panic 以进程内捕获 fatal）。
var exitFunc = os.Exit

var (
	errDoJSONFailed             = errors.New("doJSON failed")
	errRealmRequiresAValue      = errors.New("--realm requires a value")
	errParseRealmArgsFailed     = errors.New("parseRealmArgs failed")
	errValidateRealmMatchFailed = errors.New("validateRealmMatch failed")
	errLoginPending             = errors.New("登录未完成（waiting for login）。请确认已在浏览器完成登录再按 y")
	errDoJSONHeadersRequired    = errors.New("doJSON: headers 不能为空")
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

// loginIdentity 设备授权链路的客户端身份：UA、auth/state 的 platform、授权页读取的
// 客户端版本与会话标识。
type loginIdentity struct {
	userAgent string
	platform  string
	// version 授权页读取的客户端版本，来自部署 profile（仓库不内置版本常量）；空则不补。
	version string
	// sessionID 授权页的 loginSessionId：官方只对 CN 站（platform=workbuddy）现场生成。
	sessionID string
}

// loginIdentityFor 取部署 profile 的 loginUA / loginPlatform / version。profile 或
// loginUA 缺失时 userAgent 为空，由调用方拒绝登录（仓库不内置任何登录 UA）。
func loginIdentityFor(profiles *clientprofile.Set, realm string) loginIdentity {
	identity := loginIdentity{platform: authPlatform(realm)}
	if identity.platform == "workbuddy" {
		identity.sessionID = session.NewUUIDv4()
	}

	if profiles == nil {
		return identity
	}

	profile := profiles.ForRealm(realm)
	if profile == nil {
		return identity
	}

	if profile.Emit.LoginUA != nil {
		identity.userAgent = *profile.Emit.LoginUA
	}

	if profile.Emit.LoginPlatform != "" {
		identity.platform = profile.Emit.LoginPlatform
	}

	// 授权页把 version 编进 console/auth/risk-context 与 signin-callback 的 redirect_uri。
	identity.version = profile.Source.Version

	return identity
}

// mustLoginIdentity 加载部署 profile 并取登录身份；profile 或 loginUA 缺失时直接
// 退出（仓库不内置登录 UA）。仅在 url/poll 网络流程调用，realm 选域不依赖 profile。
func mustLoginIdentity(realm string) loginIdentity {
	profiles, err := clientprofile.LoadDir("data/client-profiles")
	if err != nil {
		fatalf("load client profiles: %v", err)
	}

	identity := loginIdentityFor(profiles, realm)
	if identity.userAgent == "" {
		fatalf("缺少 data/client-profiles/%s.json 的 loginUA：请用 reverse-workbuddy 技能重新生成，仓库不内置登录 UA", realm)
	}

	return identity
}

// commonHeaders 按 origin 设置通用请求头（Origin/Referer 随 realm 变化）。
// 返回 func(*http.Request)，由调用方按 realm 选定的 origin 构造一次后复用。
func commonHeaders(origin, userAgent string) func(*http.Request) {
	return func(req *http.Request) {
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/plain, */*")
		req.Header.Set("X-Requested-With", "XMLHttpRequest")
		req.Header.Set("Origin", origin)
		req.Header.Set("Referer", origin+"/")
		req.Header.Set("User-Agent", userAgent)
		req.Header.Set("X-Product", loginProductSaaS)
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
	Code int            `json:"code"`
	Msg  string         `json:"msg"`
	Data jsontext.Value `json:"data"`
}

// doJSON 与 oauth.go:33-66 一致：{code,msg,data} 信封，code!=0 → error。
func doJSON(
	client *http.Client, method, fullURL string, headers func(*http.Request), body io.Reader,
) (jsontext.Value, int, error) {
	req, err := http.NewRequestWithContext(context.Background(), method, fullURL, body)
	if err != nil {
		return nil, 0, fmt.Errorf("doJSON: %w", err)
	}

	if headers == nil {
		return nil, 0, errDoJSONHeadersRequired
	}

	headers(req)

	//nolint:gosec,nolintlint // G704 在 v2.14.0 报、v2.15.0 不报；URL 由固定端点拼接
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("doJSON: %w", err)
	}
	defer resp.Body.Close()

	// 读错误必须上报：连接中断时半截 body 会让 Unmarshal 失败，最终报成
	// 「parse failed」把网络问题误报成格式问题。与 internal/upstream 同口径加上限。
	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if readErr != nil {
		return nil, resp.StatusCode, fmt.Errorf("doJSON: read body: %w", readErr)
	}

	// 先解信封：业务错误可能随任意 HTTP 状态返回（含 4xx），code 比状态码更权威。
	var env apiEnvelope

	parseErr := jsonx.Unmarshal(raw, &env)
	if parseErr == nil && env.Code != 0 {
		return nil, resp.StatusCode, &logincode.Error{Code: env.Code, Msg: env.Msg, Status: resp.StatusCode}
	}

	if resp.StatusCode >= http.StatusBadRequest {
		return nil, resp.StatusCode, fmt.Errorf("http_error: upstream %d: %w", resp.StatusCode, errDoJSONFailed)
	}

	if resp.StatusCode >= http.StatusMultipleChoices {
		return nil, resp.StatusCode, fmt.Errorf(
			"http_error: upstream redirect %d: %w", resp.StatusCode, errDoJSONFailed,
		)
	}

	if parseErr != nil {
		return nil, resp.StatusCode, fmt.Errorf("parse failed: %w", parseErr)
	}

	return env.Data, resp.StatusCode, nil
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "login: "+format+"\n", args...)
	exitFunc(1)
}

type loginState struct {
	State string `json:"state"`
	Realm string `json:"realm,omitempty,omitzero"` // url 落盘时写回的 realm，poll 读回校验防混域
	Base  string `json:"base,omitempty,omitzero"`  // url 命中 origin，poll 优先同源
	Slot  string `json:"slot,omitempty,omitzero"`  // 发起时选定的出口槽：poll 沿用，不重新选举
}

// tokenBundle 是 auth/token 成功返回的凭证包。
type tokenBundle struct {
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
	ExpiresIn    int64  `json:"expiresIn"`
	Domain       string `json:"domain"`
}

// accountInfo 是 login/account 返回的账号标识。
// Type 是账号版本（personal / ultimate / exclusive）：官方与企业 ID 并列用它判定
// 企业版，落盘 auth 必须保留，否则后续出站头会误判为个人号。
type accountInfo struct {
	UID          string `json:"uid"`
	EnterpriseID string `json:"enterpriseId"`
	Type         string `json:"type"`
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
//
//nolint:funlen // URL/poll 候选轮转线性展开。
func runURL(
	base, origin, realm, statePath string, identity loginIdentity, client *http.Client, slot string,
	out io.Writer,
) {
	headers := noAuthHeaders(commonHeaders(origin, identity.userAgent))

	var (
		data     []byte
		lastErr  error
		usedBase string
	)

	for _, candidate := range realmBases(realm, base) {
		stateURL := loginStateURL(candidate, identity)

		var (
			status int
			err    error
		)

		data, status, err = doJSON(client, http.MethodPost, stateURL, headers, bytes.NewReader([]byte("{}")))
		if err == nil {
			lastErr = nil
			usedBase = candidate

			break
		}

		lastErr = err

		if !loginReplayableStatus(status) {
			break
		}
	}

	if lastErr != nil {
		fatalf("auth state failed: %v", lastErr)
	}

	var item struct {
		State   string `json:"state"`
		AuthURL string `json:"authUrl"`
	}
	if err := jsonx.Unmarshal(data, &item); err != nil || item.State == "" || item.AuthURL == "" {
		fatalf("auth state: missing state or authUrl")
	}

	raw, merr := jsonx.Marshal(loginState{State: item.State, Realm: realm, Base: usedBase, Slot: slot})
	if merr != nil {
		fatalf("marshal state: %v", merr)
	}

	// 走 safefile（唯一临时名 + rename）：os.WriteFile 会跟随并截断预先放置的
	// 符号链接，共享临时目录在多用户主机上可被用来覆盖他人文件；rename 替换的
	// 是链接本身，不会落到目标路径。
	if err := safefile.Write(statePath, raw, 0o600); err != nil {
		fatalf("write state: %v", err)
	}

	fmt.Fprintln(out, item.AuthURL)
}

// runPoll 执行 poll 子命令：读 state 文件（realm 校验），向 upstreamBase 的 token 端点
// GET 一次，成功再 GET login/account（带 Bearer），stdout 打印完整 token+account JSON。
// statePath 可注入临时文件便于测试。
func runPoll(
	base, origin, realm, statePath string, identity loginIdentity, client *http.Client, slot string,
	out io.Writer,
) {
	loginState2 := loadLoginState(statePath, realm)
	headers := commonHeaders(origin, identity.userAgent)
	// handlePollLogin (oauth.go:108-162)：auth/token 是权威登录状态端点，
	// pending 时业务 code 非 0（"login ing"），完成时 code=0 + token bundle
	tok, usedBase, err := pollTokenWithFallback(base, realm, loginState2.Base, loginState2.State, headers, client)
	if err != nil {
		fatalf("%v", err)
	}
	// login/account 拿 uid/nickname（带 Bearer）：终态立即失败并显示上游原因，
	// 12151 提示仍在等待账号信息，其他错误保持忽略（与旧行为一致）。
	acct, err := pollAccount(usedBase, loginState2.State, tok.AccessToken, headers, client)
	if err != nil {
		failOnPollAccountError(err)
	}

	oraw, oerr := jsonx.Marshal(buildLoginOutput(tok, realm, acct, slot))
	if oerr != nil {
		fatalf("marshal output: %v", oerr)
	}

	fmt.Fprintln(out, string(oraw))
	os.Remove(statePath)
}

// failOnPollAccountError 处理 login/account 失败：12151 提示 pending 后退出，
// 许可终态立即 fatal；信封内的其他业务码保持忽略（与旧行为一致）。
//
// 非信封错误（传输中断、JSON 解析失败）同样 fatal：此前这条路径静默通过，于是
// stdout 打出 uid 为空的 token、state 文件被删、退出码 0——用户既拿不到账号信息，
// 也无法用原 state 重试 poll，只能重跑整个 login url。
func failOnPollAccountError(err error) {
	codeErr, ok := errors.AsType[*logincode.Error](err)
	if !ok {
		fatalf("拉取登录账号信息失败: %v", err)
	}

	if logincode.IsPending(codeErr.Code) {
		fatalf("登录账号信息尚未就绪（code=%d），请稍后重新运行 login poll", codeErr.Code)
	}

	if logincode.IsTerminal(codeErr.Code) {
		fatalf("%v", codeErr)
	}
}

// pollTokenWithFallback 按 origin 候选拉取 token，仅在传输/网关错误时切换；
// preferredBase（url 阶段命中的 origin）优先。
func pollTokenWithFallback(
	base, realm, preferredBase, state string, headers func(*http.Request), client *http.Client,
) (tokenBundle, string, error) {
	var lastErr error

	for _, candidate := range preferBase(realmBases(realm, base), preferredBase) {
		tok, status, err := pollTokenOnce(candidate, state, noAuthHeaders(headers), client)
		if err == nil {
			return tok, candidate, nil
		}

		lastErr = err

		// 许可终态与 origin 无关：换域只会重复同一条失败，不再回落。
		if codeErr, ok := errors.AsType[*logincode.Error](err); ok && logincode.IsTerminal(codeErr.Code) {
			break
		}

		if !loginReplayableStatus(status) {
			break
		}
	}

	return tokenBundle{}, "", lastErr
}

// loginReplayableStatus 报告登录请求失败是否值得换 origin：仅传输错误与网关类状态码。
func loginReplayableStatus(status int) bool {
	switch status {
	case 0, http.StatusRequestTimeout, http.StatusBadGateway,
		http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	default:
		return false
	}
}

// preferBase 把 preferred 提到候选首位（不存在时保持原序）。
func preferBase(candidates []string, preferred string) []string {
	if preferred == "" {
		return candidates
	}

	for i := range candidates {
		if candidates[i] == preferred {
			candidates[0], candidates[i] = candidates[i], candidates[0]

			break
		}
	}

	return candidates
}

// loadLoginState 读取并解析 state 文件，校验 realm 与命令行 --realm 一致（防混域），
// 任一步失败即 fatal（url 与 poll 必须同域）。
func loadLoginState(statePath, realm string) loginState {
	raw, err := os.ReadFile(statePath)
	if err != nil {
		fatalf("read state: %v (先跑 login url)", err)
	}

	var loginState2 loginState
	if err := jsonx.Unmarshal(raw, &loginState2); err != nil {
		fatalf("parse state: %v", err)
	}

	if err := validateRealmMatch(loginState2.Realm, realm); err != nil {
		fatalf("%v", err)
	}

	return loginState2
}

// pollTokenOnce 单 origin 拉取 token；transient 错误返回给调用方决定是否换 origin。
func pollTokenOnce(
	base, state string, headers func(*http.Request), client *http.Client,
) (tokenBundle, int, error) {
	tokRaw, status, errTok := doJSON(
		client, http.MethodGet, base+"/v2/plugin/auth/token?state="+url.QueryEscape(state), headers, nil,
	)
	if errTok != nil {
		return tokenBundle{}, status, wrapTokenPollError(status, errTok)
	}

	var tok tokenBundle
	if err := jsonx.Unmarshal(tokRaw, &tok); err != nil || tok.AccessToken == "" {
		return tokenBundle{}, status, errLoginPending
	}

	return tok, status, nil
}

// wrapTokenPollError 按业务码与 HTTP 状态归类 token 轮询错误：pending 包装为
// errLoginPending，许可终态原样返回，其余沿用传输/HTTP 语义。
func wrapTokenPollError(status int, err error) error {
	if codeErr, ok := errors.AsType[*logincode.Error](err); ok {
		if logincode.IsPending(codeErr.Code) {
			return fmt.Errorf("%w: %w", errLoginPending, err)
		}

		if logincode.IsTerminal(codeErr.Code) {
			return err
		}
	}

	if status == 0 || status >= 500 {
		return fmt.Errorf("token endpoint error: %w", err)
	}

	// 408/429 是上游超时/限流，不代表 state 失效：保留可重试语义，避免用户
	// 在限流窗口内重跑 login url 丢弃仍有效的 state。
	if status == http.StatusRequestTimeout || status == http.StatusTooManyRequests {
		return fmt.Errorf("token endpoint temporarily unavailable (HTTP %d): %w", status, err)
	}

	// 其余非信封 4xx 不是「还没登录完」：state 很可能已失效，继续轮询永远不会
	// 成功。明确透出 HTTP 状态与上游错误，并引导重新发起登录。
	if status >= http.StatusBadRequest {
		return fmt.Errorf(
			"登录状态查询被拒（HTTP %d）：%w；state 可能已过期，请重新运行 login url",
			status, err,
		)
	}

	return fmt.Errorf("%w: %w", errLoginPending, err)
}

// pollAccount 带 Bearer 拉取 login/account；失败时返回零值与错误（调用方决定
// pending / 终态 / 忽略）。
func pollAccount(
	base, state, accessToken string, headers func(*http.Request), client *http.Client,
) (accountInfo, error) {
	var acct accountInfo

	acctHeaders := func(r *http.Request) {
		headers(r)
		r.Header.Set("Authorization", "Bearer "+accessToken)
		// 已带 Bearer：声明无用户/企业/部门上下文（官方 loopGetAccount 头组，不含 X-No-Authorization）。
		r.Header.Set("X-No-User-Id", "true")
		r.Header.Set("X-No-Enterprise-Id", "true")
		r.Header.Set("X-No-Department-Info", "true")
	}

	acctRaw, _, errAcct := doJSON(
		client,
		http.MethodGet,
		base+"/v2/plugin/login/account?state="+url.QueryEscape(state),
		acctHeaders,
		nil,
	)
	if errAcct != nil {
		return accountInfo{}, errAcct
	}

	if err := jsonx.Unmarshal(acctRaw, &acct); err != nil {
		return accountInfo{}, fmt.Errorf("parse account: %w", err)
	}

	return acct, nil
}

// buildLoginOutput 组装 poll 输出的完整 JSON（login.sh 据此落盘 auth 文件）。
// realm 永不空：显式 --realm 优先（ResolveRealm 处理），否则按上游返回的 domain 推断——
// 保证登录落盘的 auth 文件恒带 realm 键。
func buildLoginOutput(tok tokenBundle, realm string, acct accountInfo, slot string) map[string]any {
	out := map[string]any{
		"access_token":  tok.AccessToken,
		"refresh_token": tok.RefreshToken,
		"expires_in":    tok.ExpiresIn,
		"domain":        tok.Domain,
		"realm":         auth2.ResolveRealm(realm, tok.Domain),
		"uid":           acct.UID,
		"enterprise_id": acct.EnterpriseID,
		"type":          acct.Type,
		"nickname":      acct.Nickname,
	}

	// 注册出口：CLI 没有账号池，绑定只能随 auth 文件落盘，server 首次载入时作为
	// 初始绑定（state.json 里的既有绑定优先）。
	if slot != "" {
		out["proxy_slot"] = slot
	}

	return out
}

// loginSlot 命名出口槽（与 config 的 proxy_slots 同名字段一一对应）。
type loginSlot struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	URL     string `json:"url"`
	Enabled bool   `json:"enabled"`
}

// loginConfig 登录出口相关的配置片段（issue #609）：CLI 没有账号池上下文，只需要
// 命名槽、realm 默认槽，以及决定是否按槽出站的开关。
type loginConfig struct {
	Slots      []loginSlot
	RealmSlots map[string]string
	// bindProxy features.login_bind_proxy（缺省 true，与 server 的 Default 同口径）。
	bindProxy bool
}

// loadLoginConfig 读取登录出口配置。文件缺失 = 未配代理（直连）：首次部署时配置
// 可能还没生成，登录不该因此失败；文件存在但解析失败则报错（配置坏了就明确失败，
// 不要静默按直连出网）。
func loadLoginConfig(path string) (loginConfig, error) {
	config := loginConfig{bindProxy: true}

	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return config, nil
		}

		return config, fmt.Errorf("read login config: %w", err)
	}

	var parsed struct {
		Upstream struct {
			ProxySlots      []loginSlot       `json:"proxy_slots"`
			RealmProxySlots map[string]string `json:"realm_proxy_slots"`
		} `json:"upstream"`
		Features struct {
			LoginBindProxy *bool `json:"login_bind_proxy"`
		} `json:"features"`
	}

	if err := jsonx.Unmarshal(raw, &parsed); err != nil {
		return config, fmt.Errorf("parse login config: %w", err)
	}

	config.Slots = parsed.Upstream.ProxySlots
	config.RealmSlots = parsed.Upstream.RealmProxySlots

	if parsed.Features.LoginBindProxy != nil {
		config.bindProxy = *parsed.Features.LoginBindProxy
	}

	return config, nil
}

// loginEgressClient 按配置为 CLI 登录挑出口（与面板同口径，issue #609）：
//   - 开关关闭 → 直连；
//   - 一条启用槽都没有 → 直连（未配代理的部署不能因此加不了号）；
//   - 有启用槽 → 整条 state/token/account 固定走挑中的槽；
//   - 有启用槽但都不可用/不健康 → 报错（调用方必须失败，绝不回落直连）。
//
// pinned 是发起时选定的槽（state 文件里记的）：poll 是另一个进程，必须沿用它而不是
// 重新选举，否则同一 device-auth 会话的 auth/state 与 auth/token 可能来自两条出口，
// 且「第一条启用槽已死、后面有健康槽」的部署会永久挑到死槽（进程内的冷却记忆每次清零）。
//
// 返回的 slot 为空表示直连。
func loginEgressClient(config loginConfig, realm, pinned string) (*http.Client, string, error) {
	jar, _ := cookiejar.New(nil)

	client := &http.Client{Timeout: 30 * time.Second, Jar: jar}
	if !config.bindProxy {
		if pinned != "" {
			return nil, "", fmt.Errorf(
				"%w: 本次登录发起时绑定了槽 %q，但 features.login_bind_proxy 已关闭（不回落直连）",
				outbound.ErrProxyDial, pinned)
		}

		return client, "", nil
	}

	reg, err := loginRegistry(config)
	if err != nil {
		return nil, "", err
	}

	if pinned != "" {
		dial, ok := reg.SlotDial(pinned)
		if !ok {
			return nil, "", fmt.Errorf("%w: 登录发起时的代理槽 %q 已不可用（不换出口、不回落直连）",
				outbound.ErrProxyDial, pinned)
		}

		return withDial(client, dial), pinned, nil
	}

	slot, dial, lerr := reg.LoginSlot(realm)
	if lerr != nil {
		return nil, "", fmt.Errorf("挑选登录出口失败: %w", lerr)
	}

	if dial == nil {
		return client, "", nil
	}

	return withDial(client, dial), slot, nil
}

// loginRegistry 用配置文件里的槽构造一次性注册表（CLI 无长驻池，进程内不记忆冷却）。
func loginRegistry(config loginConfig) (*outbound.Registry, error) {
	slots := make([]outbound.Slot, 0, len(config.Slots))
	for _, slot := range config.Slots {
		slots = append(slots, outbound.Slot{ID: slot.ID, Name: slot.Name, URL: slot.URL, Enabled: slot.Enabled})
	}

	reg, err := outbound.NewRegistry(nil, slots, config.RealmSlots)
	if err != nil {
		return nil, fmt.Errorf("构造登录出口注册表失败: %w", err)
	}

	return reg, nil
}

// withDial 把 client 换成按槽出站的 Transport（超时口径与面板一致）。
func withDial(client *http.Client, dial func(ctx context.Context, network, addr string) (net.Conn, error)) *http.Client {
	client.Transport = &http.Transport{
		DialContext:         dial,
		TLSHandshakeTimeout: 10 * time.Second,
		IdleConnTimeout:     30 * time.Second,
	}

	return client
}

func main() {
	realm, rest, err := parseRealmArgs(os.Args[1:])
	if err != nil {
		fatalf("%v (usage: login [--realm=cn|global] <url|poll|realm>)", err)
	}

	if len(rest) < 1 {
		fatalf("usage: login [--realm=cn|global] <url|poll>")
	}

	base, origin := realmConfig(realm)

	switch rest[0] {
	case "url", "poll":
		// 登录出口（issue #609）：配了槽就整条链路固定走同一条，槽不可用直接失败。
		// `login realm` 是纯本地子命令，不走这里——配置坏掉时仍要能切域自救。
		config, cerr := loadLoginConfig(loginConfigPath)
		if cerr != nil {
			fatalf("%v", cerr)
		}

		// poll 是另一个进程：沿用 url 发起时选定的槽，不重新选举。
		pinned := ""
		if rest[0] == "poll" {
			pinned = loadLoginState(stateFile, realm).Slot
		}

		// 每个流程独立 cookie jar（oauth.go:22-29：多账号登录互不串会话）
		client, slot, eerr := loginEgressClient(config, realm, pinned)
		if eerr != nil {
			fatalf("登录出口不可用: %v", eerr)
		}

		if slot != "" {
			fmt.Fprintf(os.Stderr, "login: 登录出站走代理槽 %s\n", slot)
		}

		if rest[0] == "url" {
			runURL(base, origin, realm, stateFile, mustLoginIdentity(realm), client, slot, os.Stdout)

			return
		}

		runPoll(base, origin, realm, stateFile, mustLoginIdentity(realm), client, slot, os.Stdout)

	case "realm":
		// 交互式选域（login.sh 无 --realm 传参且 stdin 为 tty 时调用）。
		// 提示打到 stderr，stdout 只输出归一化 realm，供 $( ) 捕获。
		realm := promptRealm(os.Stdin, os.Stderr)
		fmt.Println(realm)

	default:
		fatalf("unknown subcommand %q (want url|poll|realm)", rest[0])
	}
}
