package main

import (
	"context"
	"log"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/BegoniaHe/wokbuddy-demon/internal/apikey"
	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
	"github.com/BegoniaHe/wokbuddy-demon/internal/livecfg"
	"github.com/BegoniaHe/wokbuddy-demon/internal/notify"
	"github.com/BegoniaHe/wokbuddy-demon/internal/outbound"
	"github.com/BegoniaHe/wokbuddy-demon/internal/panel"
	"github.com/BegoniaHe/wokbuddy-demon/internal/pool"
	"github.com/BegoniaHe/wokbuddy-demon/internal/pricing"
	"github.com/BegoniaHe/wokbuddy-demon/internal/quota"
	"github.com/BegoniaHe/wokbuddy-demon/internal/reqlog"
	"github.com/BegoniaHe/wokbuddy-demon/internal/server"
	"github.com/BegoniaHe/wokbuddy-demon/internal/session"
	"github.com/BegoniaHe/wokbuddy-demon/internal/upstream"
	"github.com/BegoniaHe/wokbuddy-demon/internal/usage"
)

// backgroundNameSet 把后台模型名单转为小写查表集合（issue #103）。
func backgroundNameSet(names []string) map[string]bool {
	if len(names) == 0 {
		return nil
	}

	set := make(map[string]bool, len(names))
	for _, name := range names {
		set[strings.ToLower(strings.TrimSpace(name))] = true
	}

	return set
}

// rotateStatusSet 把配置的状态码列表转为查表集合（issue #17）。空列表返回 nil，
// 表示「不按状态码过滤，维持现有按错误分类的换号语义」。
func rotateStatusSet(codes []int) map[int]bool {
	if len(codes) == 0 {
		return nil
	}

	set := make(map[int]bool, len(codes))
	for _, c := range codes {
		set[c] = true
	}

	return set
}

// realmAwareAvailableForModel 构造会话粘性路由按模型可用口径的 realm 感知闭包。
func realmAwareAvailableForModel(p *pool.Pool) func(model string) []string {
	return func(model string) []string {
		realm, bare := server.ResolveModel(model)
		return p.WeightedAvailableUIDsForModelRealm(bare, realm)
	}
}

// realmAwareStickyLowLoad 构造粘性降级窗口结束后的低水位判定闭包。
func realmAwareStickyLowLoad(p *pool.Pool) func(model, uid string) bool {
	return func(model, uid string) bool {
		realm, bare := server.ResolveModel(model)
		return p.StickyLowLoad(uid, bare, realm)
	}
}

// applyDailyLimits 把配置的每日护栏映射到 pool（issue #95）。
func applyDailyLimits(state *pool.Pool, cfg *Config) {
	toPool := func(r LimitRule) pool.DailyLimits {
		return pool.DailyLimits{Tokens: r.DailyTokens, Credits: r.DailyCredits, ModelTokens: r.ModelDailyTokens}
	}
	def := pool.DailyLimits{
		Tokens: cfg.Limits.DailyTokens, Credits: cfg.Limits.DailyCredits, ModelTokens: cfg.Limits.ModelDailyTokens,
	}
	state.SetDailyLimits(def, toPool(cfg.Limits.CN), toPool(cfg.Limits.Global))
}

// setDailyUsageProvider 把 usage 当日聚合接到 pool（issue #95）。
func setDailyUsageProvider(p *pool.Pool, rec *usage.Recorder) {
	p.SetDailyUsageProvider(func() map[string]pool.DailyUsage {
		src := rec.DailyTotalsCached(time.Now())

		out := make(map[string]pool.DailyUsage, len(src))
		for uid, u := range src {
			out[uid] = pool.DailyUsage{Tokens: u.Tokens, Credits: u.Credits, Models: u.Models}
		}

		return out
	})
}

// buildProxySlotDials 构造账号代理槽的拨号器（issue #96）。
func buildProxySlotDials(cfg *Config) map[string]func(ctx context.Context, network, addr string) (net.Conn, error) {
	out := make(map[string]func(ctx context.Context, network, addr string) (net.Conn, error), len(cfg.Upstream.ProxySlots))
	for _, slot := range cfg.Upstream.ProxySlots {
		if !slot.Enabled {
			continue
		}

		slotPool, err := outbound.New([]string{slot.URL})
		if err != nil {
			log.Printf("WARN: [server] 代理槽 %s 无效，忽略: %v", slot.ID, err)
			continue
		}

		out[slot.ID] = slotPool.DialContext
	}

	return out
}

// proxySlotInfos 把配置的代理槽映射为面板展示信息（issue #96）。
func proxySlotInfos(cfg *Config) []panel.ProxySlotInfo {
	out := make([]panel.ProxySlotInfo, 0, len(cfg.Upstream.ProxySlots))
	for _, slot := range cfg.Upstream.ProxySlots {
		if slot.Enabled {
			out = append(out, panel.ProxySlotInfo{ID: slot.ID, Name: slot.Name})
		}
	}

	return out
}

// configureAccountRouting 把账号代理槽接到上游 client（issue #96）。
func configureAccountRouting(state2 *pool.Pool, up *upstream.Client, cfg *Config) {
	slotDials := buildProxySlotDials(cfg)

	var missingSlots sync.Map

	up.AccountRoute = func(a *auth.Auth) (string, func(ctx context.Context, network, addr string) (net.Conn, error)) {
		if a == nil {
			return "", nil
		}

		slot := state2.ProxySlotOf(a.UID)
		if slot == "" {
			return "", nil
		}

		if dial, ok := slotDials[slot]; ok {
			return slot, dial
		}

		if _, warned := missingSlots.LoadOrStore(slot, true); !warned {
			log.Printf("WARN: [server] 代理槽 %s 不可用（未配置或已禁用），已绑定账号回落默认出口", slot)
		}

		return "", nil
	}
}

// newPricingCatalog 按配置创建价格缓存（issue #99）；关闭时返回 nil。
func newPricingCatalog(cfg *Config) *pricing.Catalog {
	if !cfg.Pricing.Enabled {
		return nil
	}

	return pricing.New("", nil)
}

// startPricingRefresh 启动 OpenRouter 价格拉取（issue #99）：启动即拉一次，
// refresh_minutes > 0 时按周期刷新；失败保留旧快照并记日志。
func startPricingRefresh(ctx context.Context, cfg *Config, cat *pricing.Catalog) {
	if cat == nil {
		return
	}

	refresh := func() {
		if err := cat.Refresh(ctx); err != nil {
			log.Printf("WARN: [pricing] 拉取 OpenRouter 价格失败（保留上次快照）: %v", err)
		}
	}
	go refresh()

	if cfg.Pricing.RefreshMinutes <= 0 {
		return
	}
	go func() {
		t := time.NewTicker(time.Duration(cfg.Pricing.RefreshMinutes) * time.Minute)
		defer t.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				refresh()
			}
		}
	}()
}

// quotaQueueSize 撞线事件队列容量。事件只用于面板对账，满时丢弃不阻塞选号；
// 固定 worker 避免每次事件起一个 goroutine 的无界堆积。
const quotaQueueSize = 256

type quotaEvent struct {
	uid     string
	model   string
	realm   string
	resetAt time.Time
}

// setupQuota 创建撞线事件存储并接到 pool（issue #101）。
func setupQuota(p *pool.Pool, rec *usage.Recorder, cfg *Config) *quota.Store {
	store := quota.New(stateSibling(cfg.StateFile, "quota_events.jsonl"))

	queue := make(chan quotaEvent, quotaQueueSize)
	go func() {
		for ev := range queue {
			used := rec.TokensSince(ev.realm, ev.uid, ev.model, ev.resetAt.Add(-24*time.Hour))
			store.Add(&quota.Event{
				UID: ev.uid, Realm: ev.realm, Model: ev.model,
				ResetAt: ev.resetAt, Used: used, At: time.Time{},
			})
		}
	}()

	p.SetModelRateLimitHook(func(uid, model, realm string, resetAt time.Time) {
		// 回调在 pool 写锁内触发：只做非阻塞入队，扫描 usage 与写 JSONL 由 worker 串行处理。
		select {
		case queue <- quotaEvent{uid: uid, model: model, realm: realm, resetAt: resetAt}:
		default:
			log.Printf("WARN: [quota] 撞线事件队列已满，丢弃 uid=%s model=%s", uid, model)
		}
	})

	return store
}

// handlerDeps 是构建 HTTP handler 所需的装配期依赖。
type handlerDeps struct {
	cfg            *Config
	pool           *pool.Pool
	up             *upstream.Client
	keys           *apikey.Store
	session        *session.Router
	stickyCount    func() int
	redisMode      string
	panel          *panel.Panel
	live           *livecfg.Holder
	usage          *usage.Recorder
	requestLog     *reqlog.Recorder
	notifier       notify.Sender
	trustedProxies []*net.IPNet
}

// buildHandler 把装配期依赖映射为 server.Config；这些字段改动需重启。
func buildHandler(handlerDeps2 *handlerDeps) *server.Handler {
	return server.NewHandler(&server.Config{
		Pool:         handlerDeps2.pool,
		Upstream:     handlerDeps2.up,
		APIKey:       handlerDeps2.cfg.APIKey,
		Keys:         handlerDeps2.keys,
		Session:      handlerDeps2.session,
		StickyCount:  handlerDeps2.stickyCount,
		RedisMode:    handlerDeps2.redisMode,
		SoftCooldown: handlerDeps2.cfg.SoftRateDur,
		Panel:        handlerDeps2.panel,
		Live:         handlerDeps2.live,
		Usage:        handlerDeps2.usage,
		RequestLog:   handlerDeps2.requestLog,
		PromptMode:   handlerDeps2.cfg.Prompt.Mode,
		PromptText:   handlerDeps2.cfg.PromptText,
		// 来源记录开关经 livecfg 热生效；此处同时填静态字段，供 Live 为 nil 的
		// 裸用/测试路径拿到同一缺省值。
		RecordClientInfo:    handlerDeps2.cfg.Logging.RequestClientInfo,
		DecisionTrace:       handlerDeps2.cfg.Logging.DecisionTrace,
		DecisionTraceSample: handlerDeps2.cfg.Logging.DecisionTraceSample,
		LocalWebTools:       handlerDeps2.cfg.Features.LocalWebTools,
		LocalWebToolRounds:  handlerDeps2.cfg.Features.LocalWebToolRounds,
		// 后台模型重定向（issue #103）：Live 快照优先，这里填静态回落值。
		BackgroundNames:  backgroundNameSet(handlerDeps2.cfg.Models.BackgroundNames),
		BackgroundTarget: handlerDeps2.cfg.Models.BackgroundTarget,
		// handler 侧第三道闸（global realm）：false（显式逃生门）时不列 global: 模型名。
		GlobalEnabled: handlerDeps2.cfg.Global.Enabled,
		// 客户端模型别名（issue #52）：请求解析阶段一次性生效，改后需重启。
		ModelAliases: handlerDeps2.cfg.Models.Aliases,
		// 轮换策略（issue #17）：次数上限 + 总时长预算 + 可选状态码换号闸。
		MaxRotate:      handlerDeps2.cfg.Upstream.RotateMax,
		RotateBudget:   handlerDeps2.cfg.RotateBudgetDur,
		RotateStatuses: rotateStatusSet(handlerDeps2.cfg.Upstream.RotateStatusCodes),
		// 入站 SSE 心跳（issue #54）：0 = 关闭；改后需重启。
		KeepaliveInterval: handlerDeps2.cfg.KeepaliveIntervalDur,
		// CORS 允许源（issue #91）：空 = 关闭。
		CORSOrigins: handlerDeps2.cfg.Server.CORSOrigins,
		// 可信反代网段：仅当 TCP 对端命中时才采信转发头。
		TrustedProxies: handlerDeps2.trustedProxies,
		Notifier:       handlerDeps2.notifier,
	})
}
