package outbound

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// TestLoginSlotNoSlots 一条启用槽都没有 → 直连（未配代理的部署不能因此加不了号）。
func TestLoginSlotNoSlots(t *testing.T) {
	t.Parallel()

	reg, err := NewRegistry(nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	slot, dial, lerr := reg.LoginSlot("cn")
	if lerr != nil || slot != "" || dial != nil {
		t.Fatalf("slot=%q dial=%v err=%v want 直连", slot, dial != nil, lerr)
	}
}

// TestLoginSlotDisabledOnlyIsDirect 只有禁用槽 = 一条启用槽都没有 → 直连。
func TestLoginSlotDisabledOnlyIsDirect(t *testing.T) {
	t.Parallel()

	reg, err := NewRegistry(nil, []Slot{
		{ID: "off", URL: "socks5://127.0.0.1:1", Enabled: false},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}

	slot, dial, lerr := reg.LoginSlot("cn")
	if lerr != nil || slot != "" || dial != nil {
		t.Fatalf("slot=%q dial=%v err=%v want 直连", slot, dial != nil, lerr)
	}
}

// TestLoginSlotPrefersRealmDefault realm 默认槽优先于配置顺序的其它启用槽。
func TestLoginSlotPrefersRealmDefault(t *testing.T) {
	t.Parallel()

	reg, err := NewRegistry(nil, []Slot{
		{ID: "first", URL: "socks5://127.0.0.1:1", Enabled: true},
		{ID: "realm", URL: "socks5://127.0.0.1:2", Enabled: true},
	}, map[string]string{"cn": "realm"})
	if err != nil {
		t.Fatal(err)
	}

	slot, dial, lerr := reg.LoginSlot("cn")
	if lerr != nil || slot != "realm" || dial == nil {
		t.Fatalf("slot=%q dial=%v err=%v want realm", slot, dial != nil, lerr)
	}
}

// TestLoginSlotSkipsCoolingSlot 冷却中的槽不参与挑选，取配置顺序里的下一个健康槽。
func TestLoginSlotSkipsCoolingSlot(t *testing.T) {
	t.Parallel()

	reg, err := NewRegistry(nil, []Slot{
		{ID: "dead", URL: "http://" + deadAddr(t), Enabled: true},
		{ID: "alive", URL: "socks5://127.0.0.1:1", Enabled: true},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}

	// 拨一次死槽把它推进冷却（健康快照 CooldownSec > 0）。
	if _, derr := reg.SlotPool("dead").DialContext(context.Background(), "tcp", "127.0.0.1:9"); derr == nil {
		t.Fatal("dial 死槽应失败")
	}

	slot, dial, lerr := reg.LoginSlot("cn")
	if lerr != nil || slot != "alive" || dial == nil {
		t.Fatalf("slot=%q dial=%v err=%v want alive", slot, dial != nil, lerr)
	}
}

// TestLoginSlotAllUnhealthyFails 有启用槽但全部冷却 → fail-closed，绝不回落直连。
func TestLoginSlotAllUnhealthyFails(t *testing.T) {
	t.Parallel()

	reg, err := NewRegistry(nil, []Slot{
		{ID: "dead", URL: "http://" + deadAddr(t), Enabled: true},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}

	if _, derr := reg.SlotPool("dead").DialContext(context.Background(), "tcp", "127.0.0.1:9"); derr == nil {
		t.Fatal("dial 死槽应失败")
	}

	slot, dial, lerr := reg.LoginSlot("cn")
	if !errors.Is(lerr, ErrNoHealthySlot) || slot != "" || dial != nil {
		t.Fatalf("slot=%q dial=%v err=%v want ErrNoHealthySlot 且不回落直连", slot, dial != nil, lerr)
	}
}

// TestLoginSlotUnbuildableSlotFails 启用槽构造失败（URL 非法）= 池非空但不健康，
// 同样 fail-closed，不能因为「没有可用拨号器」就直连。
func TestLoginSlotUnbuildableSlotFails(t *testing.T) {
	t.Parallel()

	reg, err := NewRegistry(nil, []Slot{
		{ID: "broken", URL: "ftp://127.0.0.1:1", Enabled: true},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}

	slot, dial, lerr := reg.LoginSlot("cn")
	if !errors.Is(lerr, ErrNoHealthySlot) || slot != "" || dial != nil {
		t.Fatalf("slot=%q dial=%v err=%v want ErrNoHealthySlot", slot, dial != nil, lerr)
	}
}

// TestLoginSlotErrorRedactsPassword 错误文案只含槽 id / realm，回显代理口令即泄密。
func TestLoginSlotErrorRedactsPassword(t *testing.T) {
	t.Parallel()

	reg, err := NewRegistry(nil, []Slot{
		{ID: "prod", URL: "ftp://user:s3cr3t-pass@127.0.0.1:1", Enabled: true},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}

	_, _, lerr := reg.LoginSlot("cn")
	if lerr == nil {
		t.Fatal("应报 ErrNoHealthySlot")
	}

	for _, secret := range []string{"s3cr3t-pass", "user:"} {
		if strings.Contains(lerr.Error(), secret) {
			t.Fatalf("错误文案回显了代理口令: %v", lerr)
		}
	}
}

// TestLoginSlotKeepsRealmIsolation 已分配给其它 realm 的槽不参与本 realm 的登录出口：
// 登录槽会落成账号 proxy_slot 永久绑定，跨 realm 兜底等于把 global 账号钉在只给
// CN 用的线路上。只剩跨 realm 候选时 fail-closed。
func TestLoginSlotKeepsRealmIsolation(t *testing.T) {
	t.Parallel()

	registry, err := NewRegistry(nil, []Slot{
		{ID: "cn-line", URL: "http://127.0.0.1:1", Enabled: true},
		{ID: "generic", URL: "http://127.0.0.1:2", Enabled: true},
	}, map[string]string{"cn": "cn-line"})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}

	id, dial, err := registry.LoginSlot("global")
	if err != nil || id != "generic" || dial == nil {
		t.Fatalf("global 只能用未被占用的通用槽: id=%q err=%v", id, err)
	}

	cnOnly, err := NewRegistry(nil, []Slot{
		{ID: "cn-line", URL: "http://127.0.0.1:1", Enabled: true},
	}, map[string]string{"cn": "cn-line"})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}

	if _, _, err := cnOnly.LoginSlot("global"); !errors.Is(err, ErrNoHealthySlot) {
		t.Fatalf("只剩跨 realm 候选必须 fail-closed: %v", err)
	}
}
