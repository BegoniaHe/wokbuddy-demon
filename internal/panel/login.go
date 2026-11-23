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
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
	"github.com/BegoniaHe/wokbuddy-demon/internal/jsonx"
	"github.com/BegoniaHe/wokbuddy-demon/internal/logincode"
	"github.com/BegoniaHe/wokbuddy-demon/internal/outbound"
	"github.com/BegoniaHe/wokbuddy-demon/internal/session"
	"github.com/BegoniaHe/wokbuddy-demon/internal/upstream"
)

const (
	originRefererCN     = "https://www.codebuddy.cn"
	originRefererGlobal = "https://www.workbuddy.ai"
	// loginProductSaaS 是官方 REST 拦截器在请求未自带 X-Product 时补的
	// deploymentType。两版 cli/product.json 都是 SaaS；本仓不走那层客户端。
	loginProductSaaS = "SaaS"
)

// 登录端点基址。用 var 只为把测试流量改到回环假上游：生产恒为官方域。
var (
	upstreamBaseCN     = "https://www.workbuddy.cn"
	upstreamBaseGlobal = "https://www.workbuddy.ai"
)

// CN 登录 origin 回落顺序：与 cmd/login、chat/refresh 保持同一口径。
var cnLoginFallbackBases = []string{"https://copilot.tencent.com", "https://wb.tencentbuddy.com"}

var errDoJSONFailed = errors.New("doJSON failed")

// errLoginProfileMissing 部署 profile 未提供 loginUA，面板无法发起登录。
var errLoginProfileMissing = errors.New("client profile 缺少 loginUA，请用 reverse-workbuddy 重新生成")

// loginEndpoint 单 realm 的一组设备授权端点、Origin 与客户端 UA。
type loginEndpoint struct {
	base      string
	state     string
	token     string
	account   string
	origin    string
	userAgent string
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

// defaultLoginIdentity 只提供 platform 缺省值与 CN 站的 loginSessionId；UA 与 version
// 必须来自部署 profile（仓库不内置客户端版本 / 登录 UA）。
func defaultLoginIdentity(realm string) loginIdentity {
	identity := loginIdentity{platform: "workbuddy"}
	if realm == "global" {
		identity.platform = "workbuddy-ai"
	}

	if identity.platform == "workbuddy" {
		identity.sessionID = session.NewUUIDv4()
	}

	return identity
}

// loginIdentityFor 取部署 profile 的 loginUA / loginPlatform。profile 或 loginUA
// 缺失时 userAgent 为空，调用方必须拒绝登录（仓库不内置任何登录 UA）。
func (p *Panel) loginIdentityFor(realm string) loginIdentity {
	identity := defaultLoginIdentity(realm)
	if p == nil || p.cfg.Upstream == nil || p.cfg.Upstream.Profile == nil {
		return identity
	}

	profile := p.cfg.Upstream.Profile.ForRealm(realm)
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

func loginEndpointForBaseWithIdentity(realm, base string, identity loginIdentity) loginEndpoint {
	origin := originRefererCN
	if realm == "global" {
		origin = originRefererGlobal
	}

	return loginEndpoint{
		base:      base,
		state:     loginStateURL(base, identity),
		token:     base + "/v2/plugin/auth/token?state=",
		account:   base + "/v2/plugin/login/account?state=",
		origin:    origin,
		userAgent: identity.userAgent,
	}
}

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

func loginCandidatesWithIdentity(realm string, identity loginIdentity) []loginEndpoint {
	if realm == "global" {
		return []loginEndpoint{loginEndpointForBaseWithIdentity(realm, upstreamBaseGlobal, identity)}
	}

	out := make([]loginEndpoint, 0, 1+len(cnLoginFallbackBases))
	out = append(out, loginEndpointForBaseWithIdentity("cn", upstreamBaseCN, identity))

	for _, base := range cnLoginFallbackBases {
		out = append(out, loginEndpointForBaseWithIdentity("cn", base, identity))
	}

	return out
}

func loginCandidatesPreferWithIdentity(realm, preferredBase string, identity loginIdentity) []loginEndpoint {
	candidates := loginCandidatesWithIdentity(realm, identity)
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
	case 0, http.StatusRequestTimeout, http.StatusBadGateway,
		http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	default:
		return false
	}
}

// loginHTTP 设备授权专用 client：短超时、无 cookie（每请求携带 state，无会话态）。
// 直连出口（一条启用槽都没有 / 开关关闭）与既有测试的替换点都用它。
var loginHTTP = &http.Client{Timeout: 30 * time.Second}

// loginDial 出站拨号器（与 outbound 池的 DialContext 同签名）。
type loginDial = func(ctx context.Context, network, addr string) (net.Conn, error)

// loginEgress 一条登录链路的固定出口（issue #609）：slot 为空 = 直连。
type loginEgress struct {
	slot string
	dial loginDial
}

// loginEgressFor 为「还没有账号」的登录挑一条启用且健康的槽，整条
// state/token/account 链路固定走它（issue #609）。
//
// 语义与 outbound.Registry.LoginSlot 一致：一条启用槽都没有 → 直连（未配代理的
// 部署不能因此加不了号）；有启用槽但都不可用 → 报错（fail-closed，绝不静默直连）。
// features.login_bind_proxy 关闭时一律直连。
func (p *Panel) loginEgressFor(realm string) (loginEgress, error) {
	if !p.loginBindProxy() {
		return loginEgress{}, nil
	}

	slot, dial, err := p.loginRegistry().LoginSlot(realm)
	if err != nil {
		return loginEgress{}, fmt.Errorf("登录出口不可用: %w", err)
	}

	return loginEgress{slot: slot, dial: dial}, nil
}

// loginEgressForSlot 按发起时选定的槽取拨号器：槽在轮询期间被删除/禁用/改名即
// fail-closed 中止本次登录，绝不换出口、也绝不回落直连。
func (p *Panel) loginEgressForSlot(slot string) (loginEgress, error) {
	if slot == "" {
		return loginEgress{}, nil
	}

	if dial, ok := p.loginRegistry().SlotDial(slot); ok {
		return loginEgress{slot: slot, dial: dial}, nil
	}

	return loginEgress{}, fmt.Errorf("%w: 代理槽 %q 已不可用（本次登录中止，不回落直连）",
		outbound.ErrProxyDial, slot)
}

// loginBindProxy 读取 features.login_bind_proxy（默认 true）。
//
// Live 快照缺失时按「开启」处理而不是按零值（false）处理：零值会让配了代理槽的
// 部署静默直连，把宿主机 IP 交出去，而这正是这条链路要避免的形态。
func (p *Panel) loginBindProxy() bool {
	if p == nil || p.cfg.Live == nil {
		return true
	}

	return p.cfg.Live.Load().LoginBindProxy
}

// loginRegistry 返回当前生效的出站注册表（nil = 未装配，等价直连）。
func (p *Panel) loginRegistry() *outbound.Registry {
	if p == nil || p.cfg.Upstream == nil {
		return nil
	}

	return p.cfg.Upstream.ProxyRegistry()
}

// client 返回本次出站的 client：有槽则新建按槽出站的 Transport；无槽沿用
// loginHTTP（直连，测试可替换）。
func (e loginEgress) client() *http.Client {
	if e.dial == nil {
		return loginHTTP
	}

	return &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			DialContext:         e.dial,
			TLSHandshakeTimeout: 10 * time.Second,
			IdleConnTimeout:     30 * time.Second,
		},
	}
}

// close 关闭按槽新建的 Transport 的空闲连接；共享的直连 client 不动。
func (e loginEgress) close(client *http.Client) {
	if e.dial != nil {
		client.CloseIdleConnections()
	}
}

// loginEgressLabel 日志用出口标签：直连 / 槽 id（槽 id 不含凭据）。
func loginEgressLabel(egress loginEgress) string {
	if egress.slot == "" {
		return "direct"
	}

	return "slot:" + egress.slot
}

// commonHeaders 按 origin 设置通用请求头（Origin/Referer 随 realm 变化）。
func commonHeaders(req *http.Request, origin, userAgent string) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("Origin", origin)
	req.Header.Set("Referer", origin+"/")
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("X-Product", loginProductSaaS)
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
	Code int            `json:"code"`
	Msg  string         `json:"msg"`
	Data jsontext.Value `json:"data"`
}

// doJSON 发一次 JSON 请求并解信封。origin 为 Origin/Referer 基础域（随 realm 切）；
// client 由调用方按本次登录的固定出口提供（loginEgress.client）。
// ctx 来自 HTTP 请求：轮询客户端断开后应立即放弃（原先用 Background()，一次
// auth/state→token→account 链路在客户端关页面后仍会跑满全部候选 origin 与超时）。
func doJSON(
	ctx context.Context, client *http.Client, method, fullURL, bearer string,
	body io.Reader, origin, userAgent string,
) (jsontext.Value, int, error) {
	//nolint:gosec // URL 由固定 realm 端点拼接，非用户可控主机
	req, err := http.NewRequestWithContext(ctx, method, fullURL, body)
	if err != nil {
		return nil, 0, fmt.Errorf("doJSON: %w", err)
	}

	commonHeaders(req, origin, userAgent)

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

	resp, err := client.Do(req) //nolint:gosec // 同上，请求 URL 来自固定 realm 端点
	if err != nil {
		return nil, 0, fmt.Errorf("doJSON: %w", err)
	}
	defer resp.Body.Close()

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

	if resp.StatusCode >= http.StatusMultipleChoices {
		return nil, resp.StatusCode, fmt.Errorf("http_error: upstream %d: %w", resp.StatusCode, errDoJSONFailed)
	}

	if parseErr != nil {
		return nil, resp.StatusCode, fmt.Errorf("parse failed: %w", parseErr)
	}

	return env.Data, resp.StatusCode, nil
}

// loginStartState 按 origin 候选发起 auth/state，返回响应、命中的 endpoint、本次
// 登录固定的出口与最后状态。
func (p *Panel) loginStartState(
	ctx context.Context, realm string,
) (jsontext.Value, loginEndpoint, loginEgress, int, error) {
	identity := p.loginIdentityFor(realm)
	if identity.userAgent == "" {
		return nil, loginEndpoint{}, loginEgress{}, 0, errLoginProfileMissing
	}

	egress, eerr := p.loginEgressFor(realm)
	if eerr != nil {
		return nil, loginEndpoint{}, loginEgress{}, 0, eerr
	}

	client := egress.client()
	defer egress.close(client)

	var (
		lastStatus int
		lastErr    error
	)

	for _, endpoint := range loginCandidatesWithIdentity(realm, identity) {
		data, status, err := doJSON(
			ctx, client, http.MethodPost, endpoint.state, "",
			bytes.NewReader([]byte("{}")), endpoint.origin, endpoint.userAgent,
		)
		if err == nil {
			return data, endpoint, egress, status, nil
		}

		lastStatus, lastErr = status, err
		if !loginReplayable(status) {
			break
		}
	}

	return nil, loginEndpoint{}, loginEgress{}, lastStatus, lastErr
}

// loginRealmFromBody 解析登录请求体里的 realm；缺省或非法值按 cn。
func loginRealmFromBody(r *http.Request) string {
	if r == nil || r.Body == nil {
		return "cn"
	}

	var reqBody struct {
		Realm string `json:"realm"`
	}

	if err := jsonx.UnmarshalRead(io.LimitReader(r.Body, 1<<12), &reqBody); err != nil {
		return "cn"
	}

	if reqBody.Realm == "global" {
		return "global"
	}

	return "cn"
}

// loginStart 发起设备授权：POST auth/state 拿授权 URL。
// body 可带 {"realm":"global"}（缺省 cn）；state 会话记 realm，poll 同 realm 落盘。
func (p *Panel) loginStart(w http.ResponseWriter, r *http.Request) {
	realm := loginRealmFromBody(r)

	data, used, egress, status, err := p.loginStartState(r.Context(), realm)
	if err != nil {
		if errors.Is(err, errLoginProfileMissing) {
			writeErr(w, http.StatusServiceUnavailable, err.Error())

			return
		}
		// 出口不可用（有启用槽但全不健康/槽缺失）必须明确报代理错误：回落直连
		// 等于把宿主机 IP 交出去，还让随后的账号绑定成为谎话（issue #609）。
		if errors.Is(err, outbound.ErrNoHealthySlot) || errors.Is(err, outbound.ErrProxyDial) {
			writeErr(w, http.StatusBadGateway, "auth state: "+err.Error())

			return
		}

		writeErr(w, http.StatusBadGateway, fmt.Sprintf("auth state (upstream %d): %v", status, err))

		return
	}

	var item struct {
		State   string `json:"state"`
		AuthURL string `json:"authUrl"`
	}
	if err := jsonx.Unmarshal(data, &item); err != nil || item.State == "" || item.AuthURL == "" {
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

	p.logins[item.State] = loginSession{created: time.Now(), realm: realm, base: used.base, slot: egress.slot}
	p.loginMu.Unlock()
	log.Printf("panel: 发起 OAuth 添加账号 realm=%s 出口=%s（state=%s...）",
		realm, loginEgressLabel(egress), item.State[:min(8, len(item.State))])
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "url": item.AuthURL, "state": item.State, "realm": realm,
	})
}

// loginPendingInfo pending 轮询携带的上游业务码与消息（用于按 state 去重记日志）。
type loginPendingInfo struct {
	code int
	msg  string
}

// loginToken auth/token 端点返回的凭证字段。
type loginToken struct {
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
	ExpiresIn    int64  `json:"expiresIn"`
	Domain       string `json:"domain"`
}

// loginExpiry 把上游 expiresIn（秒）换成有效期。
//
// 上游字段是 int64：大于约 2.9e11 秒时 *time.Second 溢出成负 duration，新账号会
// 以「已过期」落盘（或得到一个荒谬的未来时间，NeedsRefresh 永远 false）。异常值
// 一律回落到官方默认有效期。
func loginExpiry(expiresIn int64) time.Duration {
	const (
		defaultExpiry = time.Hour
		maxExpiry     = 365 * 24 * time.Hour
	)

	if expiresIn <= 0 || expiresIn > int64(maxExpiry/time.Second) {
		return defaultExpiry
	}

	return time.Duration(expiresIn) * time.Second
}

// lookupLoginSession 取登录会话并回收超时项；loginTTL 是授权 URL 的最长有效期，
// 过期后不得继续 poll（否则一次发起即可无限期占用 state，且持续打上游）。
func (p *Panel) lookupLoginSession(state string) (loginSession, bool) {
	p.loginMu.Lock()
	defer p.loginMu.Unlock()

	sess, known := p.logins[state]
	if !known || time.Since(sess.created) <= loginTTL {
		return sess, known
	}

	delete(p.logins, state)

	return loginSession{}, false
}

// loginAccount login/account 端点返回的账号字段。
// type 是账号版本（personal / ultimate / exclusive），官方据此与企业 ID 并列判定
// 企业版（X-Private-Data 等出站头），登录落盘必须保留。
type loginAccount struct {
	UID          string `json:"uid"`
	EnterpriseID string `json:"enterpriseId"`
	EditionType  string `json:"type"`
	Nickname     string `json:"nickname"`
}

// loginPoll 轮询登录态。未完成 → {done:false}；完成 → 建凭证、落盘、热加载、签到。
func (p *Panel) loginPoll(w http.ResponseWriter, r *http.Request) {
	state := r.URL.Query().Get("state")
	if state == "" {
		writeErr(w, http.StatusBadRequest, "missing state")
		return
	}

	sess, egress, client, ok := p.loginPollSession(w, state)
	if !ok {
		return
	}

	defer egress.close(client)

	tok, pendingInfo, pending, endpoint, err := p.pollLoginToken(r.Context(), client, sess.realm, sess.base, state)
	if err != nil {
		// 许可终态会让前端停止轮询；收敛会话，避免状态永久留在 p.logins。
		if loginTokenTerminal(err) {
			p.dropLoginSession(state)
		}

		writeLoginPollError(w, err)

		return
	}

	if pending {
		p.noteLoginPending(state, pendingInfo)
		writeJSON(w, http.StatusOK, map[string]any{"done": false, "message": "waiting for login"})

		return
	}

	acct, ok := p.pollLoginAccount(r.Context(), client, w, &endpoint, state, tok.AccessToken)
	if !ok {
		return
	}

	// 独占认领：lookupLoginSession 只读不占位，会话删除又发生在 postLoginTasks 之后，
	// 并发 poll（双击、两个标签页、前端重试）会各自跑完落盘 + 签到/注册/领 trial。
	// 这里把「取号成功」到「删除会话」之间收窄成单飞。
	if !p.claimLoginSession(state) {
		writeErr(w, http.StatusConflict, "登录结果已由其它请求处理，请刷新账号列表")

		return
	}

	p.finishClaimedLogin(r.Context(), w, state, sess.realm, sess.slot, tok, acct)
}

// loginPollSession 取登录会话并按发起时选定的槽重建出口。返回 false 表示已回写
// 错误响应（state 过期 / 槽已不可用），调用方直接返回。
func (p *Panel) loginPollSession(
	w http.ResponseWriter, state string,
) (loginSession, loginEgress, *http.Client, bool) {
	sess, known := p.lookupLoginSession(state)
	if !known {
		writeErr(w, http.StatusNotFound, "unknown or expired state（请重新发起添加账号）")

		return loginSession{}, loginEgress{}, nil, false
	}

	// 同一 state 的整条链路固定同一条出口：轮询阶段按发起时选定的槽重建拨号器，
	// 槽已不可用即 fail-closed 中止（不换出口、不回落直连）。
	egress, err := p.loginEgressForSlot(sess.slot)
	if err != nil {
		p.dropLoginSession(state)
		writeErr(w, http.StatusBadGateway, err.Error())

		return loginSession{}, loginEgress{}, nil, false
	}

	return sess, egress, egress.client(), true
}

// finishClaimedLogin 落盘凭证并跑登录后置任务（会话已被独占认领）。
func (p *Panel) finishClaimedLogin(
	ctx context.Context, w http.ResponseWriter, state, realm, slot string, tok loginToken, acct loginAccount,
) {
	account, status, msg := p.persistLoginAuth(realm, slot, tok, acct)
	if status != 0 {
		// 非 200 会让前端停止轮询；会话不清掉就得等 TTL 或下次 login/start 才回收。
		p.dropLoginSession(state)
		writeErr(w, status, msg)

		return
	}

	p.finishLoginPoll(ctx, w, state, realm, account, acct)
}

// claimLoginSession 独占认领一次已完成的登录（取号成功后、落盘前）。
// 返回 false 表示会话已被另一并发请求认领或已回收。
func (p *Panel) claimLoginSession(state string) bool {
	p.loginMu.Lock()
	defer p.loginMu.Unlock()

	if _, known := p.logins[state]; !known {
		return false
	}

	delete(p.logins, state)

	return true
}

// dropLoginSession 移除登录会话（成功、终态失败共用）；幂等。
func (p *Panel) dropLoginSession(state string) {
	p.loginMu.Lock()
	delete(p.logins, state)
	p.loginMu.Unlock()
}

// finishLoginPoll 登录成功后的收尾：顺带签到 + 余额刷新（幂等；失败只体现在
// 返回字段里），清理会话并回写结果。
func (p *Panel) finishLoginPoll(
	ctx context.Context, w http.ResponseWriter, state, realm string, account *auth.Auth, acct loginAccount,
) {
	checkinMsg, remain, total := p.postLoginTasks(ctx, account, realm, acct.UID)

	p.dropLoginSession(state)
	log.Printf("panel: 新账号已热加载 uid=%s nickname=%q realm=%s（免重启生效）", acct.UID, acct.Nickname, realm)
	writeJSON(w, http.StatusOK, map[string]any{
		"done":            true,
		"uid":             acct.UID,
		"nickname":        acct.Nickname,
		"realm":           realm,
		"credits":         remain,
		"credits_total":   total,
		"checkin_message": checkinMsg,
	})
}

// pollLoginAccount 取账号信息并回写 pending / 终态响应；返回值 ok=false 表示
// 本轮轮询已处理完毕，调用方应直接返回。其他错误保持忽略（由 uid 校验兜底）。
func (p *Panel) pollLoginAccount(
	ctx context.Context, client *http.Client, w http.ResponseWriter, endpoint *loginEndpoint, state, bearer string,
) (loginAccount, bool) {
	acct, err := p.fetchLoginAccount(ctx, client, endpoint, state, bearer)
	if err == nil {
		return acct, true
	}

	codeErr, ok := errors.AsType[*logincode.Error](err)
	if !ok {
		return acct, true
	}

	// 账号信息尚未就绪：继续按 pending 轮询。
	if logincode.IsPending(codeErr.Code) {
		writeJSON(w, http.StatusOK, map[string]any{"done": false, "message": "waiting for account"})

		return loginAccount{}, false
	}

	// 许可终态：立即失败并透出上游 msg，不再空转到 TTL；同时收敛会话，
	// 前端收到非 200 后会停止轮询，否则条目会永久留在 p.logins。
	if logincode.IsTerminal(codeErr.Code) {
		p.dropLoginSession(state)
		writeLoginPollError(w, err)

		return loginAccount{}, false
	}

	return acct, true
}

// writeLoginPollError 区分部署错误与「等用户完成登录」：profile 缺失不能返回
// 200 done=false，否则前端会无限轮询；其余错误仍按 pending 语义返回。
func writeLoginPollError(w http.ResponseWriter, err error) {
	if errors.Is(err, errLoginProfileMissing) {
		writeErr(w, http.StatusServiceUnavailable, err.Error())

		return
	}

	// 出口不可用不是「等用户完成登录」：返回非 200 让前端停下并展示真实原因，
	// 与 loginEgressForSlot 的 502 同口径（否则前端会每 3s 打一条死代理到 TTL）。
	if errors.Is(err, outbound.ErrProxyDial) || errors.Is(err, outbound.ErrNoHealthySlot) {
		writeErr(w, http.StatusBadGateway, err.Error())

		return
	}

	// 许可终态返回非 200 + 上游 msg，前端停止轮询并展示真实原因。
	if codeErr, ok := errors.AsType[*logincode.Error](err); ok && logincode.IsTerminal(codeErr.Code) {
		msg := codeErr.Msg
		if msg == "" {
			msg = err.Error()
		}

		writeErr(w, http.StatusBadRequest, msg)

		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"done": false, "message": err.Error()})
}

// pollLoginToken 查 auth/token：请求失败返回 err（面板按 pending 处理）；token 尚未
// 就绪返回 pending=true 并带回上游 code/msg；成功返回解析后的凭证与命中的 origin
// （account 端点同源）。
func (p *Panel) pollLoginToken(
	ctx context.Context, client *http.Client, realm, preferredBase, state string,
) (loginToken, loginPendingInfo, bool, loginEndpoint, error) {
	identity := p.loginIdentityFor(realm)
	if identity.userAgent == "" {
		return loginToken{}, loginPendingInfo{}, false, loginEndpoint{}, errLoginProfileMissing
	}

	var lastErr error

	for _, endpoint := range loginCandidatesPreferWithIdentity(realm, preferredBase, identity) {
		tok, pendingInfo, pending, status, err := requestLoginToken(ctx, client, &endpoint, state)
		if err == nil {
			return tok, pendingInfo, pending, endpoint, nil
		}

		lastErr = err
		if loginTokenRetryable(status, err) {
			continue
		}

		return loginToken{}, loginPendingInfo{}, false, loginEndpoint{}, err
	}

	return loginToken{}, loginPendingInfo{}, false, loginEndpoint{}, lastErr
}

// requestLoginToken 请求单个 origin 的 token 端点：pending 业务码与空 token 都归一为
// pending=true 且 err=nil；pending 业务码同时带回 code/msg 供日志使用。许可终态与其他
// 错误原样返回给调用方决定是否换 origin。
func requestLoginToken(
	ctx context.Context, client *http.Client, endpoint *loginEndpoint, state string,
) (loginToken, loginPendingInfo, bool, int, error) {
	tokRaw, status, err := doJSON(
		ctx, client, http.MethodGet, endpoint.token+state, "", nil, endpoint.origin, endpoint.userAgent,
	)
	if err != nil {
		if codeErr, ok := errors.AsType[*logincode.Error](err); ok && logincode.IsPending(codeErr.Code) {
			return loginToken{}, loginPendingInfo{code: codeErr.Code, msg: codeErr.Msg}, true, status, nil
		}

		return loginToken{}, loginPendingInfo{}, false, status, err
	}

	var tok loginToken

	if err := jsonx.Unmarshal(tokRaw, &tok); err != nil {
		log.Printf("WARN: [panel] 登录 token 响应解析失败（按未完成继续轮询）: %v", err)
	}

	if tok.AccessToken == "" {
		return loginToken{}, loginPendingInfo{}, true, status, nil
	}

	return tok, loginPendingInfo{}, false, status, nil
}

// noteLoginPending 按 state 去重记录 pending 的上游 code/msg：相同结果只记一次，
// 变化时再记。去重状态存 loginSession，读写都在 loginMu 下。日志只含 state 前 8 位。
func (p *Panel) noteLoginPending(state string, info loginPendingInfo) {
	if info.code == 0 && info.msg == "" {
		return
	}

	p.loginMu.Lock()

	sess, ok := p.logins[state]
	if !ok {
		p.loginMu.Unlock()

		return
	}

	if sess.pendingLogged && sess.pendingCode == info.code && sess.pendingMsg == info.msg {
		p.loginMu.Unlock()

		return
	}

	sess.pendingLogged = true
	sess.pendingCode = info.code
	sess.pendingMsg = info.msg
	p.logins[state] = sess
	p.loginMu.Unlock()

	//nolint:gosec // state 仅取前 8 位，msg 已用 %q 转义。
	log.Printf("panel: 登录等待中 state=%s... code=%d msg=%q",
		state[:min(8, len(state))], info.code, info.msg)
}

// loginTokenRetryable 报告失败是否值得换 origin：许可终态不换，其余按网关类状态码。
func loginTokenRetryable(status int, err error) bool {
	if loginTokenTerminal(err) {
		return false
	}

	return loginReplayable(status)
}

// loginTokenTerminal 报告 token 轮询错误是否为许可终态。
func loginTokenTerminal(err error) bool {
	codeErr, ok := errors.AsType[*logincode.Error](err)

	return ok && logincode.IsTerminal(codeErr.Code)
}

// fetchLoginAccount 取 uid/nickname；失败返回错误，由 loginPoll 区分 pending / 终态 / 忽略。
func (*Panel) fetchLoginAccount(
	ctx context.Context, client *http.Client, endpoint *loginEndpoint, state, bearer string,
) (loginAccount, error) {
	var acct loginAccount

	acctRaw, _, err := doJSON(
		ctx, client, http.MethodGet, endpoint.account+state, bearer, nil, endpoint.origin, endpoint.userAgent,
	)
	if err != nil {
		return loginAccount{}, err
	}

	if err := jsonx.Unmarshal(acctRaw, &acct); err != nil {
		return loginAccount{}, fmt.Errorf("parse account: %w", err)
	}

	return acct, nil
}

// persistLoginAuth 校验 uid、落盘凭证并热加载进池。返回非零 status 表示失败，
// 调用方据此回写 HTTP 错误；成功时返回落盘后的 auth。
//
// slot 是本次登录链路实际使用的出口槽（空 = 直连登录），落盘后立刻写成账号
// proxy_slot（issue #609）：位置在注册激活 / 签到 / 余额刷新等后续出站动作之前。
func (p *Panel) persistLoginAuth(
	realm, slot string, tok loginToken, acct loginAccount,
) (*auth.Auth, int, string) {
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

	account := newLoginAuth(p.cfg.AuthDir, tok, acct)
	if err := applyLoginRealm(account, realm); err != nil {
		return nil, http.StatusInternalServerError, err.Error()
	}

	// 同一 uid 重新登录：先退役旧实例再落盘。锁是按实例私有的，旧实例此刻尚未被
	// Pool.Add 替换，其进行中的 refresh 可以 SaveAtomic 把**旧** token 写回同一路径，
	// 结果内存是新凭证、磁盘是旧凭证，重启后旧凭证复活。
	if prev := p.cfg.Pool.AuthByUID(acct.UID); prev != nil {
		prev.Retire()
	}

	if err := account.SaveAtomic(); err != nil {
		return nil, http.StatusInternalServerError, "save auth: " + err.Error()
	}

	p.cfg.Pool.Add(account)
	p.cfg.Pool.Revive(acct.UID) // 全新登录 = 人工恢复口径：清掉旧号遗留的禁用/冷却/熔断

	// 注册出口固定绑定：登录用哪条槽注册的，账号就绑哪条，事后可回答「这个号是
	// 哪条出口注册的」（issue #609）。必须在 postLoginTasks 之前，否则注册后的第一
	// 批出站（激活/签到/余额）会换成另一条线路。
	//
	// 已有绑定不动：同一个账号重新登录不该顺手改掉运维手工设好的出口。
	if slot != "" && p.cfg.Pool.ProxySlotOf(acct.UID) == "" {
		if !p.cfg.Pool.SetProxySlot(acct.UID, slot) {
			log.Printf("WARN: [panel] 登录出口绑定失败：账号 %s 不在池中（槽 %s）", acct.UID, slot)
		}
	}

	return account, 0, ""
}

// newLoginAuth 按 token 与账号信息构造待落盘的凭证（嵌套形，与 auths/ 目录既有格式一致）。
func newLoginAuth(dir string, tok loginToken, acct loginAccount) *auth.Auth {
	return &auth.Auth{
		AccessToken:  tok.AccessToken,
		RefreshToken: tok.RefreshToken,
		ExpiresAt:    time.Now().Add(loginExpiry(tok.ExpiresIn)).Unix(),
		Domain:       tok.Domain,
		UID:          acct.UID,
		EnterpriseID: acct.EnterpriseID,
		EditionType:  acct.EditionType,
		Nickname:     acct.Nickname,
		FilePath:     filepath.Join(dir, fmt.Sprintf("workbuddy-%s.json", acct.UID)),
	}
}

// applyLoginRealm 按登录 realm 写 auth.realm 键。global 登录必须落盘 auth.realm=global
// （Realm() 按此判域，不写则依赖 domain 后缀回落）；CN 也显式补一次（幂等），让 auth
// 文件形态统一（与 LoadDir 存量迁移对齐）。
func applyLoginRealm(account *auth.Auth, realm string) error {
	if realm == "global" {
		if _, err := auth.BackfillRealmFor(account, "global"); err != nil {
			return fmt.Errorf("set realm: %w", err)
		}

		return nil
	}

	_, _ = account.BackfillRealm()

	return nil
}

// postLoginTasks 登录完成后的签到/激活与余额刷新，返回 (提示消息, 余额, 总额)。
// realm 分支：CN 走 DailyCheckin；global 登录时改为注册激活 + trial 领取（签到由
// 调度器统一执行，与 CN 同口径）；企业版跳过签到（上游不支持）。
func (p *Panel) postLoginTasks(
	ctx context.Context, account2 *auth.Auth, realm, uid string,
) (string, int64, int64) {
	checkinMsg := ""
	if realm == "global" {
		checkinMsg = p.activateGlobalLogin(account2, uid)
	} else if !account2.IsEnterprise() {
		// 企业版跳过签到：上游 POST /v2/billing/meter/daily-checkin 对企业号
		// 400 code 10001「企业账号不支持该操作」，发了只会把该错误写进登录返回。
		// 下方 UserResource 余额查询照常（企业额度走 get-enterprise-user-usage 口径）。
		//
		// 走回执口径（而非旧的 DailyCheckin）：旧入口把「2xx 但回执读不出来」也算成功，
		// 登录返回会显示「签到成功」而账号当天实际没签；回执把这类形态判 failed。
		if receipt, err := p.cfg.Upstream.DailyCheckinReceipt(ctx, account2); err != nil {
			checkinMsg = err.Error()
		} else if receipt.State == upstream.CheckinStateFailed {
			checkinMsg = receipt.Message
		}
	}

	remain, total := int64(-1), int64(0)
	if rm, tt, err := p.cfg.Upstream.UserResource(ctx, account2); err == nil {
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
func (*Panel) loginRegions(w http.ResponseWriter, r *http.Request) {
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
