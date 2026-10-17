package main

import (
	"context"
	"log"
	"net"

	"github.com/BegoniaHe/wokbuddy-demon/internal/apikey"
	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
	"github.com/BegoniaHe/wokbuddy-demon/internal/livecfg"
	"github.com/BegoniaHe/wokbuddy-demon/internal/notify"
	"github.com/BegoniaHe/wokbuddy-demon/internal/outbound"
	"github.com/BegoniaHe/wokbuddy-demon/internal/panel"
	"github.com/BegoniaHe/wokbuddy-demon/internal/pool"
	"github.com/BegoniaHe/wokbuddy-demon/internal/reqlog"
	"github.com/BegoniaHe/wokbuddy-demon/internal/server"
	"github.com/BegoniaHe/wokbuddy-demon/internal/session"
	"github.com/BegoniaHe/wokbuddy-demon/internal/upstream"
	"github.com/BegoniaHe/wokbuddy-demon/internal/usage"
)

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
func configureAccountRouting(p *pool.Pool, up *upstream.Client, cfg *Config) {
	slotDials := buildProxySlotDials(cfg)
	up.AccountRoute = func(a *auth.Auth) (string, func(ctx context.Context, network, addr string) (net.Conn, error)) {
		if a == nil {
			return "", nil
		}
		slot := p.ProxySlotOf(a.UID)
		if dial, ok := slotDials[slot]; ok {
			return slot, dial
		}
		return "", nil
	}
}

// handlerDeps 是构建 HTTP handler 所需的装配期依赖。
type handlerDeps struct {
	cfg         *Config
	pool        *pool.Pool
	up          *upstream.Client
	keys        *apikey.Store
	session     *session.Router
	stickyCount func() int
	redisMode   string
	panel       *panel.Panel
	live        *livecfg.Holder
	usage       *usage.Recorder
	requestLog  *reqlog.Recorder
	notifier    notify.Sender
}

// buildHandler 把装配期依赖映射为 server.Config；这些字段改动需重启。
func buildHandler(d handlerDeps) *server.Handler {
	return server.NewHandler(server.Config{
		Pool:                d.pool,
		Upstream:            d.up,
		APIKey:              d.cfg.APIKey,
		Keys:                d.keys,
		Session:             d.session,
		StickyCount:         d.stickyCount,
		RedisMode:           d.redisMode,
		SoftCooldown:        d.cfg.SoftRateDur,
		Panel:               d.panel,
		Live:                d.live,
		Usage:               d.usage,
		RequestLog:          d.requestLog,
		PromptMode:          d.cfg.Prompt.Mode,
		PromptText:          d.cfg.PromptText,
		RecordClientInfo:    d.cfg.Logging.RequestClientInfo,
		DecisionTrace:       d.cfg.Logging.DecisionTrace,
		DecisionTraceSample: d.cfg.Logging.DecisionTraceSample,
		GlobalEnabled:       d.cfg.Global.Enabled,
		ModelAliases:        d.cfg.Models.Aliases,
		MaxRotate:           d.cfg.Upstream.RotateMax,
		RotateBudget:        d.cfg.RotateBudgetDur,
		RotateStatuses:      rotateStatusSet(d.cfg.Upstream.RotateStatusCodes),
		KeepaliveInterval:   d.cfg.KeepaliveIntervalDur,
		Notifier:            d.notifier,
	})
}
