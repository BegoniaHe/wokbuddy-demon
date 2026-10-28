// proxyreg.go 出站出口注册表的装配与账号选路（issue #39）。
//
// 出口优先级（高 → 低）：账号绑定槽 → 账号 realm 默认槽 → upstream.proxies 全局池
// → 直连。前两者命中但槽不可用时 **fail-closed**（请求失败、不回落直连）——静默
// 直连会让运维误以为在走代理，实际暴露宿主机真实 IP，比不走代理更危险。
//
// 三个代理字段（proxies / proxy_slots / realm_proxy_slots）全部热生效：注册表整体
// 替换，拨号器走 upstream.Client.SetProxyRegistry 的原子替换（不就地改 Transport）。
package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"sync"
	"sync/atomic"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
	"github.com/BegoniaHe/wokbuddy-demon/internal/outbound"
	"github.com/BegoniaHe/wokbuddy-demon/internal/panel"
	"github.com/BegoniaHe/wokbuddy-demon/internal/pool"
	"github.com/BegoniaHe/wokbuddy-demon/internal/upstream"
)

// liveProxySlots 面板读取的当前生效槽视图（原子整体替换）。面板 provider 不得捕获
// 装配期 cfg —— 槽热改后那份快照就过期了（issue #39）。
var liveProxySlots atomic.Pointer[[]panel.ProxySlotInfo]

// proxySlotViews 把配置的启用槽映射为面板展示信息（provider 形态，热生效安全）。
func proxySlotViews(cfg *Config) []panel.ProxySlotInfo {
	out := make([]panel.ProxySlotInfo, 0, len(cfg.Upstream.ProxySlots))
	for _, slot := range cfg.Upstream.ProxySlots {
		if slot.Enabled {
			out = append(out, panel.ProxySlotInfo{ID: slot.ID, Name: slot.Name})
		}
	}

	return out
}

// currentProxySlotViews 返回当前生效的槽视图（nil 安全，供面板 provider 用）。
func currentProxySlotViews() []panel.ProxySlotInfo {
	if views := liveProxySlots.Load(); views != nil {
		return *views
	}

	return nil
}

// publishProxySlots 发布新的槽视图：装配期与配置热应用各调用一次。
func publishProxySlots(cfg *Config) {
	views := proxySlotViews(cfg)
	liveProxySlots.Store(&views)
}

// buildProxyRegistry 按配置构造出站注册表（issue #39）：全局池 + 命名槽 + realm
// 默认槽。global.enabled 之类的 realm 开关不参与（realm 默认出口只看账号 realm）。
//
// 错误语义：全局池 URL 非法 → 返错（配置阶段已校验，理论不触发）；单个槽构造失败
// → outbound.NewRegistry 记 WARN 并跳过，绑到该槽的账号随后 fail-closed。
func buildProxyRegistry(cfg *Config) (*outbound.Registry, error) {
	slots := make([]outbound.Slot, 0, len(cfg.Upstream.ProxySlots))
	for _, slot := range cfg.Upstream.ProxySlots {
		slots = append(slots, outbound.Slot{
			ID: slot.ID, Name: slot.Name, URL: slot.URL, Enabled: slot.Enabled,
		})
	}

	reg, err := outbound.NewRegistry(cfg.Upstream.Proxies, slots, cfg.Upstream.RealmProxySlots)
	if err != nil {
		return nil, fmt.Errorf("build proxy registry: %w", err)
	}

	return reg, nil
}

// failClosedDial 槽不可用时的拨号器：直接失败，绝不静默回落直连（issue #39）。
// 包装 outbound.ErrProxyDial ⇒ handler 侧按代理层故障处理（502、不罚账号）。
func failClosedDial(slot string) func(ctx context.Context, network, addr string) (net.Conn, error) {
	return func(context.Context, string, string) (net.Conn, error) {
		return nil, fmt.Errorf("%w: 代理槽 %q 未配置或已禁用（不回落直连）", outbound.ErrProxyDial, slot)
	}
}

// slotDial 出站拨号器签名（与 upstream.Client.AccountRoute 的字段类型同一类型）。
type slotDial = func(ctx context.Context, network, addr string) (net.Conn, error)

// configureAccountRouting 把账号出口路由接到上游 client（issue #96/#39）。
//
// 命中绑定槽或 realm 默认槽时返回**非空 slot + 非 nil dial**：upstream 侧以
// 「slot == "" || dial == nil」判回落，故 fail-closed 必须两者都给，否则会静默直连。
func configureAccountRouting(state3 *pool.Pool, up *upstream.Client, reg *outbound.Registry) {
	var warned sync.Map

	up.AccountRoute = func(account *auth.Auth) (string, slotDial) {
		if account == nil {
			return "", nil
		}

		return accountEgress(state3, reg, &warned, account)
	}
}

// accountEgress 按出口优先级为账号选拨号器：账号绑定槽 > realm 默认槽 > 空
// （= 全局池或直连）。前两档命中但槽不可用时 fail-closed。
func accountEgress(
	state4 *pool.Pool, reg *outbound.Registry, warned *sync.Map, account *auth.Auth,
) (string, slotDial) {
	if slot := state4.ProxySlotOf(account.UID); slot != "" {
		if dial, ok := reg.SlotDial(slot); ok {
			return slot, dial
		}

		// 每槽只 WARN 一次（沿用 issue #96 行为），避免每个请求刷屏。
		if _, logged := warned.LoadOrStore("slot:"+slot, true); !logged {
			log.Printf("WARN: [server] 代理槽 %s 不可用：账号 %s 出站将失败（不回落直连）", slot, account.UID)
		}

		return slot, failClosedDial(slot)
	}

	slot := reg.RealmSlot(account.Realm())
	if slot == "" {
		return "", nil // 全局池或直连
	}

	if dial, ok := reg.SlotDial(slot); ok {
		return slot, dial
	}

	// 配置期已校验；这里只防热改竞态（槽被禁用而默认出口尚未更新）。
	return slot, failClosedDial(slot)
}

// applyProxyRegistry 热应用新的代理配置（issue #39）：重建注册表并整体替换出口。
// 构造失败时保留旧出口（配置非法已在落盘前拒绝，这里只防构造期意外）。
func applyProxyRegistry(cfg *Config, state4 *pool.Pool, client4 *upstream.Client) {
	reg, err := buildProxyRegistry(cfg)
	if err != nil {
		log.Printf("WARN: [server] 代理配置无效，保留旧出口: %v", err)

		return
	}

	client4.SetProxyRegistry(reg)
	configureAccountRouting(state4, client4, reg)
	publishProxySlots(cfg)

	if proxyPool := reg.Global(); proxyPool != nil {
		log.Printf("出站代理池已生效：%d 个代理（失败自动冷却轮换）", proxyPool.Len())
	}
}
