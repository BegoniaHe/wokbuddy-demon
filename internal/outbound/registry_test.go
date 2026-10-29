package outbound

import (
	"context"
	"strings"
	"testing"
)

func TestParseFourPartProxy(t *testing.T) {
	t.Parallel()

	proxy, err := ParseProxy("1.2.3.4:1080:user:pa:ss")
	if err != nil {
		t.Fatalf("ParseProxy: %v", err)
	}

	if proxy.scheme != "socks5" {
		t.Fatalf("scheme=%q want socks5", proxy.scheme)
	}

	if proxy.addr != "1.2.3.4:1080" {
		t.Fatalf("addr=%q want 1.2.3.4:1080", proxy.addr)
	}

	if proxy.user != "user" || proxy.pass != "pa:ss" {
		t.Fatalf("user=%q pass=%q want user/pa:ss", proxy.user, proxy.pass)
	}

	if masked := proxy.Masked(); strings.Contains(masked, "pa:ss") || strings.Contains(masked, "user") {
		t.Fatalf("Masked()=%q 不得包含凭据", masked)
	}
}

func TestParseFourPartProxyErrorsRedactPassword(t *testing.T) {
	t.Parallel()

	cases := []string{
		"1.2.3.4:0:user:secret",     // 端口越界
		"1.2.3.4:70000:user:secret", // 端口越界
		"1.2.3.4:abc:user:secret",   // 端口非数字
		":1080:user:secret",         // 缺 host
	}

	for _, raw := range cases {
		_, err := ParseProxy(raw)
		if err == nil {
			t.Fatalf("ParseProxy(%q) 应报错", raw)
		}

		if strings.Contains(err.Error(), "secret") {
			t.Fatalf("错误回显了代理密码: %v", err)
		}
	}
}

func TestPoolHealthRecordsFailure(t *testing.T) {
	t.Parallel()

	pool, err := New([]string{"http://" + deadAddr(t)})
	if err != nil {
		t.Fatal(err)
	}

	if _, derr := pool.DialContext(context.Background(), "tcp", "127.0.0.1:9"); derr == nil {
		t.Fatal("dial 应失败")
	}

	health := pool.Health()
	if len(health) != 1 {
		t.Fatalf("len(health)=%d want 1", len(health))
	}

	item := health[0]
	if item.Failures != 1 {
		t.Fatalf("Failures=%d want 1", item.Failures)
	}

	if item.CooldownSec <= 0 {
		t.Fatalf("CooldownSec=%d want >0", item.CooldownSec)
	}

	if item.LastError == "" || item.LastFailAt == "" {
		t.Fatalf("LastError/LastFailAt 应被记录: %+v", item)
	}

	if item.LastOK != "" {
		t.Fatalf("LastOK 应为空: %+v", item)
	}
}

func TestPoolHealthRecordsSuccess(t *testing.T) {
	t.Parallel()

	proxy := connectProxy(t)
	defer proxy.Close()

	target := helloServer(t)
	defer target.Close()

	pool, err := New([]string{proxy.URL})
	if err != nil {
		t.Fatal(err)
	}

	conn, err := pool.DialContext(context.Background(), "tcp", target.Listener.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}

	conn.Close()

	item := pool.Health()[0]
	if item.LastOK == "" {
		t.Fatalf("LastOK 应被记录: %+v", item)
	}

	if item.LastError != "" || item.Failures != 0 || item.CooldownSec != 0 {
		t.Fatalf("成功应清零失败与冷却: %+v", item)
	}
}

func TestNewRegistrySlotViews(t *testing.T) {
	t.Parallel()

	reg, err := NewRegistry(nil, []Slot{
		{ID: "a", Name: "A", URL: "socks5://user:secret@1.2.3.4:1080", Enabled: true},
		{ID: "b", Name: "B", URL: "http://5.6.7.8:8080", Enabled: false},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}

	views := reg.Slots()
	if len(views) != 2 {
		t.Fatalf("len(views)=%d want 2（含禁用槽）", len(views))
	}

	if views[0].URL != "socks5://1.2.3.4:1080" {
		t.Fatalf("views[0].URL=%q want 脱敏地址", views[0].URL)
	}

	if views[1].Enabled || views[1].URL != "http://5.6.7.8:8080" {
		t.Fatalf("views[1]=%+v want 禁用且带脱敏地址", views[1])
	}
}

func TestNewRegistrySlotDials(t *testing.T) {
	t.Parallel()

	reg, err := NewRegistry(nil, []Slot{
		{ID: "a", URL: "socks5://1.2.3.4:1080", Enabled: true},
		{ID: "b", URL: "http://5.6.7.8:8080", Enabled: false},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}

	if _, ok := reg.SlotDial("b"); ok {
		t.Fatal("禁用槽不得产出拨号器")
	}

	if _, ok := reg.SlotDial("missing"); ok {
		t.Fatal("缺失槽不得产出拨号器")
	}

	dial, ok := reg.SlotDial("a")
	if !ok || dial == nil {
		t.Fatal("启用槽应产出拨号器")
	}

	if !reg.HasSlot("a") || reg.HasSlot("b") {
		t.Fatal("HasSlot 只对可用槽为真")
	}
}

func TestNewRegistryPoolAccessors(t *testing.T) {
	t.Parallel()

	reg, err := NewRegistry(nil, []Slot{{ID: "a", URL: "socks5://1.2.3.4:1080", Enabled: true}}, nil)
	if err != nil {
		t.Fatal(err)
	}

	if reg.SlotPool("missing") != nil {
		t.Fatal("SlotPool 只返回可用槽")
	}

	if reg.SlotPool("a") == nil {
		t.Fatal("可用槽应有池")
	}

	if reg.Global() != nil {
		t.Fatal("未配置全局池时 Global() 应为 nil（直连）")
	}
}

func TestNewRegistryRealmDefaults(t *testing.T) {
	t.Parallel()

	reg, err := NewRegistry(nil, []Slot{
		{ID: "cn1", URL: "http://1.2.3.4:8080", Enabled: true},
		{ID: "off", URL: "http://5.6.7.8:8080", Enabled: false},
	}, map[string]string{
		"cn":     "cn1",
		"global": "off", // 禁用槽：防御性丢弃
		"":       "cn1", // 空 realm 归一为 cn
	})
	if err != nil {
		t.Fatal(err)
	}

	if got := reg.RealmSlot("cn"); got != "cn1" {
		t.Fatalf("RealmSlot(cn)=%q want cn1", got)
	}

	if got := reg.RealmSlot(" CN "); got != "cn1" {
		t.Fatalf("RealmSlot 应 trim+小写: got %q", got)
	}

	if got := reg.RealmSlot(""); got != "cn1" {
		t.Fatalf("空 realm 应归一为 cn: got %q", got)
	}

	if got := reg.RealmSlot("global"); got != "" {
		t.Fatalf("RealmSlot(global)=%q want 空（槽已禁用）", got)
	}
}

func TestNilRegistryIsSafe(t *testing.T) {
	t.Parallel()

	var reg *Registry

	if reg.Global() != nil || reg.SlotPool("a") != nil || reg.Slots() != nil {
		t.Fatal("nil Registry 只读方法应返回零值")
	}

	if _, ok := reg.SlotDial("a"); ok {
		t.Fatal("nil Registry 不得产出拨号器")
	}

	if reg.RealmSlot("cn") != "" || reg.HasSlot("a") {
		t.Fatal("nil Registry 的查表应为零值")
	}
}

func TestNewRegistryInvalidGlobalURL(t *testing.T) {
	t.Parallel()

	if _, err := NewRegistry([]string{"ftp://h:21"}, nil, nil); err == nil {
		t.Fatal("非法全局代理 URL 应报错")
	}
}

func TestNewRegistrySkipsBrokenSlot(t *testing.T) {
	t.Parallel()

	reg, err := NewRegistry(nil, []Slot{{ID: "bad", URL: "ftp://h:21", Enabled: true}}, nil)
	if err != nil {
		t.Fatalf("单个槽构造失败不应影响注册表: %v", err)
	}

	if reg.HasSlot("bad") {
		t.Fatal("构造失败的槽不得可用")
	}

	if views := reg.Slots(); len(views) != 1 || views[0].URL != "" {
		t.Fatalf("views=%+v want 一条无 URL 的降级视图", views)
	}
}
