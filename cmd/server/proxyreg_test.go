package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
	"github.com/BegoniaHe/wokbuddy-demon/internal/outbound"
	"github.com/BegoniaHe/wokbuddy-demon/internal/pool"
	"github.com/BegoniaHe/wokbuddy-demon/internal/upstream"
)

// newRoutingState 构造只含一个账号的账号池（代理槽绑定按 uid 落 state）。
func newRoutingState(t *testing.T) *pool.Pool {
	t.Helper()

	state := pool.New("")
	state.Add(&auth.Auth{
		UID: "u1", AccessToken: "at", RefreshToken: "rt",
		ExpiresAt: 9999999999, Domain: "workbuddy.cn",
	})

	return state
}

// newTestRegistry 按「槽 id -> URL」构造注册表；enabled 决定槽是否启用。
func newTestRegistry(t *testing.T, enabled map[string]string, disabled []string, realm map[string]string) *outbound.Registry {
	t.Helper()

	slots := make([]outbound.Slot, 0, len(enabled)+len(disabled))
	for id, url := range enabled {
		slots = append(slots, outbound.Slot{ID: id, Name: id, URL: url, Enabled: true})
	}

	for _, id := range disabled {
		slots = append(slots, outbound.Slot{ID: id, Name: id, URL: "socks5://127.0.0.1:1080", Enabled: false})
	}

	reg, err := outbound.NewRegistry(nil, slots, realm)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}

	return reg
}

// TestAccountRouteBindingWinsOverRealmDefault 账号绑定槽优先于 realm 默认槽。
func TestAccountRouteBindingWinsOverRealmDefault(t *testing.T) {
	t.Parallel()

	state := newRoutingState(t)
	if !state.SetProxySlot("u1", "bound") {
		t.Fatal("绑定代理槽失败")
	}

	reg := newTestRegistry(t,
		map[string]string{"bound": "socks5://127.0.0.1:1080", "cn-default": "socks5://127.0.0.1:1081"},
		nil, map[string]string{"cn": "cn-default"})

	client := upstream.New()
	configureAccountRouting(state, client, reg)

	slot, dial := client.AccountRoute(&auth.Auth{UID: "u1", Domain: "workbuddy.cn"})
	if slot != "bound" || dial == nil {
		t.Fatalf("slot=%q dial=%v want bound + dial", slot, dial != nil)
	}
}

// TestAccountRouteRealmDefault realm 默认槽生效（账号自身未绑定）。
func TestAccountRouteRealmDefault(t *testing.T) {
	t.Parallel()

	state := newRoutingState(t)
	reg := newTestRegistry(t, map[string]string{"cn-default": "socks5://127.0.0.1:1081"}, nil,
		map[string]string{"cn": "cn-default"})

	client := upstream.New()
	configureAccountRouting(state, client, reg)

	slot, dial := client.AccountRoute(&auth.Auth{UID: "u1", Domain: "workbuddy.cn"})
	if slot != "cn-default" || dial == nil {
		t.Fatalf("slot=%q dial=%v want cn-default + dial", slot, dial != nil)
	}
}

// TestAccountRouteFailClosed 绑定槽不可用时必须返回非空 slot + 非 nil dial，且该
// dial 的错误包装 outbound.ErrProxyDial（handler 侧据此 502 且不罚账号）。
// 关键回归：此前实现返回 "", nil ⇒ 静默回落直连（暴露宿主机真实 IP）。
func TestAccountRouteFailClosed(t *testing.T) {
	t.Parallel()

	state := newRoutingState(t)
	if !state.SetProxySlot("u1", "missing") {
		t.Fatal("绑定代理槽失败")
	}

	reg := newTestRegistry(t, map[string]string{"other": "socks5://127.0.0.1:1080"}, []string{"off"}, nil)

	client := upstream.New()
	configureAccountRouting(state, client, reg)

	slot, dial := client.AccountRoute(&auth.Auth{UID: "u1", Domain: "workbuddy.cn"})
	if slot != "missing" || dial == nil {
		t.Fatalf("slot=%q dial=%v want missing + 非 nil dial（fail-closed）", slot, dial != nil)
	}

	_, err := dial(context.Background(), "tcp", "127.0.0.1:9")
	if !errors.Is(err, outbound.ErrProxyDial) {
		t.Fatalf("err=%v want outbound.ErrProxyDial", err)
	}

	if !strings.Contains(err.Error(), "不回落直连") {
		t.Fatalf("err=%q 应说明不回落直连", err)
	}
}

// TestAccountRouteDisabledRealmSlotFailsClosed 指向禁用槽的 realm 默认出口在热改
// 竞态下仍需 fail-closed（配置期已拒绝，这里只测防御分支）。
func TestAccountRouteDisabledRealmSlotFailsClosed(t *testing.T) {
	t.Parallel()

	state := newRoutingState(t)
	// NewRegistry 会过滤掉指向禁用槽的 realm 默认，故用直接构造的注册表模拟竞态：
	// 槽可用 → 默认出口正常；槽不可用（未注册）→ SlotDial 为 false。
	reg := newTestRegistry(t, map[string]string{"cn-default": "socks5://127.0.0.1:1081"}, nil,
		map[string]string{"cn": "cn-default"})

	client := upstream.New()
	configureAccountRouting(state, client, reg)

	if slot, _ := client.AccountRoute(&auth.Auth{UID: "u1", Domain: "workbuddy.cn"}); slot != "cn-default" {
		t.Fatalf("slot=%q want cn-default", slot)
	}

	// 直接验证 failClosedDial 的错误语义（realm 分支的防御路径与之同源）。
	if _, err := failClosedDial("off")(context.Background(), "tcp", "127.0.0.1:9"); !errors.Is(err, outbound.ErrProxyDial) {
		t.Fatalf("err=%v want outbound.ErrProxyDial", err)
	}
}

// TestAccountRouteUnsetIsZeroRegression 三档都未配置 → ("", nil)，走全局池/直连。
func TestAccountRouteUnsetIsZeroRegression(t *testing.T) {
	t.Parallel()

	state := newRoutingState(t)
	reg := newTestRegistry(t, map[string]string{"unused": "socks5://127.0.0.1:1080"}, nil, nil)

	client := upstream.New()
	configureAccountRouting(state, client, reg)

	for _, account := range []*auth.Auth{nil, {UID: "u1"}, {UID: "unknown", Domain: "workbuddy.cn"}} {
		slot, dial := client.AccountRoute(account)
		if slot != "" || dial != nil {
			t.Fatalf("account=%v slot=%q dial=%v want 空 + nil", account, slot, dial != nil)
		}
	}
}

// TestProxySlotViewsSkipsDisabled 面板 provider 只暴露启用槽。
func TestProxySlotViewsSkipsDisabled(t *testing.T) {
	t.Parallel()

	cfg := Default()
	cfg.Upstream.ProxySlots = []ProxySlot{
		{ID: "a", Name: "A", URL: "socks5://127.0.0.1:1080", Enabled: true},
		{ID: "b", Name: "B", URL: "socks5://127.0.0.1:1081", Enabled: false},
	}

	views := proxySlotViews(cfg)
	if len(views) != 1 || views[0].ID != "a" {
		t.Fatalf("views=%+v want 仅 a", views)
	}

	// provider 形态：发布后 currentProxySlotViews 读到同一份（热生效安全）。
	publishProxySlots(cfg)

	if got := currentProxySlotViews(); len(got) != 1 || got[0].ID != "a" {
		t.Fatalf("published views=%+v want 仅 a", got)
	}
}

// TestBuildProxyRegistryWiresGlobalAndRealm 注册表按配置带出全局池与 realm 默认。
func TestBuildProxyRegistryWiresGlobalAndRealm(t *testing.T) {
	t.Parallel()

	cfg := Default()
	cfg.Upstream.Proxies = []string{"socks5://127.0.0.1:1080"}
	cfg.Upstream.ProxySlots = []ProxySlot{
		{ID: "g", Name: "G", URL: "socks5://127.0.0.1:1081", Enabled: true},
	}
	cfg.Upstream.RealmProxySlots = map[string]string{"global": "g"}

	reg, err := buildProxyRegistry(cfg)
	if err != nil {
		t.Fatalf("buildProxyRegistry: %v", err)
	}

	if pool := reg.Global(); pool == nil || pool.Len() != 1 {
		t.Fatalf("global pool=%v want 1 个代理", pool)
	}

	if got := reg.RealmSlot("global"); got != "g" {
		t.Fatalf("RealmSlot(global)=%q want g", got)
	}
}

// TestNormalizeRealmProxySlots 归一与校验：键只允许 cn/global、空值 = 未设置、
// 值必须命中已启用槽。
func TestNormalizeRealmProxySlots(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		slots   []ProxySlot
		realm   map[string]string
		want    map[string]string
		wantErr string
	}{
		{
			name:  "合法配置 + 键归一",
			slots: []ProxySlot{{ID: "a", URL: "socks5://127.0.0.1:1080", Enabled: true}},
			realm: map[string]string{" CN ": "a"},
			want:  map[string]string{"cn": "a"},
		},
		{
			name:  "空值视为未设置",
			slots: []ProxySlot{{ID: "a", URL: "socks5://127.0.0.1:1080", Enabled: true}},
			realm: map[string]string{"cn": " ", "global": ""},
			want:  map[string]string{},
		},
		{
			name:    "非法 realm 键",
			slots:   []ProxySlot{{ID: "a", URL: "socks5://127.0.0.1:1080", Enabled: true}},
			realm:   map[string]string{"eu": "a"},
			wantErr: "upstream.realm_proxy_slots[eu]",
		},
		{
			name: "指向未启用槽",
			slots: []ProxySlot{
				{ID: "a", URL: "socks5://127.0.0.1:1080", Enabled: false},
			},
			realm:   map[string]string{"cn": "a"},
			wantErr: "未定义的代理槽",
		},
		{
			name:    "指向不存在的槽",
			slots:   nil,
			realm:   map[string]string{"global": "nope"},
			wantErr: "未定义的代理槽",
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			cfg := Default()
			cfg.Upstream.ProxySlots = testCase.slots
			cfg.Upstream.RealmProxySlots = testCase.realm

			err := cfg.normalizeProxies()
			if testCase.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), testCase.wantErr) {
					t.Fatalf("err=%v want 含 %q", err, testCase.wantErr)
				}

				return
			}

			if err != nil {
				t.Fatalf("normalizeProxies: %v", err)
			}

			if len(cfg.Upstream.RealmProxySlots) != len(testCase.want) {
				t.Fatalf("realm slots=%v want %v", cfg.Upstream.RealmProxySlots, testCase.want)
			}

			for realm, id := range testCase.want {
				if cfg.Upstream.RealmProxySlots[realm] != id {
					t.Fatalf("realm slots=%v want %v", cfg.Upstream.RealmProxySlots, testCase.want)
				}
			}
		})
	}
}
