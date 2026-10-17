package main

import (
	"strings"

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
		Pool:         d.pool,
		Upstream:     d.up,
		APIKey:       d.cfg.APIKey,
		Keys:         d.keys,
		Session:      d.session,
		StickyCount:  d.stickyCount,
		RedisMode:    d.redisMode,
		SoftCooldown: d.cfg.SoftRateDur,
		Panel:        d.panel,
		Live:         d.live,
		Usage:        d.usage,
		RequestLog:   d.requestLog,
		PromptMode:   d.cfg.Prompt.Mode,
		PromptText:   d.cfg.PromptText,
		// 来源记录开关经 livecfg 热生效；此处同时填静态字段，供 Live 为 nil 的
		// 裸用/测试路径拿到同一缺省值。
		RecordClientInfo:    d.cfg.Logging.RequestClientInfo,
		DecisionTrace:       d.cfg.Logging.DecisionTrace,
		DecisionTraceSample: d.cfg.Logging.DecisionTraceSample,
		LocalWebTools:       d.cfg.Features.LocalWebTools,
		LocalWebToolRounds:  d.cfg.Features.LocalWebToolRounds,
		// 后台模型重定向（issue #103）：Live 快照优先，这里填静态回落值。
		BackgroundNames:  backgroundNameSet(d.cfg.Models.BackgroundNames),
		BackgroundTarget: d.cfg.Models.BackgroundTarget,
		// handler 侧第三道闸（global realm）：false（显式逃生门）时不列 global: 模型名。
		GlobalEnabled: d.cfg.Global.Enabled,
		// 客户端模型别名（issue #52）：请求解析阶段一次性生效，改后需重启。
		ModelAliases: d.cfg.Models.Aliases,
		// 轮换策略（issue #17）：次数上限 + 总时长预算 + 可选状态码换号闸。
		MaxRotate:      d.cfg.Upstream.RotateMax,
		RotateBudget:   d.cfg.RotateBudgetDur,
		RotateStatuses: rotateStatusSet(d.cfg.Upstream.RotateStatusCodes),
		// 入站 SSE 心跳（issue #54）：0 = 关闭；改后需重启。
		KeepaliveInterval: d.cfg.KeepaliveIntervalDur,
		// CORS 允许源（issue #91）：空 = 关闭。
		CORSOrigins: d.cfg.Server.CORSOrigins,
		Notifier:    d.notifier,
	})
}
