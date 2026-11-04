package upstream

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
)

var errGlobalDialerUsed = errors.New("全局拨号器不应被 TLS 路径使用")

// TestTLSFingerprintNodeHandshake node 模式的首连必须与官方抓包同形：无 ALPN、
// 无 GREASE、无证书压缩、无 PSK，JA3/JA4 逐字一致。
func TestTLSFingerprintNodeHandshake(t *testing.T) {
	t.Parallel()

	addr, cert, store := startRawTLSServer(t)
	client := testFingerprintClient(t, addr, cert.Leaf)

	if err := client.SetTLSFingerprint(TLSFingerprintNode); err != nil {
		t.Fatalf("SetTLSFingerprint: %v", err)
	}

	doChat(t, client, "https://www.workbuddy.cn/v2/chat/completions", `{"model":"x"}`)

	conns := store.snapshot()
	if len(conns) == 0 {
		t.Fatal("未捕获到握手连接")
	}

	raw := conns[0].snapshot()
	hello := parseClientHello(t, firstClientHello(t, raw))

	if got, want := hello.ja3(), "71dc8c533dd919ae9f4963224a4ba8fd"; got != want {
		t.Errorf("JA3=%s want %s", got, want)
	}

	if got, want := hello.ja4(), "t13d181000_5d04281c6031_78e6aca7449b"; got != want {
		t.Errorf("JA4=%s want %s", got, want)
	}

	for _, forbidden := range []uint16{16, 21, 27, 41} {
		if slices.Contains(hello.extensions, forbidden) {
			t.Errorf("首连不应带扩展 %d: %v", forbidden, hello.extensions)
		}
	}

	// L2：正常 TLS 1.3 握手不得写 dummy CCS（record type 20）。
	if hasRecordType(t, raw, 20) {
		t.Error("客户端写入了 dummy change_cipher_spec（record type 20）")
	}
}

// TestTLSFingerprintPSKResumption 第二次连接复用会话缓存：ClientHello 末尾依次为
// supported_versions(43)、padding(21)、pre_shared_key(41)，仍是 512 字节，且不写 CCS。
func TestTLSFingerprintPSKResumption(t *testing.T) {
	t.Parallel()

	addr, cert, store := startRawTLSServer(t)
	client := testFingerprintClient(t, addr, cert.Leaf)

	if err := client.SetTLSFingerprint(TLSFingerprintNode); err != nil {
		t.Fatalf("SetTLSFingerprint: %v", err)
	}

	endpoint := "https://www.workbuddy.cn/v2/chat/completions"
	doChatClose(t, client, endpoint, `{"model":"x"}`)
	doChatClose(t, client, endpoint, `{"model":"x"}`)

	conns := store.snapshot()
	if len(conns) != 2 {
		t.Fatalf("连接数=%d want 2（req.Close 应强制新连接）", len(conns))
	}

	second := firstClientHello(t, conns[1].snapshot())
	hello := parseClientHello(t, second)

	if len(hello.extensions) < 3 {
		t.Fatalf("第二次 ClientHello 扩展过少: %v", hello.extensions)
	}

	tail := hello.extensions[len(hello.extensions)-3:]
	if !slices.Equal(tail, []uint16{43, 21, 41}) {
		t.Errorf("扩展尾部=%v want [43 21 41]", tail)
	}

	if len(second) != 512 {
		t.Errorf("第二次 ClientHello 长度=%d want 512", len(second))
	}

	if hasRecordType(t, conns[1].snapshot(), 20) {
		t.Error("恢复握手写入了 dummy change_cipher_spec（record type 20）")
	}
}

// TestTLSDialUsesSlotDialer 槽绑定账号的 TLS 拨号必须走槽拨号器，不得回落到全局
// 拨号器（否则会绕过代理出口）。
func TestTLSDialUsesSlotDialer(t *testing.T) {
	t.Parallel()

	addr, cert, _ := startRawTLSServer(t)
	client := newProfileClient(t)
	client.tlsRootCAs = testRootPool(cert.Leaf)
	client.SetHeaderOrder(true)

	if err := client.SetTLSFingerprint(TLSFingerprintNode); err != nil {
		t.Fatalf("SetTLSFingerprint: %v", err)
	}

	globalCalls := 0

	client.SetDialer(func(context.Context, string, string) (net.Conn, error) {
		globalCalls++

		return nil, errGlobalDialerUsed
	})

	slotCalls := 0

	client.SetAccountRoute(func(*auth.Auth) (string, func(context.Context, string, string) (net.Conn, error)) {
		return "slot-a", func(ctx context.Context, network, _ string) (net.Conn, error) {
			slotCalls++

			var dialer net.Dialer

			return dialer.DialContext(ctx, network, addr)
		}
	})

	account := surfaceGoldenAccount()
	hc := client.HTTPForAccount(account)

	tr, ok := hc.Transport.(*http.Transport)
	if !ok || tr.DialTLSContext == nil {
		t.Fatal("槽 client 缺少自定义 DialTLSContext")
	}

	conn, err := tr.DialTLSContext(context.Background(), "tcp", "www.workbuddy.cn:443")
	if err != nil {
		t.Fatalf("槽 TLS 拨号: %v", err)
	}

	defer conn.Close()

	if slotCalls != 1 {
		t.Errorf("槽拨号次数=%d want 1", slotCalls)
	}

	if globalCalls != 0 {
		t.Errorf("全局拨号次数=%d want 0", globalCalls)
	}
}

// TestTLSFingerprintNoFallbackOnHandshakeFailure 指纹握手失败直接报错，不回落
// 标准库 TLS。
func TestTLSFingerprintNoFallbackOnHandshakeFailure(t *testing.T) {
	t.Parallel()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	defer listener.Close()

	go func() {
		for {
			conn, aerr := listener.Accept()
			if aerr != nil {
				return
			}

			_ = conn.Close()
		}
	}()

	client := newProfileClient(t)
	client.SetHeaderOrder(true)

	if setErr := client.SetTLSFingerprint(TLSFingerprintNode); setErr != nil {
		t.Fatalf("SetTLSFingerprint: %v", setErr)
	}

	client.SetDialer(func(ctx context.Context, network, _ string) (net.Conn, error) {
		var dialer net.Dialer

		return dialer.DialContext(ctx, network, listener.Addr().String())
	})

	tr := client.HTTP.Transport.(*http.Transport)

	_, err = tr.DialTLSContext(context.Background(), "tcp", "www.workbuddy.cn:443")
	if err == nil {
		t.Fatal("对端直接关闭时应返回握手错误")
	}

	if !strings.Contains(err.Error(), "指纹握手失败") {
		t.Fatalf("错误未标明指纹握手: %v", err)
	}
}

// TestTLSFingerprintRequiresProfileTLS node 模式缺 tls 段必须拒绝启用。
func TestTLSFingerprintRequiresProfileTLS(t *testing.T) {
	t.Parallel()

	if err := New().SetTLSFingerprint(TLSFingerprintNode); err == nil {
		t.Fatal("无 profile 时应报错")
	}

	client := newProfileClient(t)
	client.Profile.ForRealm("cn").TLS = nil
	client.Profile.ForRealm("global").TLS = nil

	if err := client.SetTLSFingerprint(TLSFingerprintNode); err == nil {
		t.Fatal("profile 缺 tls 段时应报错")
	}

	if err := client.SetTLSFingerprint("chrome"); err == nil {
		t.Fatal("未知模式应报错")
	}
}

// TestHeaderOrderOnlyUsesGoTLS 只开头序时仍走标准库 TLS：证书校验生效、无 uTLS。
func TestHeaderOrderOnlyUsesGoTLS(t *testing.T) {
	t.Parallel()

	addr, cert, _ := startRecordingTLSServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))

	client := testFingerprintClient(t, addr, cert.Leaf)

	if client.FingerprintMode() != TLSFingerprintOff {
		t.Fatalf("mode=%q want off", client.FingerprintMode())
	}

	tr := client.HTTP.Transport.(*http.Transport)
	if tr.DialTLSContext == nil {
		t.Fatal("header_order 开启时应安装自定义 TLS 拨号")
	}

	doChat(t, client, "https://"+addr+"/v2/chat/completions", `{"model":"x"}`)

	// 证书不受信时必须失败（证明没有跳过校验）。
	untrusted := testFingerprintClient(t, addr, cert.Leaf)
	untrusted.tlsRootCAs = nil

	req, err := http.NewRequest(http.MethodPost, "https://"+addr+"/v2/chat/completions",
		bytes.NewReader([]byte(`{"model":"x"}`)))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}

	resp, err := untrusted.HTTP.Do(req)
	if err == nil {
		_ = resp.Body.Close()

		t.Fatal("自签证书未注入根池时应校验失败")
	}
}

// TestSetDialTLSDisabledWhenAllOff 两个开关都关时不安装 DialTLSContext。
func TestSetDialTLSDisabledWhenAllOff(t *testing.T) {
	t.Parallel()

	client := New()
	client.SetHeaderOrder(true)

	if client.HTTP.Transport.(*http.Transport).DialTLSContext == nil {
		t.Fatal("header_order 开启时应安装")
	}

	client.SetHeaderOrder(false)

	if client.HTTP.Transport.(*http.Transport).DialTLSContext != nil {
		t.Fatal("两个开关都关时应移除自定义 DialTLSContext")
	}
}
