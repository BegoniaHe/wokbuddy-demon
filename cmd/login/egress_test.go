package main

import (
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BegoniaHe/wokbuddy-demon/internal/outbound"
)

// newLoginUpstream 起覆盖 state/token/account 的假登录上游，返回命中的路径序列。
func newLoginUpstream(t *testing.T) (*httptest.Server, func() []string) {
	t.Helper()

	var (
		mu   sync.Mutex
		seen []string
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()

		seen = append(seen, r.URL.Path)

		switch r.URL.Path {
		case "/v2/plugin/auth/state":
			_, _ = w.Write([]byte(`{"code":0,"data":{"state":"s-1","authUrl":"https://example.invalid/auth"}}`))
		case "/v2/plugin/auth/token":
			_, _ = w.Write([]byte(`{"code":0,"data":{"accessToken":"tok","refreshToken":"ref","expiresIn":3600}}`))
		case "/v2/plugin/login/account":
			_, _ = w.Write([]byte(`{"code":0,"data":{"uid":"u-1","nickname":"n"}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	hits := func() []string {
		mu.Lock()
		defer mu.Unlock()

		return append([]string(nil), seen...)
	}

	return srv, hits
}

// loginTunnelProxy 本地假 CONNECT 代理：把隧道接到路由表里的真实地址。
type loginTunnelProxy struct {
	addr string

	mu      sync.Mutex
	connect []string
}

func newLoginTunnelProxy(t *testing.T, routes map[string]string) *loginTunnelProxy {
	t.Helper()

	proxy := &loginTunnelProxy{}

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

		proxy.mu.Lock()
		proxy.connect = append(proxy.connect, r.Host)
		proxy.mu.Unlock()

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

func (p *loginTunnelProxy) targets() []string {
	p.mu.Lock()
	defer p.mu.Unlock()

	return append([]string(nil), p.connect...)
}

// deadLoginAddr 返回一个拒绝连接的本地地址（监听后立即关闭）。
func deadLoginAddr(t *testing.T) string {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	addr := listener.Addr().String()
	listener.Close()

	return addr
}

// makeLoginSlot 造一条配置里的出口槽。
func makeLoginSlot(id, rawURL string, enabled bool) loginSlot {
	return loginSlot{ID: id, Name: id, URL: rawURL, Enabled: enabled}
}

// loginConfigWith 造一份开启登录出口绑定的配置。
func loginConfigWith(slots ...loginSlot) loginConfig {
	return loginConfig{bindProxy: true, Slots: slots}
}

// TestLoginCLISlotEgress 配一条槽时 url/poll 三条请求都经该槽出站（issue #609）。
// 判据同面板：登录基址是不监听的本机地址，隧道代理把它换成本地假上游。
//
//nolint:paralleltest // 共享包级 stateFile 目录与假代理，串行更稳
func TestLoginCLISlotEgress(t *testing.T) {
	srv, hits := newLoginUpstream(t)

	target := deadLoginAddr(t)
	proxy := newLoginTunnelProxy(t, map[string]string{target: srv.Listener.Addr().String()})

	client, slot, err := loginEgressClient(loginConfigWith(makeLoginSlot("egress", "http://"+proxy.addr, true)), realmCN)
	if err != nil {
		t.Fatalf("loginEgressClient: %v", err)
	}

	if slot != "egress" {
		t.Fatalf("slot=%q want egress", slot)
	}

	runLoginFlow(t, "http://"+target, client)

	if got := len(hits()); got != 3 {
		t.Fatalf("上游登录请求数=%d want 3", got)
	}

	if len(proxy.targets()) == 0 {
		t.Fatal("登录请求未经代理槽出站")
	}
}

// TestLoginCLINoSlotIsDirect 花名册一条启用槽都没有 → 直连并登录成功。
func TestLoginCLINoSlotIsDirect(t *testing.T) {
	t.Parallel()

	srv, hits := newLoginUpstream(t)

	client, slot, err := loginEgressClient(loginConfigWith(), realmCN)
	if err != nil || slot != "" {
		t.Fatalf("slot=%q err=%v want 直连", slot, err)
	}

	runLoginFlow(t, srv.URL, client)

	if got := len(hits()); got != 3 {
		t.Fatalf("上游登录请求数=%d want 3", got)
	}
}

// TestLoginCLISwitchOffIsDirect features.login_bind_proxy=false → 直连。
func TestLoginCLISwitchOffIsDirect(t *testing.T) {
	t.Parallel()

	srv, hits := newLoginUpstream(t)

	proxy := newLoginTunnelProxy(t, map[string]string{srv.Listener.Addr().String(): srv.Listener.Addr().String()})

	config := loginConfigWith(makeLoginSlot("egress", "http://"+proxy.addr, true))
	config.bindProxy = false

	client, slot, err := loginEgressClient(config, realmCN)
	if err != nil || slot != "" {
		t.Fatalf("slot=%q err=%v want 直连", slot, err)
	}

	runLoginFlow(t, srv.URL, client)

	if got := len(hits()); got != 3 {
		t.Fatalf("上游登录请求数=%d want 3", got)
	}

	if targets := proxy.targets(); len(targets) != 0 {
		t.Fatalf("开关关闭时不得经代理出站: %v", targets)
	}
}

// TestLoginCLIBrokenSlotFailsClosed 有启用槽但都不可用 → 报错，绝不回落直连；
// 错误文案不含代理口令。
func TestLoginCLIBrokenSlotFailsClosed(t *testing.T) {
	t.Parallel()

	client, slot, err := loginEgressClient(
		loginConfigWith(makeLoginSlot("broken", "ftp://user:s3cr3t-pass@127.0.0.1:1", true)), realmCN)
	if !errors.Is(err, outbound.ErrNoHealthySlot) || client != nil || slot != "" {
		t.Fatalf("slot=%q client=%v err=%v want ErrNoHealthySlot", slot, client != nil, err)
	}

	if !strings.Contains(err.Error(), "不回落直连") {
		t.Fatalf("错误文案应说明不回落直连: %v", err)
	}

	for _, secret := range []string{"s3cr3t-pass", "user:"} {
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("错误文案回显了代理口令: %v", err)
		}
	}
}

// TestLoadLoginConfigMissingFile 配置文件不存在 = 未配代理（直连），但开关仍缺省 true：
// 首次部署配置还没生成时登录不该失败。
func TestLoadLoginConfigMissingFile(t *testing.T) {
	t.Parallel()

	missing, err := loadLoginConfig(filepath.Join(t.TempDir(), "absent.json"))
	if err != nil || !missing.bindProxy || len(missing.Slots) != 0 {
		t.Fatalf("缺文件 bindProxy=%v slots=%d err=%v want true/0", missing.bindProxy, len(missing.Slots), err)
	}
}

// TestLoadLoginConfig 配置读取：开关键缺席保持 true；显式 false 生效；坏配置报错
// （不得静默按直连出网）。
func TestLoadLoginConfig(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		body      string
		wantBind  bool
		wantSlots int
		wantRealm string
		wantErr   bool
	}{
		{
			name:      "开关缺席保持 true",
			body:      `{"upstream":{"proxy_slots":[{"id":"a","url":"http://127.0.0.1:1","enabled":true}],"realm_proxy_slots":{"cn":"a"}}}`,
			wantBind:  true,
			wantSlots: 1,
			wantRealm: "a",
		},
		{name: "显式 false", body: `{"features":{"login_bind_proxy":false}}`},
		{name: "坏配置报错", body: `{`, wantErr: true},
	}

	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			t.Parallel()

			path := filepath.Join(t.TempDir(), "config.json")
			if werr := os.WriteFile(path, []byte(item.body), 0o600); werr != nil {
				t.Fatal(werr)
			}

			config, err := loadLoginConfig(path)
			if item.wantErr {
				if err == nil {
					t.Fatal("坏配置应报错")
				}

				return
			}

			if err != nil {
				t.Fatal(err)
			}

			if config.bindProxy != item.wantBind || len(config.Slots) != item.wantSlots {
				t.Fatalf("bindProxy=%v slots=%d want %v/%d", config.bindProxy, len(config.Slots), item.wantBind, item.wantSlots)
			}

			if config.RealmSlots["cn"] != item.wantRealm {
				t.Fatalf("realm cn=%q want %q", config.RealmSlots["cn"], item.wantRealm)
			}
		})
	}
}

// runLoginFlow 走一遍 CLI 的 url + poll 两段（state 落临时目录，避免污染 /tmp）。
// state 路径按调用传入而非改包级 stateFile：用例并行时不共享全局状态。
func runLoginFlow(t *testing.T, base string, client *http.Client) {
	t.Helper()

	statePath := filepath.Join(t.TempDir(), "state.json")
	identity := loginIdentity{userAgent: "ua", platform: "workbuddy"}

	// 两段各自有 30s 超时；这里收紧总时长，逻辑失败会立刻返回而不是等满超时。
	client.Timeout = 10 * time.Second

	runURL(base, originRefererCN, realmCN, statePath, identity, client, io.Discard)
	runPoll(base, originRefererCN, realmCN, statePath, identity, client, io.Discard)
}
