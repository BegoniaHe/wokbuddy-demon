// Package livecfg 运行期可变配置的并发安全持有者。
//
// 背景：进程启动时读入的配置是普通字段（读多写零），但管理面板允许在线改配置，
// 于是少量"可热生效"的字段需要有并发安全的读写点。此处用不可变快照 + atomic 指针：
// 读方 Load 拿到一致视图，写方 Store 整体替换，无锁无数据竞争。
//
// 只承载**读路径深、热改需求强**的少数字段；池参数/排程参数等各有既有 setter
// （pool.SetBreaker、scheduler.Reconfigure 等），不重复收编到这里。
package livecfg

import (
	"strings"
	"sync/atomic"
	"time"
)

// Snapshot 一次读取的不可变配置视图。
type Snapshot struct {
	APIKey               string        // 网关/面板共同鉴权密钥；空 = 不鉴权
	SoftCooldown         time.Duration // 429 软冷却基数（<=0 时调用方回退内置默认）
	SanitizeFingerprints bool          // 出站请求体指纹脱敏
	RecordClientInfo     bool          // 请求日志是否记录调用来源（客户端 IP / UA）
	// MultimodalRouting 请求模态不匹配时在目录内改选支持该模态的模型
	// （features.multimodal_routing，默认 false = 明确 400，issue #109）。
	MultimodalRouting bool
	// LocalWebTools 是否允许 Responses 本地代跑 web_search/web_fetch（issue #104）。
	LocalWebTools bool
	// SearchEndpoint 是否开放 POST /v1/search/messages（features.search_endpoint，
	// 默认 false）：握手官方联网搜索，会消耗账号额度，故与其它新增出站能力同口径默认关。
	SearchEndpoint bool
	// SearchDeep 是否允许 /v1/search/messages 的 deep=true（issue #277）。
	SearchDeep bool
	// WebFetchEndpoint 是否开放 POST /v1/tools/fetch（issue #276）。
	WebFetchEndpoint bool
	// ImagesEndpoint 是否开放 POST /v1/images/generations（features.images_endpoint，
	// 默认 false）。未在真实账号验证前不默认开启（issue #253）。
	ImagesEndpoint bool
	// LocalWebToolRounds 本地工具最多代跑轮数（默认 3，封顶 8）。
	LocalWebToolRounds int
	// BackgroundNames 后台模型改写名单（小写；issue #103）。空 = 关闭。
	BackgroundNames map[string]bool
	// BackgroundTarget 后台模型改写目标（issue #103）。
	BackgroundTarget string
	// HiddenModels 模型黑名单（models.hidden），归一为 "cn:x"/"global:x" 全量形式：
	// 命中即不在 /v1/models 列出、点名调用返回 400 model_unavailable。空 = 关闭。
	HiddenModels map[string]struct{}
}

// NormalizeHiddenModels 把 models.hidden 归一为 "realm:model" 键集合：
//   - 裸名 = 两域都隐藏（展开成 cn:x + global:x）；
//   - "cn:x" / "global:x" 只藏该域；
//   - 大小写敏感（与 models.aliases / splitRealm 同口径），空白项丢弃。
func NormalizeHiddenModels(list []string) map[string]struct{} {
	if len(list) == 0 {
		return nil
	}

	out := make(map[string]struct{}, len(list)*2)

	for _, raw := range list {
		name := strings.TrimSpace(raw)
		if name == "" {
			continue
		}

		realm, bare, prefixed := splitHiddenRealm(name)
		if bare == "" {
			continue
		}

		if prefixed {
			out[realm+":"+bare] = struct{}{}
			continue
		}

		out["cn:"+bare] = struct{}{}
		out["global:"+bare] = struct{}{}
	}

	if len(out) == 0 {
		return nil
	}

	return out
}

// splitHiddenRealm 拆分可选的 realm 前缀（只认 cn/global，其余原样视为裸模型名）。
func splitHiddenRealm(name string) (realm, bare string, prefixed bool) {
	for _, candidate := range []string{"cn", "global"} {
		prefix := candidate + ":"
		if trimmed, ok := strings.CutPrefix(name, prefix); ok {
			return candidate, strings.TrimSpace(trimmed), true
		}
	}

	return "", name, false
}

// Holder 原子持有当前快照。
type Holder struct {
	p atomic.Pointer[Snapshot]
}

// New 以初始快照构建。
func New(s Snapshot) *Holder {
	h := &Holder{}
	h.Store(s)

	return h
}

// Load 返回当前快照（Holder 为 nil 或从未 Store 时返回零值快照，调用方无需判空）。
func (h *Holder) Load() Snapshot {
	if h == nil {
		return Snapshot{}
	}

	if s := h.p.Load(); s != nil {
		return *s
	}

	return Snapshot{}
}

// Store 整体替换快照。
func (h *Holder) Store(s Snapshot) { h.p.Store(&s) }
