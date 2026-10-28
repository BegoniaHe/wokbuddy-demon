// registry.go 出站出口注册表（issue #39）：全局池 + 命名槽 + realm 默认槽。
//
// 出口优先级（高 → 低）：账号绑定槽 → 账号 realm 默认槽 → 全局池 → 直连。
// 构造后不可变；热更新整体替换新实例（见 upstream.Client.SetProxyRegistry），
// 避免就地改 Transport 与在途请求构成数据竞争。
package outbound

import (
	"context"
	"log"
	"net"
	"strings"
)

// Slot 命名代理槽配置（与 config 的同名字段一一对应）。
type Slot struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	URL     string `json:"url"`
	Enabled bool   `json:"enabled"`
}

// SlotView 面板展示用的槽信息。URL 一律脱敏（Masked），**不含**口令。
type SlotView struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	URL     string `json:"url"`
	Enabled bool   `json:"enabled"`
}

// Registry 出站出口注册表：全局池 + 命名槽 + realm 默认槽。
// 构造后不可变；热更新整体替换新实例（见 upstream.Client.SetProxyRegistry）。
type Registry struct {
	global *Pool            // nil = 直连
	slots  map[string]*Pool // 仅启用且构造成功的槽
	views  []SlotView       // 配置顺序，含禁用槽（面板要能看见它们）
	realm  map[string]string
}

// NewRegistry 按配置构造注册表。
//
// 错误策略：全局池任一 URL 非法 → 返错（配置阶段已校验，理论不触发）；
// 单个槽构造失败 → 记录 WARN 并跳过该槽（与既有行为一致），此时
// SlotDial 返回 ok=false，调用方走 fail-closed（绝不静默回落直连）。
func NewRegistry(globalURLs []string, slots []Slot, realmDefault map[string]string) (*Registry, error) {
	reg := &Registry{slots: map[string]*Pool{}, realm: map[string]string{}}

	if len(globalURLs) > 0 {
		pool, err := New(globalURLs)
		if err != nil {
			return nil, err
		}

		reg.global = pool
	}

	for _, slot := range slots {
		reg.addSlot(slot)
	}

	reg.setRealmDefaults(realmDefault)

	return reg, nil
}

// addSlot 构造单个槽：禁用或构造失败的槽也进视图（面板要能看见并修复），
// 但不产出拨号器 —— 绑到它的账号随后 fail-closed。
func (r *Registry) addSlot(slot Slot) {
	view := SlotView{ID: slot.ID, Name: slot.Name, Enabled: slot.Enabled}

	if !slot.Enabled {
		if proxy, err := ParseProxy(slot.URL); err == nil {
			view.URL = proxy.Masked()
		}

		r.views = append(r.views, view)

		return
	}

	pool, err := New([]string{slot.URL})
	if err != nil {
		log.Printf("WARN: [outbound] 代理槽 %s 无效，忽略: %v", slot.ID, err)

		r.views = append(r.views, view)

		return
	}

	r.slots[slot.ID] = pool
	view.URL = pool.Masked()
	r.views = append(r.views, view)
}

// setRealmDefaults 记录 realm → 默认槽。只有「启用且构造成功」的槽才能作为默认
// 出口；指向禁用/缺失槽的配置在 normalize 阶段已被拒绝，这里只是防御性过滤。
func (r *Registry) setRealmDefaults(realmDefault map[string]string) {
	for realm, id := range realmDefault {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}

		if _, ok := r.slots[id]; !ok {
			log.Printf("WARN: [outbound] realm %s 的默认槽 %q 不可用，按未设置处理", realm, id)

			continue
		}

		r.realm[normRealm(realm)] = id
	}
}

// Global 返回全局代理池；nil = 直连。
func (r *Registry) Global() *Pool {
	if r == nil {
		return nil
	}

	return r.global
}

// SlotDial 返回命名槽的拨号器；ok=false 表示该槽未配置/已禁用/构造失败
// （调用方必须 fail-closed，不得回落直连）。
func (r *Registry) SlotDial(
	id string,
) (func(ctx context.Context, network, addr string) (net.Conn, error), bool) {
	if r == nil {
		return nil, false
	}

	pool, ok := r.slots[id]
	if !ok {
		return nil, false
	}

	return pool.DialContext, true
}

// RealmSlot 返回 realm 的默认槽 id；空串 = 未设置。
func (r *Registry) RealmSlot(realm string) string {
	if r == nil {
		return ""
	}

	return r.realm[normRealm(realm)]
}

// HasSlot 报告是否存在可用（启用且构造成功）的命名槽。
func (r *Registry) HasSlot(id string) bool {
	if r == nil {
		return false
	}

	_, ok := r.slots[id]

	return ok
}

// Slots 返回面板展示用的槽视图（配置顺序，含禁用槽；URL 已脱敏）。
// 返回副本，调用方可以自由持有。
func (r *Registry) Slots() []SlotView {
	if r == nil {
		return nil
	}

	return append([]SlotView(nil), r.views...)
}

// SlotPool 返回命名槽的池（健康快照/出口探测用）；nil = 无此可用槽。
func (r *Registry) SlotPool(id string) *Pool {
	if r == nil {
		return nil
	}

	return r.slots[id]
}

// normRealm realm 归一：trim + 小写，空按 cn（与账号侧 Realm() 同口径）。
func normRealm(realm string) string {
	out := strings.ToLower(strings.TrimSpace(realm))
	if out == "" {
		return "cn"
	}

	return out
}
