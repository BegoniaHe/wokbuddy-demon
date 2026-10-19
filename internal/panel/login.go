// login.go 面板内嵌的 WorkBuddy CN OAuth 设备授权流程（cmd/login 的进程内移植）。
//
//	POST /panel/api/login/start → 拿 state+authUrl，state 存进程内（不再落 /tmp，
//	  原方案在 Windows 上不可用），返回授权 URL；
//	GET  /panel/api/login/poll   → 面板前端每 3s 轮询本接口；未完成返回 done=false，
//	  完成后取 uid/nickname、凭证落盘 auths/workbuddy-<uid>.json、热加载进池
//	  （pool.Add + Revive），并顺带签到 + 余额刷新 —— 免重启加载新账号。
//
// 无 PKCE（workbuddy 设备流由服务端签发 state），请求头与上游端点与 cmd/login 保持一致。
package panel

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
	"time"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
)

const (
	upstreamBaseCN      = "https://www.workbuddy.cn"
	upstreamBaseGlobal  = "https://www.workbuddy.ai"
	clientUA            = "CLI/2.63.2 CodeBuddy/2.63.2"
	originRefererCN     = "https://www.codebuddy.cn"
	originRefererGlobal = "https://www.workbuddy.ai"
)

// CN 登录 origin 回落顺序：与 cmd/login、chat/refresh 保持同一口径。
var cnLoginFallbackBases = []string{"https://copilot.tencent.com", "https://wb.tencentbuddy.com"}

var errDoJSONFailed = errors.New("doJSON failed")

// loginEndpoint 单 realm 的一组设备授权端点与 Origin。
type loginEndpoint struct {
	base    string
	state   string
	token   string
	account string
	origin  string
}

// loginEndpointForBase 按 realm/base 拼出设备授权三端点。
// auth/state 的 platform 参数取官方 product.json authentication.attributes.platform
// （cn "workbuddy" / global "workbuddy-ai"）。
func loginEndpointForBase(realm, base string) loginEndpoint {
	if realm == "global" {
		return loginEndpoint{
			base:    base,
			state:   base + "/v2/plugin/auth/state?platform=workbuddy-ai",
			token:   base + "/v2/plugin/auth/token?state=",
			account: base + "/v2/plugin/login/account?state=",
			origin:  originRefererGlobal,
		}
	}

	return loginEndpoint{
		base:    base,
		state:   base + "/v2/plugin/auth/state?platform=workbuddy",
		token:   base + "/v2/plugin/auth/token?state=",
		account: base + "/v2/plugin/login/account?state=",
		origin:  originRefererCN,
	}
}

// loginEndpoints 按 realm 返回主 origin 的设备授权三端点 + Origin。
// realm=="global" → 国际版（workbuddy.ai 同域）；cn/非法/缺省 → CN（workbuddy.cn）。
func loginEndpoints(realm string) (state, token, account, origin string) {
	base := upstreamBaseCN
	if realm == "global" {
		base = upstreamBaseGlobal
	}

	ep := loginEndpointForBase(realm, base)

	return ep.state, ep.token, ep.account, ep.origin
}

// loginCandidates 返回该 realm 可尝试的 origin 顺序；CN 主域失败时可回落旧域。
func loginCandidates(realm string) []loginEndpoint {
	if realm == "global" {
		return []loginEndpoint{loginEndpointForBase(realm, upstreamBaseGlobal)}
	}

	out := make([]loginEndpoint, 0, 1+len(cnLoginFallbackBases))
	out = append(out, loginEndpointForBase("cn", upstreamBaseCN))

	for _, base := range cnLoginFallbackBases {
		out = append(out, loginEndpointForBase("cn", base))
	}

	return out
}

// loginCandidatesPrefer 把 preferredBase 提到候选首位（发起登录用的 origin 优先）。
func loginCandidatesPrefer(realm, preferredBase string) []loginEndpoint {
	candidates := loginCandidates(realm)
	if preferredBase == "" {
		return candidates
	}

	for i := range candidates {
		if candidates[i].base == preferredBase {
			candidates[0], candidates[i] = candidates[i], candidates[0]

			break
		}
	}

	return candidates
}

// loginReplayable 报告登录请求失败是否值得换 origin：仅传输错误与网关类状态码。
func loginReplayable(status int) bool {
	switch status {
	case 0, http.StatusRequestTimeout, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	default:
		return false
	}
}

// loginHTTP 设备授权专用 client：短超时、无 cookie（每请求携带 state，无会话态）。
var loginHTTP = &http.Client{Timeout: 30 * time.Second}

func commonHeaders(req *http.Request, origin string) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("Origin", origin)
	req.Header.Set("Referer", origin+"/")
	req.Header.Set("User-Agent", clientUA)
}

// validUID 校验上游返回的 uid 是否可安全用于拼文件名。
// 只放行字母、数字、下划线、连字符（腾讯侧 uid 实测为 UUID 形态），
// 长度上限 64 兜底异常超长串；拒绝 . / \ 等路径字符与空串。
func validUID(uid string) bool {
	if uid == "" || len(uid) > 64 {
		return false
	}

	for _, c := range uid {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_':
		default:
			return false
		}
	}

	return true
}

// apiEnvelope 与 upstream 同形：{code,msg,data}，code!=0 视为业务错误。
type apiEnvelope struct {
	Code int             `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

// doJSON 发一次 JSON 请求并解信封。origin 为 Origin/Referer 基础域（随 realm 切）。
func doJSON(method, fullURL, bearer string, body io.Reader, origin string) (json.RawMessage, int, error) {
	req, err := http.NewRequestWithContext(context.Background(), method, fullURL, body)
	if err != nil {
		return nil, 0, fmt.Errorf("doJSON: %w", err)
	}

	commonHeaders(req, origin)

	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	} else {
		// 未登录态显式声明（官方 fetchAuthState/loopGetToken 同款）：无凭证。
		req.Header.Set("X-No-Authorization", "true")
	}
	// 设备授权流程恒声明无用户/企业/部门上下文（官方 fetchAuthState 头组）。
	req.Header.Set("X-No-User-Id", "true")
	req.Header.Set("X-No-Enterprise-Id", "true")
	req.Header.Set("X-No-Department-Info", "true")

	resp, err := loginHTTP.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("doJSON: %w", err)
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		return nil, resp.StatusCode, fmt.Errorf("http_error: upstream %d: %w", resp.StatusCode, errDoJSONFailed)
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

// loginStartState 按 origin 候选发起 auth/state，返回响应、命中的 endpoint 与最后状态。
func loginStartState(realm string) (json.RawMessage, loginEndpoint, int, error) {
	var (
		lastStatus int
		lastErr    error
	)

	for _, endpoint := range loginCandidates(realm) {
		data, status, err := doJSON(http.MethodPost, endpoint.state, "", bytes.NewReader([]byte("{}")), endpoint.origin)
		if err == nil {
			return data, endpoint, status, nil
		}

		lastStatus, lastErr = status, err
		if !loginReplayable(status) {
			break
		}
	}

	return nil, loginEndpoint{}, lastStatus, lastErr
}

// loginStart 发起设备授权：POST auth/state 拿授权 URL。
// body 可带 {"realm":"global"}（缺省 cn）；state 会话记 realm，poll 同 realm 落盘。
func (p *Panel) loginStart(w http.ResponseWriter, r *http.Request) {
	realm := "cn"

	if r.Body != nil {
		var reqBody struct {
			Realm string `json:"realm"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<12)).Decode(&reqBody); err == nil {
			if reqBody.Realm == "global" {
				realm = "global"
			}
		}
	}

	data, used, status, err := loginStartState(realm)
	if err != nil {
		writeErr(w, http.StatusBadGateway, fmt.Sprintf("auth state (upstream %d): %v", status, err))

		return
	}

	var item struct {
		State   string `json:"state"`
		AuthURL string `json:"authUrl"`
	}
	if err := json.Unmarshal(data, &item); err != nil || item.State == "" || item.AuthURL == "" {
		writeErr(w, http.StatusBadGateway, "auth state: missing state or authUrl")
		return
	}

	p.loginMu.Lock()
	// 顺手回收过期会话，防"开弹窗走开"的 state 滞留。
	for s, sess := range p.logins {
		if time.Since(sess.created) > loginTTL {
			delete(p.logins, s)
		}
	}

	p.logins[item.State] = loginSession{created: time.Now(), realm: realm, base: used.base}
	p.loginMu.Unlock()
	log.Printf("panel: 发起 OAuth 添加账号 realm=%s（state=%s...）", realm, item.State[:min(8, len(item.State))])
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "url": item.AuthURL, "state": item.State, "realm": realm})
}

// loginToken auth/token 端点返回的凭证字段。
type loginToken struct {
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
	ExpiresIn    int64  `json:"expiresIn"`
	Domain       string `json:"domain"`
}

// loginAccount login/account 端点返回的账号字段。
type loginAccount struct {
	UID          string `json:"uid"`
	EnterpriseID string `json:"enterpriseId"`
	Nickname     string `json:"nickname"`
}

// loginPoll 轮询登录态。未完成 → {done:false}；完成 → 建凭证、落盘、热加载、签到。
func (p *Panel) loginPoll(w http.ResponseWriter, r *http.Request) {
	state := r.URL.Query().Get("state")
	if state == "" {
		writeErr(w, http.StatusBadRequest, "missing state")
		return
	}

	p.loginMu.Lock()
	sess, known := p.logins[state]
	// 超时会话直接回收：loginTTL 是授权 URL 的最长有效期，过期后不得再
	// 继续 poll（否则一次发起即可无限期占用 state，且持续打上游）。
	if known && time.Since(sess.created) > loginTTL {
		delete(p.logins, state)

		known = false
	}
	p.loginMu.Unlock()

	if !known {
		writeErr(w, http.StatusNotFound, "unknown or expired state（请重新发起添加账号）")
		return
	}

	tok, pending, endpoint, err := pollLoginToken(sess.realm, sess.base, state)
	if err != nil {
		// pending / 未完成：面板前端继续轮询。
		writeJSON(w, http.StatusOK, map[string]any{"done": false, "message": err.Error()})
		return
	}

	if pending {
		writeJSON(w, http.StatusOK, map[string]any{"done": false, "message": "waiting for login"})
		return
	}

	acct := fetchLoginAccount(&endpoint, state, tok.AccessToken)

	account, status, msg := p.persistLoginAuth(sess.realm, tok, acct)
	if status != 0 {
		writeErr(w, status, msg)
		return
	}

	// 顺带签到 + 余额刷新（幂等；失败不影响登录结果，只体现在返回字段里）。
	checkinMsg, remain, total := p.postLoginTasks(account, sess.realm, acct.UID)

	p.loginMu.Lock()
	delete(p.logins, state)
	p.loginMu.Unlock()
	log.Printf("panel: 新账号已热加载 uid=%s nickname=%q realm=%s（免重启生效）", acct.UID, acct.Nickname, sess.realm)
	writeJSON(w, http.StatusOK, map[string]any{
		"done":            true,
		"uid":             acct.UID,
		"nickname":        acct.Nickname,
		"realm":           sess.realm,
		"credits":         remain,
		"credits_total":   total,
		"checkin_message": checkinMsg,
	})
}

// pollLoginToken 查 auth/token：请求失败返回 err（面板按 pending 处理）；token 尚未
// 就绪返回 pending=true；成功返回解析后的凭证与命中的 origin（account 端点同源）。
func pollLoginToken(realm, preferredBase, state string) (loginToken, bool, loginEndpoint, error) {
	var lastErr error

	for _, endpoint := range loginCandidatesPrefer(realm, preferredBase) {
		tokRaw, status, err := doJSON(http.MethodGet, endpoint.token+state, "", nil, endpoint.origin)
		if err != nil {
			lastErr = err

			if loginReplayable(status) {
				continue
			}

			return loginToken{}, false, loginEndpoint{}, err
		}

		var tok loginToken

		_ = json.Unmarshal(tokRaw, &tok)
		if tok.AccessToken == "" {
			return loginToken{}, true, endpoint, nil
		}

		return tok, false, endpoint, nil
	}

	return loginToken{}, false, loginEndpoint{}, lastErr
}

// fetchLoginAccount 取 uid/nickname（失败不阻塞，仅缺展示名）。
func fetchLoginAccount(endpoint *loginEndpoint, state, bearer string) loginAccount {
	var acct loginAccount
	if acctRaw, _, err := doJSON(http.MethodGet, endpoint.account+state, bearer, nil, endpoint.origin); err == nil {
		_ = json.Unmarshal(acctRaw, &acct)
	}

	return acct
}

// persistLoginAuth 校验 uid、落盘凭证并热加载进池。返回非零 status 表示失败，
// 调用方据此回写 HTTP 错误；成功时返回落盘后的 auth。
func (p *Panel) persistLoginAuth(realm string, tok loginToken, acct loginAccount) (*auth.Auth, int, string) {
	if acct.UID == "" {
		return nil, http.StatusBadGateway, "login done but no uid（token 已发但账号信息获取失败，请重试）"
	}
	// UID 来自上游响应，未经校验就用于拼文件名会被路径穿越利用
	// （filepath.Join("./auths", "workbuddy-../../evil.json") → auths/evil.json）。
	// UID 是腾讯侧账号标识，实测为 UUID（十六进制与连字符），故只放行 [A-Za-z0-9_-]。
	if !validUID(acct.UID) {
		return nil, http.StatusBadGateway, "上游返回的 uid 含非法字符，拒绝落盘（防路径穿越）"
	}

	// 凭证落盘（嵌套形，与 auths/ 目录既有格式一致）→ 热加载进池。
	if err := os.MkdirAll(p.cfg.AuthDir, 0o755); err != nil {
		return nil, http.StatusInternalServerError, "mkdir auth dir: " + err.Error()
	}

	account := &auth.Auth{
		AccessToken:  tok.AccessToken,
		RefreshToken: tok.RefreshToken,
		ExpiresAt:    time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second).Unix(),
		Domain:       tok.Domain,
		UID:          acct.UID,
		EnterpriseID: acct.EnterpriseID,
		Nickname:     acct.Nickname,
		FilePath:     filepath.Join(p.cfg.AuthDir, fmt.Sprintf("workbuddy-%s.json", acct.UID)),
	}
	// global 登录：落盘 auth.realm=global（Realm() 按此判域；不写则依赖 domain 后缀回落）。
	if realm == "global" {
		if _, err := auth.BackfillRealmFor(account, "global"); err != nil {
			return nil, http.StatusInternalServerError, "set realm: " + err.Error()
		}
	} else {
		// CN 也显式补 realm 键（幂等），让 auth 文件形态统一（与 LoadDir 存量迁移对齐）。
		_, _ = account.BackfillRealm()
	}

	if err := account.SaveAtomic(); err != nil {
		return nil, http.StatusInternalServerError, "save auth: " + err.Error()
	}

	p.cfg.Pool.Add(account)
	p.cfg.Pool.Revive(acct.UID) // 全新登录 = 人工恢复口径：清掉旧号遗留的禁用/冷却/熔断

	return account, 0, ""
}

// postLoginTasks 登录完成后的签到/激活与余额刷新，返回 (提示消息, 余额, 总额)。
// realm 分支：CN 走 DailyCheckin；global 登录时改为注册激活 + trial 领取（签到由
// 调度器统一执行，与 CN 同口径）；企业版跳过签到（上游不支持）。
func (p *Panel) postLoginTasks(account2 *auth.Auth, realm, uid string) (string, int64, int64) {
	checkinMsg := ""
	if realm == "global" {
		checkinMsg = p.activateGlobalLogin(account2, uid)
	} else if !account2.IsEnterprise() {
		// 企业版跳过签到：上游 POST /v2/billing/meter/daily-checkin 对企业号
		// 400 code 10001「企业账号不支持该操作」，发了只会把该错误写进登录返回。
		// 下方 UserResource 余额查询照常（企业额度走 get-enterprise-user-usage 口径）。
		if err := p.cfg.Upstream.DailyCheckin(account2); err != nil {
			checkinMsg = err.Error()
		}
	}

	remain, total := int64(-1), int64(0)
	if rm, tt, err := p.cfg.Upstream.UserResource(account2); err == nil {
		remain, total = rm, tt
		p.cfg.Pool.ReenableIfCredits(uid, rm, tt)
	}

	return checkinMsg, remain, total
}

// activateGlobalLogin global 登录的注册激活 + trial 领取（幂等；失败不阻断登录结果，
// auth 已落盘，只在返回字段里体现）。
func (p *Panel) activateGlobalLogin(account *auth.Auth, uid string) string {
	checkinMsg := ""
	// 注册激活（幂等）：region required 时自动补地区（白名单首个，HK）后重新激活。
	if activated, err := p.cfg.Upstream.GlobalCompleteRegistration(account); err != nil {
		checkinMsg = "注册激活失败: " + err.Error()
		log.Printf("panel: global 注册激活 uid=%s: %v", uid, err)
	} else if activated {
		log.Printf("panel: global 注册激活 uid=%s 完成", uid)
	}
	// trial 加油包（幂等 14051 = 已领过，非错误）。
	if claimed, err := p.cfg.Upstream.ClaimTrial(account); err != nil {
		checkinMsg = joinMsg(checkinMsg, "trial 领取失败: "+err.Error())
		log.Printf("panel: global trial uid=%s: %v", uid, err)
	} else if claimed {
		log.Printf("panel: global trial uid=%s 已领", uid)
	}

	return checkinMsg
}

// joinMsg 拼接 login 完成后的提示消息（多段用「；」连接，空段跳过）。
func joinMsg(parts ...string) string {
	out := ""

	for _, item := range parts {
		if item == "" {
			continue
		}

		if out != "" {
			out += "；"
		}

		out += item
	}

	return out
}

// loginRegions 返回 global 注册可选地区（panel 前端选地区弹窗用；CN 不调用）。
// 未持账号时返回白名单静态兜底（前端只读展示，不依赖上游）。
func (p *Panel) loginRegions(w http.ResponseWriter, r *http.Request) {
	// 静态白名单（对齐国际版 web 展示集）：面板前端只读展示，无需账号态。
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true,
		"regions": []map[string]string{
			{"code": "HK", "name": "Hong Kong"},
			{"code": "MO", "name": "Macao"},
			{"code": "SG", "name": "Singapore"},
			{"code": "TH", "name": "Thailand"},
			{"code": "PH", "name": "Philippines"},
			{"code": "MY", "name": "Malaysia"},
			{"code": "ID", "name": "Indonesia"},
		},
	})
}
