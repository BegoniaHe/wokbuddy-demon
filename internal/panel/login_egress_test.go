package panel

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
	"github.com/BegoniaHe/wokbuddy-demon/internal/clientprofile"
	"github.com/BegoniaHe/wokbuddy-demon/internal/jsonx"
	"github.com/BegoniaHe/wokbuddy-demon/internal/livecfg"
	"github.com/BegoniaHe/wokbuddy-demon/internal/outbound"
	"github.com/BegoniaHe/wokbuddy-demon/internal/pool"
	"github.com/BegoniaHe/wokbuddy-demon/internal/upstream"
)

// TestLoginSlotEgressesThroughSlot 配一条代理槽时，auth/state → auth/token →
// login/account 三条请求都必须经该槽出站（issue #609）。
//
// 判据是「只有经代理才可达」：登录基址指向一个**不监听**的本机地址，隧道代理把该
// 目标换成本地假上游。任一条请求走直连都会 ECONNREFUSED，登录必然失败。
//
//nolint:paralleltest // 串行替换包级登录基址，避免与其它登录用例互相干扰
func TestLoginSlotEgressesThroughSlot(t *testing.T) {
	loginSrv, loginHits := newFakeLoginUpstream(t)
	billingSrv, billingHits := newFakeBillingUpstream(t)

	loginTarget := deadLoopbackAddr(t)
	billingTarget := deadLoopbackAddr(t)
	proxy := newFakeTunnelProxy(t, map[string]string{
		loginTarget:   listenersAddr(loginSrv),
		billingTarget: listenersAddr(billingSrv),
	})
	redirectLoginBases(t, "http://"+loginTarget)

	panel, state, _ := newEgressPanel(t, proxy.addr, livecfg.Snapshot{LoginBindProxy: true}, "http://"+billingTarget)

	stateID := panel.startLogin(t)
	rec := panel.pollLogin(t, stateID)

	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"done":true`) {
		t.Fatalf("poll status=%d body=%s", rec.Code, rec.Body.String())
	}

	if got := atomic.LoadInt64(loginHits); got != 3 {
		t.Fatalf("上游登录请求数=%d want 3（state/token/account）", got)
	}

	for _, target := range []string{loginTarget, billingTarget} {
		if hits := proxy.hits(target); hits == 0 {
			t.Fatalf("目标 %s 未经代理（proxy=%v）", target, proxy.targets())
		}
	}

	// 登录链路注册出口固定绑定：落盘后账号带 proxy_slot。
	if got := state.ProxySlotOf("u-proxy"); got != "egress" {
		t.Fatalf("proxy_slot=%q want egress", got)
	}

	// 绑定在 post-login 出站之前生效：签到/余额已走该槽（否则会打直连死地址）。
	if got := atomic.LoadInt64(billingHits); got == 0 {
		t.Fatalf("post-login 出站未走槽（billing 直连请求数=%d，proxy=%v）", got, proxy.targets())
	}
}

// TestLoginSlotBindingPersistsToPool 账号绑定要落进池状态（state.json 同源），
// 重启后由 pool 恢复，不依赖面板会话。
//
//nolint:paralleltest // 串行替换包级登录基址
func TestLoginSlotBindingPersistsToPool(t *testing.T) {
	loginSrv, _ := newFakeLoginUpstream(t)
	billingSrv, _ := newFakeBillingUpstream(t)

	loginTarget := deadLoopbackAddr(t)
	proxy := newFakeTunnelProxy(t, map[string]string{
		loginTarget:               listenersAddr(loginSrv),
		listenersAddr(billingSrv): listenersAddr(billingSrv),
	})
	redirectLoginBases(t, "http://"+loginTarget)

	panel, state, _ := newEgressPanel(t, proxy.addr, livecfg.Snapshot{LoginBindProxy: true}, billingSrv.URL)

	stateID := panel.startLogin(t)
	panel.pollLogin(t, stateID)

	account := state.AuthByUID("u-proxy")
	if account == nil {
		t.Fatal("登录完成后账号未进池")
	}

	if got := state.ProxySlotOf(account.UID); got != "egress" {
		t.Fatalf("proxy_slot=%q want egress", got)
	}
}

// TestLoginWithoutSlotIsDirect 池为空（一条启用槽都没有）→ 直连并加号成功：
// 未配代理的部署不能因此加不了号。
//
//nolint:paralleltest // 串行替换包级登录基址
func TestLoginWithoutSlotIsDirect(t *testing.T) {
	loginSrv, loginHits := newFakeLoginUpstream(t)
	billingSrv, _ := newFakeBillingUpstream(t)

	loginTarget := listenersAddr(loginSrv)
	proxy := newFakeTunnelProxy(t, map[string]string{loginTarget: loginTarget})
	redirectLoginBases(t, "http://"+loginTarget)

	panel, state, _ := newEgressPanel(t, "", livecfg.Snapshot{LoginBindProxy: true}, billingSrv.URL)

	stateID := panel.startLogin(t)
	rec := panel.pollLogin(t, stateID)

	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"done":true`) {
		t.Fatalf("poll status=%d body=%s", rec.Code, rec.Body.String())
	}

	if got := atomic.LoadInt64(loginHits); got != 3 {
		t.Fatalf("上游登录请求数=%d want 3", got)
	}

	if targets := proxy.targets(); len(targets) != 0 {
		t.Fatalf("无槽部署不得经代理出站，proxy=%v", targets)
	}

	if got := state.ProxySlotOf("u-proxy"); got != "" {
		t.Fatalf("无槽部署不得绑定出口，proxy_slot=%q", got)
	}
}

// TestLoginUnhealthySlotFailsClosed 有启用槽但全部不健康 → 登录明确报代理错误，
// 绝不静默回落直连（直连在这里是可达的：基址就是假上游，回落会让用例「登录成功」）。
//
//nolint:paralleltest // 串行替换包级登录基址与 logger
func TestLoginUnhealthySlotFailsClosed(t *testing.T) {
	loginSrv, loginHits := newFakeLoginUpstream(t)

	loginTarget := listenersAddr(loginSrv)
	proxy := newFakeTunnelProxy(t, map[string]string{loginTarget: loginTarget})
	redirectLoginBases(t, "http://"+loginTarget)

	var logs bytes.Buffer

	prevOutput, prevFlags := log.Writer(), log.Flags()

	log.SetOutput(&logs)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(prevOutput)
		log.SetFlags(prevFlags)
	})

	const slotURL = "http://user:s3cr3t-pass@" + "127.0.0.1:1"

	panel, _, _ := newEgressPanel(t, proxy.addr, livecfg.Snapshot{LoginBindProxy: true}, loginTarget)
	panel.cfg.Upstream.SetProxyRegistry(cooldownRegistry(t, slotURL))

	rec := httptest.NewRecorder()
	panel.loginStart(rec, httptest.NewRequest(http.MethodPost, "/panel/api/login/start",
		strings.NewReader(`{"realm":"cn"}`)))

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status=%d want 502 body=%s", rec.Code, rec.Body.String())
	}

	body := rec.Body.String()
	if !strings.Contains(body, "不回落直连") || !strings.Contains(body, outbound.ErrNoHealthySlot.Error()) {
		t.Fatalf("错误文案应说明代理槽不可用且不回落直连: %s", body)
	}

	if got := atomic.LoadInt64(loginHits); got != 0 {
		t.Fatalf("fail-closed 后仍打了上游 %d 次（静默回落直连）", got)
	}

	if targets := proxy.targets(); len(targets) != 0 {
		t.Fatalf("不该有出站，proxy=%v", targets)
	}

	for _, secret := range []string{"s3cr3t-pass", "user:"} {
		if strings.Contains(body, secret) || strings.Contains(logs.String(), secret) {
			t.Fatalf("代理口令进入错误文案或日志: body=%s logs=%s", body, logs.String())
		}
	}
}

// TestLoginSwitchOffIsDirect features.login_bind_proxy=false → 直连（配了槽也不用）。
//
//nolint:paralleltest // 串行替换包级登录基址
func TestLoginSwitchOffIsDirect(t *testing.T) {
	loginSrv, loginHits := newFakeLoginUpstream(t)
	billingSrv, _ := newFakeBillingUpstream(t)

	loginTarget := listenersAddr(loginSrv)
	proxy := newFakeTunnelProxy(t, map[string]string{loginTarget: loginTarget})
	redirectLoginBases(t, "http://"+loginTarget)

	panel, state, _ := newEgressPanel(t, proxy.addr, livecfg.Snapshot{LoginBindProxy: false}, billingSrv.URL)

	stateID := panel.startLogin(t)
	rec := panel.pollLogin(t, stateID)

	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"done":true`) {
		t.Fatalf("poll status=%d body=%s", rec.Code, rec.Body.String())
	}

	if got := atomic.LoadInt64(loginHits); got != 3 {
		t.Fatalf("上游登录请求数=%d want 3", got)
	}

	if targets := proxy.targets(); len(targets) != 0 {
		t.Fatalf("开关关闭时不得经代理出站，proxy=%v", targets)
	}

	if got := state.ProxySlotOf("u-proxy"); got != "" {
		t.Fatalf("开关关闭时不得绑定出口，proxy_slot=%q", got)
	}
}

// startLogin 走一次真实 login/start，返回 state。
func (p *Panel) startLogin(t *testing.T) string {
	t.Helper()

	rec := httptest.NewRecorder()
	p.loginStart(rec, httptest.NewRequest(http.MethodPost, "/panel/api/login/start",
		strings.NewReader(`{"realm":"cn"}`)))

	if rec.Code != http.StatusOK {
		t.Fatalf("login start status=%d body=%s", rec.Code, rec.Body.String())
	}

	var item struct {
		State string `json:"state"`
	}
	if err := jsonx.Unmarshal(rec.Body.Bytes(), &item); err != nil || item.State == "" {
		t.Fatalf("parse start body=%s err=%v", rec.Body.String(), err)
	}

	return item.State
}

// pollLogin 轮询一次登录态。
func (p *Panel) pollLogin(t *testing.T, state string) *httptest.ResponseRecorder {
	t.Helper()

	rec := httptest.NewRecorder()
	p.loginPoll(rec, httptest.NewRequest(http.MethodGet, "/panel/api/login/poll?state="+state, nil))

	return rec
}

// newEgressPanel 构造带真实出站注册表的面板：proxyAddr 为空 = 一条槽都没有。
// billingBase 是 post-login（签到/余额）用的 billing base，必须是本地假上游或只有
// 经代理才可达的地址——留空会落到官方域，用例就会打真实外网。
func newEgressPanel(
	t *testing.T, proxyAddr string, snapshot livecfg.Snapshot, billingBase string,
) (*Panel, *pool.Pool, *outbound.Registry) {
	t.Helper()

	profiles, err := clientprofile.LoadDir("../upstream/testdata/profile")
	if err != nil {
		t.Fatalf("load test profiles: %v", err)
	}

	slots := []outbound.Slot{}
	if proxyAddr != "" {
		slots = append(slots, outbound.Slot{ID: "egress", URL: "http://" + proxyAddr, Enabled: true})
	}

	reg, err := outbound.NewRegistry(nil, slots, nil)
	if err != nil {
		t.Fatalf("build registry: %v", err)
	}

	upstreamClient := &upstream.Client{
		Profile: profiles,
		// 非 nil Transport：upstream 的按槽 client 只能克隆 *http.Transport，
		// 用默认 RoundTripper 会让槽路由静默回落直连（这正是本用例要抓的行为）。
		HTTP: &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{}},
	}
	upstreamClient.SetProxyRegistry(reg)

	state := pool.New("")
	// 与 cmd/server 的账号选路同口径：账号绑定槽优先，未绑定 = 直连。
	upstreamClient.SetAccountRoute(func(account *auth.Auth) (string, func(context.Context, string, string) (net.Conn, error)) {
		slot := state.ProxySlotOf(account.UID)
		if slot == "" {
			return "", nil
		}

		dial, ok := reg.SlotDial(slot)
		if !ok {
			return "", nil
		}

		return slot, dial
	})

	upstreamClient.BillingBaseCN = billingBase
	upstreamClient.ChatBaseCN = billingBase

	panel := New(&Config{
		Pool:     state,
		Upstream: upstreamClient,
		AuthDir:  t.TempDir(),
		Live:     livecfg.New(snapshot),
	})

	return panel, state, reg
}

// cooldownRegistry 构造只含一条全冷却槽的注册表：先拨一次死地址把唯一代理推进
// 冷却（健康快照 CooldownSec > 0），随后 LoginSlot 必须 fail-closed。
func cooldownRegistry(t *testing.T, slotURL string) *outbound.Registry {
	t.Helper()

	reg, err := outbound.NewRegistry(nil, []outbound.Slot{
		{ID: "egress", URL: slotURL, Enabled: true},
	}, nil)
	if err != nil {
		t.Fatalf("build registry: %v", err)
	}

	if _, derr := reg.SlotPool("egress").DialContext(context.Background(), "tcp", "127.0.0.1:9"); derr == nil {
		t.Fatal("拨死代理应失败")
	}

	return reg
}

// fakeTunnelProxy 本地假 CONNECT 代理：按 CONNECT 目标把隧道接到对应假上游。
type fakeTunnelProxy struct {
	addr string

	mu      sync.Mutex
	connect []string
}

// newFakeTunnelProxy 起代理：routes 的键是客户端请求的 CONNECT 目标，值是真实落地地址。
func newFakeTunnelProxy(t *testing.T, routes map[string]string) *fakeTunnelProxy {
	t.Helper()

	proxy := &fakeTunnelProxy{}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect {
			http.Error(w, "仅支持 CONNECT", http.StatusMethodNotAllowed)

			return
		}

		target, ok := routes[r.Host]
		if !ok {
			http.Error(w, "未知 CONNECT 目标", http.StatusBadGateway)

			return
		}

		proxy.record(r.Host)

		upstreamConn, err := net.Dial("tcp", target)
		if err != nil {
			http.Error(w, "上游不可达", http.StatusBadGateway)

			return
		}

		hijacker, ok := w.(http.Hijacker)
		if !ok {
			upstreamConn.Close()
			http.Error(w, "不支持 hijack", http.StatusInternalServerError)

			return
		}

		clientConn, _, herr := hijacker.Hijack()
		if herr != nil {
			upstreamConn.Close()

			return
		}

		_, _ = clientConn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))

		go func() {
			_, _ = io.Copy(upstreamConn, clientConn)
			upstreamConn.Close()
		}()

		_, _ = io.Copy(clientConn, upstreamConn)
		clientConn.Close()
	}))
	t.Cleanup(srv.Close)

	proxy.addr = strings.TrimPrefix(srv.URL, "http://")

	return proxy
}

func (p *fakeTunnelProxy) record(host string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.connect = append(p.connect, host)
}

func (p *fakeTunnelProxy) hits(host string) int {
	p.mu.Lock()
	defer p.mu.Unlock()

	out := 0

	for _, item := range p.connect {
		if item == host {
			out++
		}
	}

	return out
}

func (p *fakeTunnelProxy) targets() []string {
	p.mu.Lock()
	defer p.mu.Unlock()

	return append([]string(nil), p.connect...)
}

// newFakeLoginUpstream 假登录上游：三条设备授权端点都回成功信封。
func newFakeLoginUpstream(t *testing.T) (*httptest.Server, *int64) {
	t.Helper()

	var hits int64

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&hits, 1)
		w.Header().Set("Content-Type", "application/json")

		switch {
		case strings.HasSuffix(r.URL.Path, "/v2/plugin/auth/state"):
			_, _ = w.Write([]byte(`{"code":0,"data":{"state":"state-done","authUrl":"https://example.test/authorize"}}`))
		case strings.HasSuffix(r.URL.Path, "/v2/plugin/auth/token"):
			_, _ = w.Write([]byte(`{"code":0,"data":{"accessToken":"at","refreshToken":"rt","expiresIn":3600}}`))
		default:
			_, _ = w.Write([]byte(`{"code":0,"data":{"uid":"u-proxy","nickname":"proxy-buddy"}}`))
		}
	}))
	t.Cleanup(srv.Close)

	return srv, &hits
}

// newFakeBillingUpstream 假 billing 上游（post-login 签到/余额）。
func newFakeBillingUpstream(t *testing.T) (*httptest.Server, *int64) {
	t.Helper()

	var hits int64

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt64(&hits, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"data":{}}`))
	}))
	t.Cleanup(srv.Close)

	return srv, &hits
}

// listenersAddr 返回 httptest 服务监听地址（host:port）。
func listenersAddr(srv *httptest.Server) string {
	return srv.Listener.Addr().String()
}

// deadLoopbackAddr 返回一个拒绝连接的本地地址（监听后立即关闭，端口随即不可达）。
func deadLoopbackAddr(t *testing.T) string {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	addr := listener.Addr().String()
	listener.Close()

	return addr
}

// redirectLoginBases 把登录端点基址改到本地假上游（var 已在生产代码里预留该缝隙：
// 仓库不内置可注入端口，测试必须能改到回环地址）。
func redirectLoginBases(t *testing.T, base string) {
	t.Helper()

	oldCN, oldGlobal, oldFallback := upstreamBaseCN, upstreamBaseGlobal, cnLoginFallbackBases

	upstreamBaseCN, upstreamBaseGlobal = base, base
	cnLoginFallbackBases = nil

	t.Cleanup(func() {
		upstreamBaseCN, upstreamBaseGlobal, cnLoginFallbackBases = oldCN, oldGlobal, oldFallback
	})
}

// TestLoginKeepsExistingProxySlotBinding 重新登录不覆盖人工绑定：只有账号尚无绑定
// 时才把本次出口写成 proxy_slot（issue #609 审查）。
//
//nolint:paralleltest // 串行替换包级登录基址
func TestLoginKeepsExistingProxySlotBinding(t *testing.T) {
	loginSrv, _ := newFakeLoginUpstream(t)
	billingSrv, _ := newFakeBillingUpstream(t)

	loginTarget := deadLoopbackAddr(t)
	proxy := newFakeTunnelProxy(t, map[string]string{
		loginTarget:               listenersAddr(loginSrv),
		listenersAddr(billingSrv): listenersAddr(billingSrv),
	})
	redirectLoginBases(t, "http://"+loginTarget)

	panel, state, _ := newEgressPanel(t, proxy.addr, livecfg.Snapshot{LoginBindProxy: true}, billingSrv.URL)

	panel.pollLogin(t, panel.startLogin(t))

	const uid = "u-proxy"
	if got := state.ProxySlotOf(uid); got != "egress" {
		t.Fatalf("首次登录应绑定本次出口: proxy_slot=%q", got)
	}

	if !state.SetProxySlot(uid, "manual") {
		t.Fatal("预置人工绑定失败（账号不在池中）")
	}

	panel.pollLogin(t, panel.startLogin(t))

	if got := state.ProxySlotOf(uid); got != "manual" {
		t.Fatalf("重新登录不得覆盖人工绑定: proxy_slot=%q", got)
	}
}

// TestLoginBindProxyDefaultsOnWithoutLive 未装配 Live 快照时按「开启」理解，不能按
// 零值直连——那正是配了代理槽却把宿主机 IP 交出去的形态。
func TestLoginBindProxyDefaultsOnWithoutLive(t *testing.T) {
	t.Parallel()

	var nilPanel *Panel
	if !nilPanel.loginBindProxy() {
		t.Fatal("nil Panel 必须视为开启")
	}

	if !(&Panel{}).loginBindProxy() {
		t.Fatal("缺失 Live 快照必须视为开启")
	}
}

// TestLoginPollProxyFailureIs502 出口故障是部署错误，不能按 pending 让前端空转到 TTL。
func TestLoginPollProxyFailureIs502(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	writeLoginPollError(rec, fmt.Errorf("doJSON: %w", outbound.ErrProxyDial))

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("code=%d want 502", rec.Code)
	}
}
