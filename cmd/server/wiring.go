package main

import (
	"github.com/BegoniaHe/wokbuddy-demon/internal/apikey"
	"github.com/BegoniaHe/wokbuddy-demon/internal/livecfg"
	"github.com/BegoniaHe/wokbuddy-demon/internal/notify"
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
//
// 粘性分配的模型名可能带 realm 前缀（"global:gpt-5.4" / "cn:glm-5.2"）：必须按前缀剥出
// realm + bareModel，再交给分池选号域过滤——否则裸名取池子全集，global 号会被粘性分配给
// CN 前缀请求（跨 realm 泄漏）。裸名/显式 cn → cn 集合；global: → global 集合。
//
// realm 为空串时 pool.WeightedAvailableUIDsForModelRealm 退化为现状
// （AvailableUIDsForModel），老调用（无前缀模型名）语义零改动。
//
// 返回列表可能对快过期账号重复同一 UID，作为虚拟实例权重；会话哈希分配无需感知
// 权重细节，已有绑定的快路径仍直接返回原账号，不做迁移。
func realmAwareAvailableForModel(p *pool.Pool) func(model string) []string {
	return func(model string) []string {
		realm, bare := server.ResolveModel(model)
		return p.WeightedAvailableUIDsForModelRealm(bare, realm)
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
		Pool: d.pool, Upstream: d.up, APIKey: d.cfg.APIKey, Keys: d.keys,
		Session: d.session, StickyCount: d.stickyCount, RedisMode: d.redisMode,
		SoftCooldown: d.cfg.SoftRateDur, Panel: d.panel, Live: d.live,
		Usage: d.usage, RequestLog: d.requestLog,
		PromptMode: d.cfg.Prompt.Mode, PromptText: d.cfg.PromptText,
		RecordClientInfo: d.cfg.Logging.RequestClientInfo,
		DecisionTrace:    d.cfg.Logging.DecisionTrace, DecisionTraceSample: d.cfg.Logging.DecisionTraceSample,
		GlobalEnabled: d.cfg.Global.Enabled, ModelAliases: d.cfg.Models.Aliases,
		MaxRotate: d.cfg.Upstream.RotateMax, RotateBudget: d.cfg.RotateBudgetDur,
		RotateStatuses:    rotateStatusSet(d.cfg.Upstream.RotateStatusCodes),
		KeepaliveInterval: d.cfg.KeepaliveIntervalDur, Notifier: d.notifier,
	})
}
