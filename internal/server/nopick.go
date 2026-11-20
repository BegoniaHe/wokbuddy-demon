// nopick.go 池耗尽（no_healthy_account）的成因拆解与 gateway_hint（issue #607）。
//
// 选号失败时池侧已有完整的排除原因（internal/pool/exclusion.go），这里只负责把它
// 接进失败现场：503 的 gateway_hint 带计数、WARN 日志打一行。所有非对话端点共用本
// 文件的方法，不允许各自拼一份文案。
package server

import (
	"log"

	"github.com/BegoniaHe/wokbuddy-demon/internal/pool"
	"github.com/BegoniaHe/wokbuddy-demon/internal/upstream"
)

// noPickHint 池耗尽时的 gateway_hint：基线下文 + 一次成因拆解，并同步打一行 WARN。
//
// 只在失败路径调用（正常请求不遍历池）：tried 为本轮已尝试的 uid 集合，media 表示
// 是否媒体号池。拆解为空（池里没有可排除的账号）时返回既有常量 hint，不多出噪声。
func (h *Handler) noPickHint(
	stage, reqModel, realm string, tried map[string]bool, media bool,
) string {
	summary := pool.NoPickSummary(h.cfg.Pool.ExclusionSummary(tried, reqModel, realm, media))
	if summary == "" {
		return upstream.NoHealthyAccountHint()
	}

	log.Printf("WARN: [server] %s pool exhausted realm=%q model=%q %s",
		stage, realm, reqModel, summary)

	return upstream.NoHealthyAccountHint() + "; " + summary
}

// pickFailureHint 池耗尽（一次都没打到上游）时的 gateway_hint：带一次选号成因拆解
// （issue #607）。其它错误（上游/传输层）沿用既有常量 hint，不做池遍历。
func (f *chatFlow) pickFailureHint() string {
	if f.lastErr != nil {
		return upstream.NoHealthyAccountHint()
	}

	// 跨域回退开启时域不是约束（pickRealms 会把两个域都试一遍）：按请求域归类会把
	// 另一域账号算成「realm 排除」，运维会照着这条提示去查域配置。
	realm := f.realm
	if f.crossRealm {
		realm = ""
	}

	return f.h.noPickHint("chat", f.bareModel, realm, f.tried, false)
}
