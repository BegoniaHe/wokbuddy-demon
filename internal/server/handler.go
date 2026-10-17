// Package server 暴露 OpenAI 兼容 HTTP 接口，内部驱动 pool 挑号 + upstream 转发。
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"math/rand/v2"
	"net"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/BegoniaHe/wokbuddy-demon/internal/apikey"
	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
	"github.com/BegoniaHe/wokbuddy-demon/internal/httpauth"
	"github.com/BegoniaHe/wokbuddy-demon/internal/livecfg"
	"github.com/BegoniaHe/wokbuddy-demon/internal/logfmt"
	"github.com/BegoniaHe/wokbuddy-demon/internal/notify"
	"github.com/BegoniaHe/wokbuddy-demon/internal/outbound"
	"github.com/BegoniaHe/wokbuddy-demon/internal/pool"
	"github.com/BegoniaHe/wokbuddy-demon/internal/prompt"
	"github.com/BegoniaHe/wokbuddy-demon/internal/reqlog"
	"github.com/BegoniaHe/wokbuddy-demon/internal/session"
	"github.com/BegoniaHe/wokbuddy-demon/internal/upstream"
	"github.com/BegoniaHe/wokbuddy-demon/internal/usage"
)

// Config handler 依赖。
type Config struct {
	Pool      *pool.Pool
	Upstream  *upstream.Client
	APIKey    string // 空 = 不鉴权（静态值；与 Live 同时给出时 Live 优先）
	MaxRotate int    // 单请求最多换号次数，默认 3
	// RotateBudget 轮换总时长预算：从首个尝试起累计耗时达此值即停止换号（0 = 不限）。
	// 与 MaxRotate 构成「先到先停」——避免慢上游下 MaxRotate × header_timeout 把
	// 客户端拖很久（issue #17）。
	RotateBudget time.Duration
	// RotateStatuses 非空时，HTTP 状态码错误仅当 status 在集合内才换号（空 = 维持
	// 现有按错误分类的换号语义）。只收窄换号，不改账号冷却/熔断惩罚（issue #17）。
	RotateStatuses map[int]bool
	// Keys 命名密钥存储（可选；nil = 仅默认 api_key）。命名密钥只用于网关 API
	// 面（/v1/chat/completions、/v1/models）；/status 仍只认默认密钥。
	Keys *apikey.Store
	// Session 会话粘性路由器（可选；nil = 关闭粘性，纯 Pick 轮换）。
	Session *session.Router
	// StickyCount 返回当前粘性会话绑定数（供 /status）；nil 时报告 0。
	StickyCount func() int
	// RedisMode 观测字段（"upstash" / "noop"），供 /status 透出。
	RedisMode    string
	SoftCooldown time.Duration // 429/限流文案软冷却基数，默认 600s（连续触发指数退避，封顶 soft_rate_max）
	RefreshSkew  time.Duration // token 提前刷新窗口，默认 10m

	// Panel 管理面板 handler（可选；nil = 不挂载）。挂载在 /panel/ 前缀下，
	// 面板自带 Bearer 鉴权（同一 api_key）与内嵌静态资源，主路由只做转发。
	Panel http.Handler

	// Live 运行期可变配置（面板在线改 api_key / soft_rate / 脱敏开关时立即生效）。
	// nil 时回退静态字段（测试与裸用场景）。
	Live *livecfg.Holder

	// PromptMode "custom"（网关用自有提示词替换 system）/ "passthrough"（透传）。
	PromptMode string
	// PromptText custom 模式下注入的系统提示词文本（来自 config.PromptText）。
	PromptText string

	// ModelAliases 客户端模型别名 → 上游 "[realm:]model"（config models.aliases）。
	// 命中后选号/粘性/成本台账/日志统一使用解析后的裸模型名（issue #52）。
	ModelAliases map[string]string

	// MaxBodyBytes 聊天类入站请求体上限（server.max_body_bytes）；
	// 0 = 不限制。超限在解析前返回 413，不进入选号。
	MaxBodyBytes int64

	// KeepaliveInterval 入站 SSE 主动心跳间隔（config upstream.keepalive_interval，
	// issue #54）；0 = 关闭（保持逐字节行为）。
	KeepaliveInterval time.Duration

	// Notifier webhook 告警出口（可选；nil = 关闭）。发送失败不影响主路径。
	Notifier notify.Sender

	// MultimodalRouting 静态回落值（Live 为 nil 时使用）：请求模态与模型能力
	// 不匹配时在同 realm 目录里改选支持该模态的模型（features.multimodal_routing）。
	MultimodalRouting bool

	// GlobalEnabled global realm 路由开关（config global.enabled，缺省 true）。
	// handler 侧第三道闸（与 main 注入 auth 开关、upstream.GlobalEnabled 呼应）：
	// false（显式逃生门）时即便 auth realm=global 也不提供 global: 模型名
	// （modelList 不列 global 名单）。
	GlobalEnabled bool

	// Usage 逐请求用量记录器（可选；nil = 不记录）。
	// 在 recordAttempt 这一唯一汇聚点调用，因此流式/非流式、成功/失败都会计入，
	// 且与 pool 的每账号累计器同源，两条口径不会漂移。
	Usage *usage.Recorder

	// RequestLog 请求指标与脱敏 JSONL 归档（可选；nil = 不记录）。
	RequestLog *reqlog.Recorder

	// DecisionTrace / DecisionTraceSample 选号决策链记录开关与采样率（issue #63）。
	DecisionTrace       bool
	DecisionTraceSample float64

	// CORSOrigins 允许跨域访问 `/v1/*` 的 Origin（已规范化）；空 = 关闭。
	CORSOrigins []string
	// LocalWebTools 是否允许 Responses 本地 web 工具代跑（issue #104）。
	LocalWebTools bool
	// LocalWebToolRounds 本地工具代跑轮数（0 回落默认 3，封顶 8）。
	LocalWebToolRounds int
	// BackgroundNames 后台模型改写名单（小写；issue #103）；空 = 关闭。
	BackgroundNames map[string]bool
	// BackgroundTarget 后台模型改写目标（issue #103）。
	BackgroundTarget string

	// RecordClientInfo 是否在请求日志里记录调用来源（客户端 IP / User-Agent）。
	// 来自 logging.request_client_info（缺省 true）；关闭时 reqlog 事件的来源字段
	// 保持为空，归档与面板都不出现来源信息。
	RecordClientInfo bool
}

// Handler 主路由。
type Handler struct {
	cfg     Config
	mux     *http.ServeMux
	degrade degradeGate
	// metrics 进程内生命周期指标（/metrics 的耗时 histogram 与换号计数）。
	metrics *metricsRegistry
	// cors 允许跨域的 Origin 集合；空 = 关闭 CORS。
	cors map[string]bool
	// wafIP WAF IP 级拦截状态机（fail-fast，wafip.go）：短窗多号 WAF 403 →
	// 激活期轮转遇 WAF 403 直接终止（不放大请求量）。进程内状态、重启清零。
	wafIP wafIPGate
	// streams 在途 SSE 流注册表（issue #19）：关闭时显式等待排空并记录未排空数。
	streams *streamTracker
	// authLimiter 运维面鉴权失败按 IP 限速（issue #61）。
	authLimiter *httpauth.Limiter
}

// loadLive 返回当前运行期快照；Live 为 nil 时用静态字段合成。
func (h *Handler) loadLive() livecfg.Snapshot {
	if h.cfg.Live != nil {
		return h.cfg.Live.Load()
	}
	return livecfg.Snapshot{
		APIKey:             h.cfg.APIKey,
		SoftCooldown:       h.cfg.SoftCooldown,
		RecordClientInfo:   h.cfg.RecordClientInfo,
		MultimodalRouting:  h.cfg.MultimodalRouting,
		LocalWebTools:      h.cfg.LocalWebTools,
		LocalWebToolRounds: h.cfg.LocalWebToolRounds,
		BackgroundNames:    h.cfg.BackgroundNames,
		BackgroundTarget:   h.cfg.BackgroundTarget,
	}
}

// softCooldown 返回当前生效的软冷却基数（热改优先，<=0 回退默认）。
func (h *Handler) softCooldown() time.Duration {
	if d := h.loadLive().SoftCooldown; d > 0 {
		return d
	}
	if h.cfg.SoftCooldown > 0 {
		return h.cfg.SoftCooldown
	}
	return 600 * time.Second
}

// notFoundCooldown 上游 404 的固定短冷却时长。
// 与 SoftCooldown 分流的原因：404 是上游**偶发**路径缺失，不是"本账号在限流"，
// 若共用 soft_rate（600s 起 + 指数升级），一次偶发 404 会把好账号罚 10 分钟并逐次加倍。
// 故固定 60s 防雪崩即可，不随 soft_rate 配置、也不参与软退避指数。
const notFoundCooldown = 60 * time.Second

// ServiceName 网关身份标识。经 /healthz 响应体 service 字段与 X-Service 头同时透出：
// 宿主（如 workbuddy-switch 托管网关子进程）探测同端口的旧服务/其他服务时，对方即使
// 返回 2xx 也不带本标识，宿主据此可识别"假成功"。
const ServiceName = "wokbuddy-demon"

// NewHandler 构建 handler。
func NewHandler(cfg Config) *Handler {
	if cfg.MaxRotate <= 0 {
		cfg.MaxRotate = 3
	}
	if cfg.SoftCooldown <= 0 {
		cfg.SoftCooldown = 600 * time.Second // 软限流基数（连续触发按指数退避放大）
	}
	if cfg.RefreshSkew <= 0 {
		cfg.RefreshSkew = 10 * time.Minute
	}
	if cfg.PromptMode == "" {
		cfg.PromptMode = "custom" // 缺省 custom：网关自有提示词
	}
	h := &Handler{
		cfg: cfg, mux: http.NewServeMux(), metrics: newMetricsRegistry(), streams: newStreamTracker(),
		authLimiter: httpauth.NewLimiter(httpauth.DefaultFailLimit, httpauth.DefaultFailWindow),
		cors:        make(map[string]bool, len(cfg.CORSOrigins)),
	}
	for _, origin := range cfg.CORSOrigins {
		h.cors[origin] = true
	}
	// 网关 API 面（/v1/*）接受默认密钥或任一命名密钥；/status、/metrics 属运维面，
	// 只认默认密钥（命名密钥分发给客户端，不应获得账号池可见性）。
	h.mux.HandleFunc("POST /v1/chat/completions", h.withClientAuth(h.chatCompletions))
	h.mux.HandleFunc("POST /v1/responses", h.withClientAuth(h.responsesHandler))
	h.mux.HandleFunc("POST /v1/messages", h.withClientAuth(h.anthropicMessagesHandler))
	h.mux.HandleFunc("POST /v1/messages/count_tokens", h.withClientAuth(h.anthropicCountTokensHandler))
	h.mux.HandleFunc("GET /v1/models", h.withClientAuth(h.models))
	h.mux.HandleFunc("GET /status", h.withAuth(h.status))
	h.mux.HandleFunc("GET /healthz", h.healthz)
	// Prometheus 指标端点（issue #13）：鉴权与主服务同口径（withAuth），空 api_key 即放行。
	h.mux.HandleFunc("GET /metrics", h.withAuth(h.metricsHandler))
	if cfg.Panel != nil {
		h.mux.Handle("/panel/", cfg.Panel) // /panel → /panel/ 由 ServeMux 自动重定向
	}
	// 未注册路径兜底（Issue #12）：返回 OpenAI 风格 JSON 错误体，而非 ServeMux 裸 404。
	// 注册在最具体的路由之后；ServeMux 优先精确/更长前缀，故不会遮蔽上面的端点。
	h.mux.HandleFunc("/", h.notFound)
	return h
}

// ActiveStreams 返回当前在途 SSE 流数（供优雅关闭记录未排空情况，issue #19）。
func (h *Handler) ActiveStreams() int { return h.streams.Active() }

// notFound 未注册路径的统一出口：OpenAI 风格 404 JSON（不鉴权——未知路径无需泄露
// 鉴权存在性，也不暴露端点清单）。协议面（OpenAI 兼容 API）只承诺
// POST /v1/chat/completions 与 GET /v1/models；其余一律落到这里。
func (h *Handler) notFound(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusNotFound, map[string]any{
		"error": map[string]any{
			"message": "unknown endpoint: " + r.Method + " " + r.URL.Path,
			"type":    "invalid_request_error",
		},
	})
}

// handleCORS 处理 `/v1/*` 的跨域头与 OPTIONS 预检。返回 true 表示已写出
// 预检响应；普通请求只附加允许头后继续。管理面与未配置 CORS 时完全跳过。
func (h *Handler) handleCORS(w http.ResponseWriter, r *http.Request) bool {
	if len(h.cors) == 0 || !isV1Path(r.URL.Path) {
		return false
	}
	w.Header().Add("Vary", "Origin")
	origin := strings.ToLower(r.Header.Get("Origin"))
	allowed := origin != "" && h.cors[origin]
	if allowed {
		w.Header().Set("Access-Control-Allow-Origin", origin)
	}
	if r.Method != http.MethodOptions {
		return false
	}
	if allowed {
		w.Header().Set("Access-Control-Allow-Methods", "POST, GET, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, X-Requested-With")
		w.Header().Set("Access-Control-Max-Age", "600")
	}
	w.WriteHeader(http.StatusNoContent)
	return true
}

// isV1Path 仅匹配网关 API 面（/v1 及其子路径），管理面不参与 CORS。
func isV1Path(path string) bool {
	return path == "/v1" || strings.HasPrefix(path, "/v1/")
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h.handleCORS(w, r) {
		return
	}
	if h.cfg.RequestLog != nil && r.Method == http.MethodPost &&
		(r.URL.Path == "/v1/chat/completions" || r.URL.Path == "/v1/messages") {
		trace := &requestTrace{id: reqlog.NewRequestID(), start: time.Now(), path: r.URL.Path}
		if h.loadLive().RecordClientInfo {
			trace.captureClientInfo(r)
		}
		r = r.WithContext(context.WithValue(r.Context(), requestTraceKey{}, trace))
		obs := &responseObserver{ResponseWriter: w}
		w.Header().Set("X-Request-ID", trace.id)
		h.cfg.RequestLog.Begin()
		defer func() {
			status := obs.status
			if status == 0 {
				status = http.StatusOK
			}
			h.cfg.RequestLog.Record(trace.event(status))
		}()
		h.mux.ServeHTTP(obs, r)
		return
	}
	h.mux.ServeHTTP(w, r)
}

// withAuth 默认密钥鉴权（/status 等运维面）：只认 config.api_key。
// api_key 非空时按来源 IP 限制连续失败（issue #61）：超限返回 429 + Retry-After；
// 来源 IP 只认 RemoteAddr，不信任可伪造的转发头；成功鉴权清零窗口。
func (h *Handler) withAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := h.loadLive().APIKey
		if key == "" {
			next(w, r)
			return
		}
		ip := httpauth.RemoteIP(r)
		if ok, retry := h.authLimiter.Allow(ip); !ok {
			writeAuthFailLimit(w, retry)
			return
		}
		if !httpauth.VerifyBearer(r, key) {
			h.authLimiter.Fail(ip)
			writeOpenAIError(w, http.StatusUnauthorized, "invalid_api_key", "missing or invalid API key")
			return
		}
		h.authLimiter.Reset(ip)
		next(w, r)
	}
}

// writeClientAuthError 按入站协议写出鉴权失败：Anthropic 端点用 Anthropic 错误体，
// 其余保持 OpenAI 错误体。
func writeClientAuthError(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/v1/messages") {
		writeAnthropicError(w, http.StatusUnauthorized, "authentication_error", "missing or invalid API key", "")
		return
	}
	writeOpenAIError(w, http.StatusUnauthorized, "invalid_api_key", "missing or invalid API key")
}

// writeAuthFailLimit 运维面鉴权失败超限响应。
func writeAuthFailLimit(w http.ResponseWriter, retry time.Duration) {
	seconds := int((retry + time.Second - 1) / time.Second)
	if seconds < 1 {
		seconds = 1
	}
	w.Header().Set("Retry-After", strconv.Itoa(seconds))
	writeOpenAIError(w, http.StatusTooManyRequests, "too_many_auth_failures", "too many authentication failures")
}

// withClientAuth 网关 API 面（/v1/*）的鉴权：默认密钥或任一未撤销命名密钥。
//
// 命中命名密钥时挂一个用量累加器到 context，请求结束后计入该密钥的请求数/token
// 与 last_used_at；默认密钥命中不在此计数（其用量由 usage 记录器承载）。
// 空默认密钥 + 无命名密钥 = 不鉴权（既有默认，见 httpauth.Authenticate）。
func (h *Handler) withClientAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		res := httpauth.Authenticate(r, h.loadLive().APIKey, h.cfg.Keys.Keys())
		if !res.OK {
			writeClientAuthError(w, r)
			return
		}
		if res.IsDefault || h.cfg.Keys == nil {
			next(w, r)
			return
		}
		release, retryAfter, ok := h.cfg.Keys.Acquire(res.KeyID)
		if !ok {
			h.cfg.Keys.RecordRejected(res.KeyID)
			writeKeyLimitError(w, retryAfter)
			return
		}
		defer release()
		ku := &keyUsage{id: res.KeyID}
		ctx := context.WithValue(r.Context(), keyUsageKey{}, ku)
		if realm := h.cfg.Keys.Realm(res.KeyID); realm != "" {
			ctx = context.WithValue(ctx, keyRealmCtxKey{}, realm)
		}
		next(w, r.WithContext(ctx))
		h.cfg.Keys.RecordUsage(ku.id, ku.prompt, ku.completion, ku.total)
	}
}

func (h *Handler) healthz(w http.ResponseWriter, r *http.Request) {
	total, healthy, _, _, _ := h.cfg.Pool.CountsDetailed() //nolint:dogsled // CountsDetailed 五元组，探活只需前两项
	// 用 ServableNow 判定：healthy>0 但全占满在途时 chat 会 503，探活必须同口径，
	// 否则负载均衡器会把流量持续打进无法受理的实例。
	status := http.StatusOK
	if !h.cfg.Pool.ServableNow() {
		status = http.StatusServiceUnavailable
	}
	// realm_servable 域可服务维度：不改判活语义（存在性探活保持不变），
	// 只新增 CN/global 各自可达性供双域部署运维观察（任一域不可用单独告警）。
	realmServable := map[string]bool{
		"cn":     h.cfg.Pool.ServableForRealm("cn"),
		"global": h.cfg.Pool.ServableForRealm("global"),
	}
	// 恒无鉴权（负载均衡/编排探活只需 2xx/503 语义），身份靠 service 字段 + X-Service 头双保险。
	w.Header().Set("X-Service", ServiceName)
	writeJSON(w, status, map[string]any{
		"healthy":        healthy,
		"total":          total,
		"service":        ServiceName,
		"realm_servable": realmServable,
	})
}

func (h *Handler) status(w http.ResponseWriter, r *http.Request) {
	total, healthy, cooling, disabled, inFlightFull := h.cfg.Pool.CountsDetailed()
	sticky := 0
	if h.cfg.StickyCount != nil {
		sticky = h.cfg.StickyCount()
	}
	redisMode := h.cfg.RedisMode
	if redisMode == "" {
		redisMode = "noop"
	}
	// cost_explore 探索台账（issue #136 §5 可观测性）：累计探索事件数 + 各
	// (域, 模型) 的最近探索时刻（键 "realm|model"）。与 accounts[].model_costs
	// 行对照即可读出「探索→毕业」全链路（单一事实来源，不做双表示）。零回归只增键。
	exploreEvents, exploreLast := h.cfg.Pool.CostExploreStatus()
	writeJSON(w, http.StatusOK, map[string]any{
		"accounts":       h.cfg.Pool.List(),
		"total":          total,
		"healthy":        healthy,
		"cooling":        cooling,
		"disabled":       disabled,
		"in_flight_full": inFlightFull,
		// realm_totals 按域分组的计数汇总（双 realm 并存时运维一眼看到各域可用性）：
		// 只新增字段，既有 total/healthy/cooling/disabled/in_flight_full 汇总键不变（零回归）。
		"realm_totals": map[string]map[string]int{
			"cn":     countsMapFrom(h.cfg.Pool.CountsDetailedForRealm("cn")),
			"global": countsMapFrom(h.cfg.Pool.CountsDetailedForRealm("global")),
		},
		"sticky_sessions": sticky,
		"redis_mode":      redisMode,
		// model_locks 当前有未过期模型级限流的 (域, 模型) 全清单：账号池视图回答
		// 「哪些号不能用」，本键回答「哪些模型不能用、锁了几个号、还要锁多久」。
		// 与 ModelBlocked（请求失败时的单模型判定）互补；无锁时为 null。零回归只增键。
		"model_locks": h.cfg.Pool.ModelLockView(),
		// credit_floor 生效的积分保底值（0 = 关闭）。与 accounts[].credits +
		// model_costs 对照即可判定「某号为何对某模型不出票」。零值也显式写出
		// （运维口径：缺失会让人误以为没记录）。
		"credit_floor": h.cfg.Pool.CreditFloor(),
		// cost_explore 事件与 per-model 时间戳（时间值由 encoding/json 写 RFC3339）。
		"cost_explore": map[string]any{
			"events_total": exploreEvents,
			"per_model":    exploreLast,
		},
	})
}

// countsMapFrom 把 CountsDetailed 五元组打包成 /status 的域分组建模。
func countsMapFrom(total, healthy, cooling, disabled, inFlightFull int) map[string]int {
	return map[string]int{
		"total":          total,
		"healthy":        healthy,
		"cooling":        cooling,
		"disabled":       disabled,
		"in_flight_full": inFlightFull,
	}
}

// dynamicModelsCache 动态模型缓存。
var dynamicModelsCache struct {
	sync.RWMutex

	ids      []upstream.ModelInfo
	fetched  time.Time // 最近一次成功拉取时间
	lastFail time.Time // 最近一次拉取失败时间（负缓存）
}

const (
	// dynamicModelsTTL 模型目录缓存时长。曾是 1h；缩到 10min 对齐「面板实时、
	// API 缓存」的漂移痛点（PR #38 报告）：目录新增模型时面板立即可见，公开
	// /v1/models 最多滞后一个 TTL。再短就不值得——每次失效都是 2 次上游探测。
	dynamicModelsTTL        = 10 * time.Minute
	modelsFetchFailCooldown = 5 * time.Minute
)

// models 返回模型列表：纯动态（缓存 10min），失败/无号返回空列表（无静态兜底——
// 拉不出目录即意味着上游不可用，假名单只会让客户端选到 11102 的模型）。
func (h *Handler) models(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"object": "list",
		"data":   h.modelList(),
	})
}

// fmtCreditsPrefix 从上游 credits 原文提取倍率并格式化为 "[x0.05 credit]"。
// 上游格式不统一："x0.05 credits" / "x0.29" / "x0.00 credits" 等，
// 统一提取 x数字 部分，去 "credits" 后缀。
func fmtCreditsPrefix(raw string) string {
	s := strings.TrimSpace(raw)
	s = strings.TrimSuffix(s, "credits")
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	return "[" + s + " credit]"
}

// applyModelInfoFields 把上游模型对象全字段（ModelInfo）按「空值省略」写出规则
// 合入 /v1/models 条目：name/description/credits/tags/vendor/能力旗标/
// max_allowed_size/reasoning_effort/reasoning_summary。CN 动态分支与 global
// 探测命中分支共用（两域模型对象同构），保证输出字段集一致。
// 不覆盖 id/object/created/owned_by 及调用方先前写好的基础字段；上游未下发的
// 字段（零值）整体省略——不编造。
func applyModelInfoFields(entry map[string]any, mi upstream.ModelInfo) map[string]any {
	if mi.Name != "" {
		entry["name"] = mi.Name
	}
	if mi.Description != "" {
		entry["description"] = modelDescription(mi.Credits, mi.Description)
	}
	if mi.Credits != "" {
		entry["credits"] = mi.Credits // 积分倍率原文（如 "x0.05"），仅展示
	}
	if len(mi.Tags) > 0 {
		entry["tags"] = mi.Tags
	}
	if mi.Vendor != "" {
		entry["vendor"] = mi.Vendor
	}
	if mi.IsDefault {
		entry["is_default"] = true
	}
	applyModelInfoCapabilities(entry, mi)
	return entry
}

// modelDescription 拼接积分倍率前缀与描述文本。倍率前缀：从 "x0.05 credits" /
// "x0.29" 等格式提取纯数字，统一为 "[x0.05 credit]" 前缀，方便下游面板直接展示。
func modelDescription(credits, description string) string {
	if credits != "" {
		return fmtCreditsPrefix(credits) + " " + description
	}
	return description
}

// applyModelInfoCapabilities 合入能力旗标 / 尺寸上限 / 推理档位字段（空值省略）。
func applyModelInfoCapabilities(entry map[string]any, mi upstream.ModelInfo) {
	mods := mi.EffectiveInputModalities()
	// 兼容字段与 input_modalities 同源：image 能力也可能来自 models.dev 推导。
	if containsModality(mods, upstream.ModalityImage) {
		entry["supports_images"] = true
	}
	// 输入模态（issue #109）：已知才宣称（未知 = 省略），normalize 后至少含 text/image
	// 之类的确定值；supports_images 保留作为兼容字段。
	if len(mods) > 0 {
		entry["input_modalities"] = mods
	}
	if mi.SupportsReasoning {
		entry["supports_reasoning"] = true
		if mi.CanDisableThinking {
			entry["can_disable_thinking"] = true
		}
	}
	if mi.SupportsToolCall {
		entry["supports_tool_call"] = true
	}
	if mi.OnlyReasoning {
		entry["only_reasoning"] = true
	}
	if mi.MaxAllowedSize > 0 {
		entry["max_allowed_size"] = mi.MaxAllowedSize
	}
	if mi.ReasoningEffort != "" {
		entry["reasoning_effort"] = mi.ReasoningEffort
	}
	if mi.ReasoningSummary != "" {
		entry["reasoning_summary"] = mi.ReasoningSummary
	}
}

// modelList 模型列表：CN 模型输出统一加 "cn:" 前缀（gateway 路由协议，与 resolveModel
// 对称）；global.enabled=true 时追加 global: 前缀的国际版名单。
// 纯动态：动态拉取失败/无号 → 该域空列表，无静态兜底。
func (h *Handler) modelList() []map[string]any {
	out := make([]map[string]any, 0)
	for _, mi := range h.fetchDynamicModels() {
		out = append(out, h.cnModelEntry(mi))
	}
	// global 模型名单：仅 GlobalEnabled=true 时列出（逃生门）。
	// 名单 = 纯动态探测结果（fetchGlobalModels，失败/无号 → 空）。
	if h.cfg.GlobalEnabled {
		out = append(out, h.globalModelEntries()...)
	}
	return h.appendAliasEntries(out)
}

// appendAliasEntries 把配置的模型别名追加到 /v1/models 列表（issue #52）。
// 别名按名字排序保证输出稳定；与真实模型同 id 时以别名为准（请求解析同样
// 别名优先），避免"列表显示一个、实际路由到另一个"。
func (h *Handler) appendAliasEntries(out []map[string]any) []map[string]any {
	if len(h.cfg.ModelAliases) == 0 {
		return out
	}
	names := make([]string, 0, len(h.cfg.ModelAliases))
	for name := range h.cfg.ModelAliases {
		names = append(names, name)
	}
	sort.Strings(names)
	index := make(map[string]int, len(out))
	for i, entry := range out {
		if id, ok := entry["id"].(string); ok {
			index[id] = i
		}
	}
	for _, name := range names {
		entry := map[string]any{
			"id":       name,
			"object":   "model",
			"created":  1753600000,
			"owned_by": "workbuddy",
			// upstream_id 保留配置里的 "[realm:]model" 原文；省略 realm 时
			// 请求侧裸名按 cn 解析，这里也补上 cn: 便于客户端识别目标域。
			"upstream_id": normalizeAliasTarget(h.cfg.ModelAliases[name]),
		}
		if i, ok := index[name]; ok {
			out[i] = entry
			continue
		}
		out = append(out, entry)
	}
	return out
}

// normalizeAliasTarget 把别名目标规范成 "[realm:]model"（缺省 realm 补 cn）。
func normalizeAliasTarget(target string) string {
	realm, bare, prefixed := splitRealm(target)
	if !prefixed {
		realm = "cn"
	}
	return realm + ":" + bare
}

// cnModelEntry 组装单个 CN 模型条目（realm 前缀 + 四级长度查找 + 全字段 + effort）。
func (h *Handler) cnModelEntry(mi upstream.ModelInfo) map[string]any {
	entry := map[string]any{
		"id":       "cn:" + mi.ID,
		"object":   "model",
		"created":  1753600000,
		"owned_by": "workbuddy",
	}
	// context_length / max_output_tokens 四级查找（upstream.model_catalog）：
	// 上游动态值（maxInputTokens/maxOutputTokens）权威 → 静态种子表 →
	// model.json 本地缓存 → models.dev 按需拉取（异步不阻塞本次响应，拉到后
	// 写 model.json 供下次命中）→ 1M 兜底 / max_output_tokens 省略。
	// 上游零值不再透出假 131072（误导 Codex/ZCode 等按 context_length 提前
	// 截断、白白丢上下文）。
	entry["context_length"] = upstream.ContextWindowListingV4(mi.ID, mi.ContextWindow, h.cfg.Upstream.HTTP)
	if mo, ok := upstream.MaxOutputTokensListingV4(mi.ID, mi.MaxTokens, h.cfg.Upstream.HTTP); ok {
		entry["max_output_tokens"] = mo
	}
	// 上游模型对象全字段透出（name/描述/标签/倍率/能力旗标等，空值省略）。
	entry = applyModelInfoFields(entry, mi)
	// effort 能力透出——远端 supportedEfforts 权威，缺失落到 CN 静态兜底表
	// （客户端可发现档位，不再盲传）。无档位 → 省略字段。
	if efforts, def := upstream.EffortListing("cn", mi.ID, mi.Efforts, mi.DefaultEffort); efforts != nil {
		entry["reasoning_supported_efforts"] = efforts
		if def != "" {
			entry["reasoning_default_effort"] = def
		}
	}
	return entry
}

// globalModelEntries 组装 global 域模型条目：先 fetchGlobalModels（内部探测并落
// effort 桶）拿名单，再按 id 取探测对象形态的全字段富信息（共享同一次探测缓存）。
func (h *Handler) globalModelEntries() []map[string]any {
	globalIDs, globalAccount := h.fetchGlobalModels()
	// 探测对象形态的全字段条目（与 fetchGlobalModels 共享同一次探测缓存）：
	// 命中 id 才透出富字段；窄表/失败 → 零值 ModelInfo（applyModelInfoFields 全空省略）。
	// globalAccount 为 nil（无 global 号）时 FetchGlobalModelInfos 返回 nil。
	globalInfos := map[string]upstream.ModelInfo{}
	for _, mi := range h.cfg.Upstream.FetchGlobalModelInfos(globalAccount) {
		globalInfos[mi.ID] = mi
	}
	globalEfforts, globalDefaults := h.cfg.Upstream.GlobalEffortSnapshot()
	out := make([]map[string]any, 0, len(globalIDs))
	for _, id := range globalIDs {
		out = append(out, h.globalModelEntry(id, globalInfos[id], globalEfforts[id], globalDefaults[id]))
	}
	return out
}

// globalModelEntry 组装单个 global 模型条目（与 CN 动态分支同口径）。
func (h *Handler) globalModelEntry(id string, mi upstream.ModelInfo, effort []string, defaultEffort string) map[string]any {
	entry := map[string]any{
		"id":       "global:" + id,
		"object":   "model",
		"created":  1753600000,
		"owned_by": "workbuddy",
	}
	entry = applyModelInfoFields(entry, mi)
	entry["context_length"] = upstream.ContextWindowListingV4(id, mi.ContextWindow, h.cfg.Upstream.HTTP)
	if mo, ok := upstream.MaxOutputTokensListingV4(id, mi.MaxTokens, h.cfg.Upstream.HTTP); ok {
		entry["max_output_tokens"] = mo
	}
	if efforts, def := upstream.EffortListing("global", id, effort, defaultEffort); efforts != nil {
		entry["reasoning_supported_efforts"] = efforts
		if def != "" {
			entry["reasoning_default_effort"] = def
		}
	}
	return entry
}

// fetchGlobalModels 返回 global 模型名单（纯动态探测结果）及被探测账号。
// 缓存/失败回落封在 upstream.FetchGlobalModels（内部 1h + 5min 负缓存）。
// 本方法只负责"何时探测"：池中无 global 账号 → 空名单 + nil 账号（零上游调用）。
// 返回的 acct 供调用方在同一账号上取富 ModelInfo（FetchGlobalModelInfos 与
// FetchGlobalModels 共享缓存，不会触发第二次上游探测）。
// GlobalEnabled=false 时 modelList 已不进入本分支（逃生门在调用方 gate）。
func (h *Handler) fetchGlobalModels() ([]string, *auth.Auth) {
	acct := h.cfg.Pool.PickExcludingForRealm(nil, "", "global")
	if acct == nil {
		return nil, nil
	}
	return h.cfg.Upstream.FetchGlobalModels(acct), acct
}

// fetchDynamicModels 从第一个可用 CN 账号拉模型列表（含 contextWindow/maxTokens），
// 缓存 10min。
// 选号与 /panel/api/models 完全同口径（AvailableUIDsForRealm("cn") 首个 + AuthByUID），
// 而非 Pool.Pick()：Pick 无 realm 过滤，混合池里可能选中 global 号去打 CN 端点，
// 表现为偶发失败/面板与 /v1/models 两套目录（PR #38 报告并给出的选号修复）。
// 缓存 + 5min 负缓存按既有语义**保留**（#38 原案整体删除缓存被拒）：公开端点逐请求
// 实时拉取 = 每次 2 个上游探测，客户端周期性刷新模型列表会持续打上游；上游故障时
// 无冷却窗口，客户端重试即放大请求量——负缓存正是为此设计（见 handler_test 吸收
// 上游 9832283 的注释）；且 cachedModelsSnapshot（gateway_hint 判定）依赖缓存写入。
func (h *Handler) fetchDynamicModels() []upstream.ModelInfo {
	dynamicModelsCache.RLock()
	if len(dynamicModelsCache.ids) > 0 && time.Since(dynamicModelsCache.fetched) < dynamicModelsTTL {
		out := dynamicModelsCache.ids
		dynamicModelsCache.RUnlock()
		return out
	}
	// 失败负缓存：冷却期内不再请求上游。
	if !dynamicModelsCache.lastFail.IsZero() && time.Since(dynamicModelsCache.lastFail) < modelsFetchFailCooldown {
		dynamicModelsCache.RUnlock()
		return nil
	}
	dynamicModelsCache.RUnlock()

	uids := h.cfg.Pool.AvailableUIDsForRealm("cn")
	if len(uids) == 0 {
		return nil
	}
	acct := h.cfg.Pool.AuthByUID(uids[0])
	if acct == nil {
		return nil
	}
	infos, err := h.cfg.Upstream.FetchModels(acct)
	if err != nil || len(infos) == 0 {
		// 拉取失败只进负缓存（5min lastFail），不 NoteError：NoteError 喂的是 chat
		// 熔断器，models 端点偶发 5xx 跨界惩罚 chat 通道健康的账号；
		// models 拉取失败 ≠ 账号 chat 不可用。
		dynamicModelsCache.Lock()
		dynamicModelsCache.lastFail = time.Now()
		dynamicModelsCache.Unlock()
		return nil
	}
	dynamicModelsCache.Lock()
	dynamicModelsCache.ids = infos
	dynamicModelsCache.fetched = time.Now()
	dynamicModelsCache.lastFail = time.Time{} // 成功则清空负缓存
	dynamicModelsCache.Unlock()
	return infos
}

// cachedModelsSnapshot 只读模型目录缓存（TTL 内快照）；缓存冷/空 → nil。
// 不发起任何上游调用（hint 判定用：错误路径加一次 FetchModels 网络调用既拖慢
// 错误响应、又污染上游调用语义）。
func cachedModelsSnapshot() []upstream.ModelInfo {
	dynamicModelsCache.RLock()
	defer dynamicModelsCache.RUnlock()
	if len(dynamicModelsCache.ids) == 0 || time.Since(dynamicModelsCache.fetched) >= dynamicModelsTTL {
		return nil
	}
	return dynamicModelsCache.ids
}

// chatFlow 一次 /v1/chat/completions 请求的可变状态与阶段方法。
// chatCompletions 原为一个超长热路径函数（gocognit 216），此处按「解析请求 →
// 选号+重试轮转 → 转发与错误分类 → 流式/非流式收尾 → 末端错误透传」拆分为等价
// 阶段；原函数局部变量逐字迁移为字段，各阶段内语句顺序与早返回语义保持不变。
type chatFlow struct {
	h *Handler

	// 传输无关端口（Issue #12）：out 承载结构化结果出口；核心不再持有
	// http.Request / http.ResponseWriter。请求 context.Context 不存字段（containedctx
	// 禁止），而由入口显式逐层传参（取消/超时信号传播到上游与轮转退避）。
	out chatResponder

	// 请求体与解析结果
	body     []byte
	stream   bool
	rawModel string
	realm    string
	// realmErr 命名密钥出口绑定与请求显式前缀冲突（issue #94）；非 nil 时
	// serveChat 在选号前返回 400。
	realmErr      error
	bareModel     string
	modelRate     string
	declaredTools map[string]bool
	reqHasImage   bool
	// reqModalities 请求携带的非文本输入模态（issue #109，改写前取一次，
	// 供 gateway_hint 与能力判定/路由共用，避免重复解析请求体）。
	reqModalities []string

	// 统计与追踪
	st       *chatStat
	clientIP string
	// keyUsage 命名密钥用量累加器（nil = 默认密钥/未鉴权请求），出口回填 token。
	keyUsage *keyUsage

	// 入站头（适配器提取；供 buildChatMeta 组装会话头族）
	convReqID string
	traceID   string

	// 会话标识
	sessKey   string
	turnKey   string
	stickyUID string

	// 轮转状态
	tried           map[string]bool
	lastErr         error
	modelBlock      pool.ModelBlockStatus
	degradedApplied bool
	heldUID         string
	// rotateReason 本轮换号原因（供 /metrics 的 wokbuddy_upstream_rotations_total
	// 标签）。各换号 return rotContinue 分支写入；degrade 复用同号重试、不算换号，
	// 保持空值即跳过计数。
	rotateReason string

	chatMeta upstream.ChatMeta
}

// rotationAction 轮转循环内一次尝试后的控制流去向。
type rotationAction int

const (
	rotContinue rotationAction = iota // 换号继续（continue）
	rotBreak                          // 终止轮转（break），走末端错误透传
	rotDone                           // 已写出响应，直接结束请求
)

func (h *Handler) chatCompletions(w http.ResponseWriter, r *http.Request) {
	// 入站适配：解析 HTTP 请求为结构化输入，并构造输出端口（唯一的 net/http 边界）。
	in, out, ok := newChatInput(w, r, h.cfg.KeepaliveInterval, h.cfg.MaxBodyBytes)
	if !ok {
		return
	}
	h.serveChat(r, in, out)
}

// serveChat 运行一次请求的核心流水线（OpenAI 与 Anthropic 入站适配共用）。
func (h *Handler) serveChat(r *http.Request, in chatInput, out chatResponder) {
	if ku := in.keyUsage; ku != nil && !h.cfg.Keys.ModelAllowed(ku.id, in.model) {
		h.cfg.Keys.RecordRejected(ku.id)
		out.Error(http.StatusForbidden, "model_not_allowed", "model is not allowed for this API key", "")
		return
	}
	f := h.newChatFlow(in, out)
	if f.realmErr != nil {
		f.out.Error(http.StatusBadRequest, "realm_binding_conflict", f.realmErr.Error(), "")
		f.st.status = http.StatusBadRequest
		return
	}
	// 出口统一释放：先释放在途租约，再落请求流水行（与原 defer 顺序一致）。
	defer f.st.done()
	defer f.releaseHeld()
	// 命名密钥用量回填（无累加器时空操作）；在 withClientAuth 落账前完成。
	defer f.recordKeyUsage()
	// 模态能力判定/路由（issue #109）：在选号前完成，不匹配时写 400 并终止。
	if !f.applyModalityRoute() {
		return
	}
	// ctx 显式传参（不入结构体）：取消/超时信号来自入站请求，传播到上游转发与轮转退避。
	if f.runRotation(r.Context()) {
		return
	}
	f.writeFinalError()
}

// backgroundTarget 返回后台模型改写目标；未命中或名单为空返回空串。
func (h *Handler) backgroundTarget(model string) string {
	live := h.loadLive()
	if len(live.BackgroundNames) == 0 {
		return ""
	}
	if !live.BackgroundNames[strings.ToLower(strings.TrimSpace(model))] {
		return ""
	}
	return live.BackgroundTarget
}

// newChatFlow 由结构化输入组装统计、会话标识与轮转状态（传输无关）。
// 入站 HTTP 解析（读 body / 提取头与 IP）已在 newChatInput 完成。
func (h *Handler) newChatFlow(in chatInput, out chatResponder) *chatFlow {
	// 后台任务模型重定向（issue #103）：只改 model 字段，后续 realm/别名/选号/
	// 记账全部基于改写后的模型。名单为空时不改变任何行为。
	if target := h.backgroundTarget(in.model); target != "" {
		log.Printf("INFO: [server] alias: background model %s -> %s", in.model, target)
		in.body = rewriteModel(in.body, target)
		in.model = target
	}
	// realm 前缀解析（D6）：model 名可能带 "[realm:]" 前缀。剥出 realm + bareModel，
	// bareModel 用于选号/粘性/出站 body 重写（前缀是网关侧路由协议，上游只认裸名）。
	// 裸名 → ("cn", 原串)，CN 现状零回归。
	realm, bareModel := resolveRequestModel(in.model, h.cfg.ModelAliases)
	var realmErr error
	if in.keyRealm != "" {
		if err := checkKeyRealmBinding(in.model, h.cfg.ModelAliases, in.keyRealm); err != nil {
			realmErr = err
		} else {
			realm = in.keyRealm
		}
	}
	modelRate := ""
	if h.cfg.Upstream != nil {
		modelRate = h.cfg.Upstream.ModelRate(realm, bareModel)
	}

	// 请求级统计：出口即打一行表格日志（任何路径都会走到）。
	st := newChatStat(time.Now(), in.body, in.stream)
	st.decision = h.sampleDecisionTrace()
	if in.trace != nil {
		in.trace.stat = st
		// 来源在 ServeHTTP 入口采集（此时才知道开关与请求头），此处转交给统计对象，
		// 让 stdout 流水行与归档事件共用同一份来源值，两处不会漂移。
		st.clientIP, st.userAgent = in.trace.clientIP, in.trace.userAgent
	}
	// 请求模态在改写前解析一次（gateway_hint + 能力判定/路由共用）。
	reqMods := requestModalities(in.body)
	f := &chatFlow{
		h: h, out: out, clientIP: in.clientIP,
		body:     in.body,
		stream:   in.stream,
		rawModel: in.model,
		realm:    realm,
		realmErr: realmErr,
		// 工具名名单：上游在没有 tools 的请求里会把工具调用吐成原生标记文本
		// （见 upstream/dsml.go），修复层用它做严格判定。名单来自「本请求声明的
		// tools」与「会话历史里出现过的工具名」两个来源。在提示词/模型名改写之前
		// 取（那些改写不动 tools / messages[].tool_calls 子树）。
		declaredTools: upstream.ToolNameAllowlist(in.body),
		// gateway_hint 判定与模态能力判定所需的请求形态（多模态 part）：在改写前取。
		reqHasImage:   containsModality(reqMods, upstream.ModalityImage),
		reqModalities: reqMods,
		bareModel:     bareModel,
		modelRate:     modelRate,
		st:            st,
		keyUsage:      in.keyUsage,
		tried:         map[string]bool{},
		convReqID:     in.convReqID,
		traceID:       in.traceID,
	}

	// 会话粘性：从请求体提取会话键并解析绑定号（找不到/无效则 stickyUID 为空，
	// 走普通轮换）。ExtractKey 与粘性开关解耦：关闭粘性时会话头族的聚合主键仍按
	// 会话级（RequestIDForKey(sessKey)），不悄悄退化成轮级。
	f.sessKey = session.ExtractKey(in.body)
	if h.cfg.Session != nil && f.sessKey != "" {
		// 按模型解析：绑定号在**当前模型**被 6004 限额时视为不可用 → 重新分配，
		// 而不是钉在限额号上反复失败（"限额后换不动号"的正解）。
		// 必须带 realm 前缀：粘性可用性闭包按 realm 过滤账号，只传裸名会回落 cn，
		// 让 global 请求把会话绑到 cn 号（或每轮重新分配，粘性形同失效）。
		// 别名目标省略 realm 时，f.realm 已是继承请求侧后的结果。
		if uid, ok := h.cfg.Session.ResolveForModel(f.sessKey, f.realm+":"+f.bareModel); ok {
			f.stickyUID = uid
		}
	}

	// 轮级聚合键：按 body 里最后一条 user 消息派生。必须在 prompt.Rewrite 之前取
	// ——改写会动 messages 内容，之后取会让键漂移。
	f.turnKey = session.TurnKey(in.body)

	// 系统提示词改写（出站前、轮转前；每个请求一次）。
	f.applyPromptRewrite()
	// outbound model 名重写为 bareModel（D6）：realm 前缀是网关侧路由协议，
	// 上游不认前缀（global 账号也请求裸模型名）。裸名时 bareModel==peek.Model 恒等。
	if bareModel != in.model {
		f.body = rewriteModel(f.body, bareModel)
	}
	f.buildChatMeta()
	return f
}

// applyPromptRewrite 按 PromptMode 改写出站提示词（轮转前、每请求一次）。
//   - custom：用自有提示词替换客户端 system/developer（从源头消灭 system 指纹误报）。
//   - append：开头连续 system/developer 块后插自有提示词，既有消息逐字不动。
//   - passthrough + 降级期：换 Degraded 中性提示词直达，不再先撞 400。
//   - append + 降级期：退化为 replace（带指纹原文重试是确定性再撞墙）。
func (f *chatFlow) applyPromptRewrite() {
	switch {
	case f.h.cfg.PromptMode == "custom" && f.h.cfg.PromptText != "":
		f.body = prompt.Rewrite(f.body, f.h.cfg.PromptText)
	case f.h.cfg.PromptMode == "append" && f.h.cfg.PromptText != "" && !f.h.degrade.Active():
		f.body = prompt.Append(f.body, f.h.cfg.PromptText)
	case f.canDegrade() && f.h.degrade.Active():
		f.body = prompt.Rewrite(f.body, prompt.Degraded)
		f.degradedApplied = true
	}
}

// canDegrade 报告当前 PromptMode 是否支持降级重试（passthrough / append）。
func (f *chatFlow) canDegrade() bool {
	return f.h.cfg.PromptMode == "passthrough" || f.h.cfg.PromptMode == "append"
}

// buildChatMeta 组装会话头族（对话轮级聚合 ID + 透传 ID），轮转循环外生成一次：
//   - conversationID：body 提取（透传客户端原值，缺省空串——不伪造）；无则回落会话键；
//   - conversationRequestID：入站 X-Conversation-Request-Id 透传优先，否则按粘性 key
//     进程内稳定生成；粘性 key 也空时走轮级兜底（TurnKey/TurnRequestID）。
func (f *chatFlow) buildChatMeta() {
	convID := session.ResolveConversationID(f.body)
	if convID == "" {
		convID = f.sessKey
	}
	f.chatMeta = upstream.ChatMeta{ConversationID: convID}
	if v := f.convReqID; v != "" {
		f.chatMeta.ConversationRequestID = v
	} else if f.turnKey != "" && f.sessKey != "" {
		// 轮级复合键：sessKey 入键防跨会话同轮文本互撞（#170 统一轮级）。
		f.chatMeta.ConversationRequestID = session.TurnRequestID(f.sessKey + ":" + f.turnKey)
	} else if f.turnKey != "" {
		// 无会话键客户端：纯轮级键（既有兜底语义不变，存量会话键值零漂移）。
		f.chatMeta.ConversationRequestID = session.TurnRequestID(f.turnKey)
	} else if f.sessKey != "" {
		// 残留空态兜底（无 user 消息/无可签名内容）：会话级聚合，好于请求级随机。
		f.chatMeta.ConversationRequestID = session.RequestIDForKey(f.sessKey)
	} else {
		// 无会话键也无轮级键：请求级随机（轮转内捕获一次即共享）。
		f.chatMeta.ConversationRequestID = session.TurnRequestID("")
	}
	f.chatMeta.TraceID = f.traceID
}

// releaseHeld 释放在途租约（幂等）。
func (f *chatFlow) releaseHeld() {
	if f.heldUID != "" {
		f.h.cfg.Pool.Release(f.heldUID)
		f.heldUID = ""
	}
}

// unbindSticky 解绑当前会话粘性号（stickyUID 非空时）。幂等：已空则空操作；
// 不会误解绑其他轮的绑定。仅当 Session != nil 时 stickyUID 才会非空。
func (f *chatFlow) unbindSticky() {
	if f.stickyUID != "" {
		f.h.cfg.Session.Unbind(f.sessKey)
		f.stickyUID = ""
	}
}

// fail 在轮转失败分支统一：释放租约 + 若失败号正是粘性号则解绑（下次请求重新分配）。
func (f *chatFlow) fail(uid string) {
	f.releaseHeld()
	if f.stickyUID != "" && uid == f.stickyUID {
		f.unbindSticky()
	}
}

// runRotation 执行选号 + 重试轮转。返回 true 表示已写出响应（成功或请求级错误）。
// 「先到先停」：次数上限 MaxRotate 与总时长预算 RotateBudget（>0 时）任一先到即停。
func (f *chatFlow) runRotation(ctx context.Context) bool {
	// 全局在途满额：在选号前快速失败（issue #92），避免为一个注定拿不到名额的
	// 请求做选号、粘性与轮换。并发竞态仍由 Acquire 的 CAS 兜底。
	if f.h.cfg.Pool.GlobalInFlightFull() {
		f.st.status = http.StatusServiceUnavailable
		return false
	}
	var deadline time.Time
	if f.h.cfg.RotateBudget > 0 {
		deadline = time.Now().Add(f.h.cfg.RotateBudget)
	}
	for i := range f.h.cfg.MaxRotate {
		// 预算检查放在循环顶部（含上一轮退避之后）：退避已把耗时推过预算时立即停。
		if !deadline.IsZero() && !time.Now().Before(deadline) {
			log.Printf("WARN: [server] rotate budget %s exhausted after %d attempt(s): stop rotating",
				f.h.cfg.RotateBudget, i)
			return false
		}
		f.rotateReason = ""
		switch f.attempt(ctx, i) {
		case rotContinue:
			// 换号继续（下一轮）；计入上游轮换指标（issue #13）。degrade 复用同号
			// 重试、并非换号 → rotateReason 保持空，跳过计数。
			if f.rotateReason != "" {
				f.h.metrics.incRotation(f.rotateReason)
				f.recordRotation()
			}
		case rotDone:
			return true
		case rotBreak:
			return false
		}
	}
	return false
}

// recordRotation 把轮换原因写入决策链（上限 8 条）。
func (f *chatFlow) recordRotation() {
	if f.st.decision == nil || len(f.st.decision.Rotations) >= 8 {
		return
	}
	f.st.decision.Rotations = append(f.st.decision.Rotations, f.rotateReason)
}

// attempt 执行一次「选号 → 占名额 → 刷新 token → 转发」尝试，返回控制流去向。
func (f *chatFlow) attempt(ctx context.Context, i int) rotationAction {
	acct := f.pickAccount()
	if acct == nil {
		f.st.status = http.StatusServiceUnavailable
		// 记下"是不是模型级阻塞"。选号返回 nil 有两种完全不同的成因：
		//   (a) 池子真的没有可用号（账号级冷却/在途占满/积分保底）；
		//   (b) 号都在，但每个号都对这个模型处于模型级冷却（11102/6004）。
		// 两者此前都报 no_healthy_account，导致「模型不可用」被读成「号全挂了」。
		f.modelBlock = f.h.cfg.Pool.ModelBlocked(f.bareModel)
		detail := map[string]any{"model": f.bareModel}
		if f.modelBlock.Blocked {
			detail["model_blocked"] = true
			detail["block_reason"] = f.modelBlock.Reason
		}
		f.h.notify(notify.Event{
			Event:   notify.EventPoolUnavailable,
			Realm:   f.realm,
			Summary: "无可用账号（池健康或在途名额耗尽）",
			Detail:  detail,
		})
		return rotBreak
	}
	f.st.uid = acct.UID
	// 同步昵称：请求流水行只写 uid8 时无法直观看是哪一号，昵称随本次选号带入日志行。
	f.st.nick = acct.Nickname
	f.tried[acct.UID] = true

	// 占用在途名额：Pick 已跳过满额账号，此处 CAS 兜底并发抢名额的竞态。
	if !f.h.cfg.Pool.Acquire(acct.UID) {
		// 全局名额被并发抢走：直接终止，不再换号空转。
		if f.h.cfg.Pool.GlobalInFlightFull() {
			f.st.status = http.StatusServiceUnavailable
			return rotBreak
		}
		return f.retryAcquireFailed(ctx, acct, i)
	}
	f.heldUID = acct.UID

	// token 临近过期 → 先 refresh（失败冷却换号）。
	if proceed, stop := f.ensureFreshToken(ctx, acct, i); !proceed {
		if stop {
			return rotBreak
		}
		f.rotateReason = "refresh"
		return rotContinue
	}
	return f.dispatch(ctx, acct, i)
}

// pickAccount 选号：粘性号优先（PickByUIDForModel 已校验该模型可用性 + 在途未满），
// 否则普通轮换（模型感知 + realm 感知：模型非空时启用 6004 模型级冷却豁免）。
func (f *chatFlow) pickAccount() *auth.Auth {
	if f.stickyUID != "" {
		acct := f.h.cfg.Pool.PickByUIDForModel(f.stickyUID, f.bareModel)
		if acct != nil && (f.realm == "" || acct.Realm() == f.realm) {
			if f.st.decision != nil {
				f.st.decision.StickyHit = true
				f.st.decision.FinalUID = logfmt.UID8(acct.UID)
			}
			return acct
		}
		// 粘性号在当前模型不可用（冷却/占满/该模型被 6004 限额）或 realm 不符 →
		// 解绑，本次回落普通轮换。
		f.unbindSticky()
	}
	if f.st.decision != nil {
		acct, trace := f.h.cfg.Pool.PickExcludingForRealmTrace(f.tried, f.bareModel, f.realm)
		mergePickTrace(f.st.decision, trace)
		return acct
	}
	return f.h.cfg.Pool.PickExcludingForRealm(f.tried, f.bareModel, f.realm)
}

// sampleDecisionTrace 按开关与采样率决定本次是否记录决策链。
func (h *Handler) sampleDecisionTrace() *reqlog.DecisionTrace {
	if !h.cfg.DecisionTrace {
		return nil
	}
	sample := h.cfg.DecisionTraceSample
	if sample > 0 && sample < 1 && rand.Float64() >= sample {
		return nil
	}
	return &reqlog.DecisionTrace{}
}

// mergePickTrace 把 pool 决策快照并入请求级决策链（Top 覆盖为最近一次尝试）。
func mergePickTrace(dst *reqlog.DecisionTrace, src pool.PickTrace) {
	dst.Candidates += src.Candidates
	if dst.Excluded == nil {
		dst.Excluded = map[string]int{}
	}
	for reason, n := range src.Excluded {
		dst.Excluded[reason] += n
	}
	dst.CostTier = src.CostTier
	dst.Explored = dst.Explored || src.Explored
	if len(src.Top) > 0 {
		dst.Top = make([]reqlog.DecisionCandidate, 0, len(src.Top))
		for _, c := range src.Top {
			dst.Top = append(dst.Top, reqlog.DecisionCandidate{UID: c.UID, Weight: c.Weight, Tier: c.Tier})
		}
	}
	if src.FinalUID != "" {
		dst.FinalUID = src.FinalUID
	}
}

// retryAcquireFailed 处置并发抢名额失败：被抢的若是粘性号则立即解绑并回落普通轮换，
// 避免下一轮仍撞同一个满载粘性号再浪费一次 PickByUID 往返。
func (f *chatFlow) retryAcquireFailed(ctx context.Context, acct *auth.Auth, i int) rotationAction {
	if f.stickyUID != "" && acct.UID == f.stickyUID {
		f.unbindSticky()
	}
	if !rotateBackoff(ctx, i) {
		// 客户端已断连：换号重试无意义，终止轮转走末端错误透传。
		return rotBreak
	}
	f.rotateReason = "acquire"
	return rotContinue
}

// ensureFreshToken 在 token 临近过期时刷新。返回 (proceed, stop)：proceed=false 表示
// 刷新失败已按策略处置（stop=true → 终止轮转，false → 换号继续）。
func (f *chatFlow) ensureFreshToken(ctx context.Context, acct *auth.Auth, i int) (proceed, stop bool) {
	if !acct.NeedsRefresh(f.h.cfg.RefreshSkew) {
		return true, false
	}
	if err := f.h.cfg.Upstream.RefreshToken(acct); err != nil {
		f.lastErr = err
		f.penalizeRefreshFailure(acct, err)
		f.fail(acct.UID)
		if !rotateBackoff(ctx, i) {
			return false, true // ctx 取消：终止轮转（refresh 失败换号退避）
		}
		return false, false
	}
	if err := acct.SaveAtomic(); err != nil {
		// 刷新成功但落盘失败：下次启动会用旧 token，必须暴露。
		log.Printf("ERR: [server] chat refresh acct=%s: save auth failed: %v", logfmt.Label(acct.UID, acct.Nickname), err)
	}
	return true, false
}

// penalizeRefreshFailure refresh 失败按分类处置：session 死亡禁用，否则喂熔断计数。
func (f *chatFlow) penalizeRefreshFailure(acct *auth.Auth, err error) {
	var ue *upstream.Error
	if errors.As(err, &ue) && ue.Kind == upstream.ErrSessionDead {
		f.h.cfg.Pool.Disable(acct.UID, "refresh session dead")
		f.h.notifyAccount(acct.UID, notify.EventSessionDead, "账号 session 失效，已禁用，需重新登录",
			map[string]any{"stage": "refresh"})
		return
	}
	f.h.cfg.Pool.NoteError(acct.UID)
}

// dispatch 发起一次上游转发，按错误分类与响应形态分派收尾。
func (f *chatFlow) dispatch(ctx context.Context, acct *auth.Auth, i int) rotationAction {
	// 客户端 IP 按请求传递（PassthroughIP 开启时注入；消除共享字段竞态）。
	attemptStarted := time.Now()
	rc, status, respBody, terr := f.h.cfg.Upstream.ChatStreamContext(ctx, acct, f.body, f.clientIP, f.chatMeta)
	// 分类信封一次成型：upstream 已在错误路径返回 *upstream.Error（Kind +
	// Retry-After 头解析）。传输层错误（非 *Error）走抖动换号分支；防御分支
	// （terr 为 nil 但 status>=400，如 ErrNone 兜底）回落本地 Classify，双保险。
	var uerr *upstream.Error
	if errors.As(terr, &uerr) {
		status = uerr.Status
	}
	if uerr == nil && terr != nil {
		return f.handleTransportError(ctx, acct, terr, attemptStarted, i)
	}
	if status >= 400 {
		return f.handleHTTPError(ctx, acct, status, respBody, uerr, attemptStarted, i)
	}
	// 成功判定与粘性绑定一律**延后到这一跳真正成功之后**（见下方流式/非流式分支）：
	// 上游「200 已开流 + 一帧 error」是真实形态（6004 限流、内容拦截、审核）。
	if f.stream {
		return f.handleStreamSuccess(ctx, acct, rc, attemptStarted)
	}
	return f.handleNonStreamSuccess(ctx, acct, rc, attemptStarted, i)
}

// handleTransportError 处置传输层错误（terr 非 nil 且非 *upstream.Error 信封）。
func (f *chatFlow) handleTransportError(ctx context.Context, acct *auth.Auth, terr error, attemptStarted time.Time, i int) rotationAction {
	// 上游超时 / 停滞：**不换号、不罚号**。超时不是账号的问题：同一份请求换到
	// 别的号撞上的是同一个慢上游，只会把客户端拖到 MaxRotate × header_timeout。
	// 判定三态见 isUpstreamTimeout；客户端主动断连时 ctx 已取消，走抖动分支。
	if isUpstreamTimeout(terr, ctx.Err() != nil) {
		f.recordAttempt(acct.UID, pool.TokenUsageDelta{}, 0, false, attemptStarted, 0)
		f.st.status = http.StatusServiceUnavailable
		f.lastErr = fmt.Errorf("%w: %w", errUpstreamTimeout, terr)
		log.Printf("WARN: [server] upstream timeout acct=%s: %v (rotation stopped, account not penalized)",
			logfmt.Label(acct.UID, acct.Nickname), terr)
		return rotBreak
	}
	// 出站代理层拨号失败（issue #15）：不是账号的问题，且同一请求换号仍走同一代理池
	// ——终止轮转、不惩罚账号，代理池自身已轮换/冷却。不调用 recordAttempt：连接从未
	// 建立，不算一次上游尝试，避免喂账号健康台账（issue #16 的错误压力）与连败降权。
	if errors.Is(terr, outbound.ErrProxyDial) {
		f.st.status = http.StatusServiceUnavailable
		f.lastErr = fmt.Errorf("proxy: %w", terr)
		log.Printf("WARN: [server] outbound proxy dial failed acct=%s: %v (rotation stopped, account not penalized)",
			logfmt.Label(acct.UID, acct.Nickname), terr)
		return rotBreak
	}
	// 网络层抖动：只换号，不喂熔断计数（传输层错误对连续失败连坐熔断过于严苛）。
	// 连败兜底（issue #114）：喂连败计数——连不上上游是「不知道原因的失败」。
	f.recordAttempt(acct.UID, pool.TokenUsageDelta{}, 0, false, attemptStarted, 0)
	f.st.status = http.StatusServiceUnavailable
	f.lastErr = terr
	f.h.cfg.Pool.NoteFailures(acct.UID)
	f.fail(acct.UID)
	if !rotateBackoff(ctx, i) {
		return rotBreak // ctx 取消：终止轮转（传输层错误换号退避）
	}
	f.rotateReason = "transport"
	return rotContinue
}

// handleHTTPError 处置上游 >=400 响应：分类 → 降级重试 / 请求级 fail-fast / 冷却换号。
func (f *chatFlow) handleHTTPError(
	ctx context.Context, acct *auth.Auth, status int, respBody []byte,
	uerr *upstream.Error, attemptStarted time.Time, i int,
) rotationAction {
	f.recordAttempt(acct.UID, pool.TokenUsageDelta{}, 0, false, attemptStarted, 0)
	f.st.status = status
	var kind upstream.ErrKind
	if uerr != nil {
		kind = uerr.Kind
	} else {
		kind = upstream.Classify(status, string(respBody))
		uerr = &upstream.Error{Kind: kind, Status: status, Msg: string(respBody)}
	}
	// 内容拦截误报（passthrough/append 模式首遇）：判定为 system 指纹误报，触发降级
	// 到次日 00:00 CST，换 Degraded 中性提示词同请求内重试（append 降级重试同样退化
	// 为 replace——原文在场只会确定性再撞 400）。第二次仍被拦 → 回内容防火墙错误。
	// 内容问题非账号问题：applyErrorPolicy 不罚账号。
	if kind == upstream.ErrContentBlocked && f.canDegrade() && !f.degradedApplied {
		f.h.degrade.Trigger()
		f.body = prompt.Rewrite(f.body, prompt.Degraded)
		f.degradedApplied = true
		delete(f.tried, acct.UID) // 单账号池也能拿到重试机会（降级重试占一次名额）
		f.releaseHeld()
		log.Printf("content-blocked (likely fingerprint false positive) -> degraded prompt retry")
		return rotContinue
	}
	// 请求级错误（内容防火墙 / 上下文超限 / 图片无效 / 请求体畸形）：换任何账号都会
	// 撞同一结果，轮转纯属浪费，透传上游原文立即返回。
	if failFastKind(kind) {
		f.handleRequestLevelError(acct, kind, respBody, uerr)
		return rotDone
	}
	// lastErr 携带完整 body（uerr.Msg 在 upstream 侧截断 200 字符，透传语义要求原文
	// 全量）+ Kind/RetryAfter（末端映射与冷却时长共用）。
	f.lastErr = &upstream.Error{Kind: kind, Status: status, Msg: string(respBody), RetryAfter: uerr.RetryAfter}
	f.h.applyErrorPolicy(acct.UID, kind, string(respBody), f.bareModel, uerr)
	f.fail(acct.UID)
	// WAF IP 级 fail-fast（优先于 rotateBackoff 退避——IP 级拦截时退避无意义）：该次
	// WAF 403 喂入 IP 级状态机，若激活（短窗多号命中）则立即终止轮转，避免把请求放大
	// MaxRotate 倍打同一出口 IP。账号级软冷却已在上方 applyErrorPolicy 照常记账。
	if kind == upstream.ErrWafBlock && f.h.wafIP.noteWaf(acct.UID) {
		f.h.notifyAccount(acct.UID, notify.EventWAFIPBlocked, "出口 IP 触发上游 WAF 级拦截，轮转已停止",
			map[string]any{"model": f.bareModel})
		return rotBreak
	}
	// 状态码换号闸（issue #17，默认关闭）：allowlist 非空时仅列出的状态码才换号；
	// 不在列表则停止换号、透传上游错误（账号惩罚已由 applyErrorPolicy 记账，不变）。
	if !f.rotateAllowedForStatus(status) {
		return rotBreak
	}
	if !rotateBackoff(ctx, i) {
		return rotBreak // ctx 取消：终止轮转（分类错误换号退避）
	}
	f.rotateReason = kind.String()
	return rotContinue
}

// rotateAllowedForStatus 报告该 HTTP 状态码是否允许换号：RotateStatuses 为空时恒 true
// （维持现有按错误分类的换号语义）；非空时为 allowlist 判定。
func (f *chatFlow) rotateAllowedForStatus(status int) bool {
	if len(f.h.cfg.RotateStatuses) == 0 {
		return true
	}
	return f.h.cfg.RotateStatuses[status]
}

// failFastKind 报告 kind 是否属于「换号必然同样失败」的请求级错误。
func failFastKind(kind upstream.ErrKind) bool {
	return kind == upstream.ErrContentBlocked || kind == upstream.ErrPromptTooLong ||
		kind == upstream.ErrImageInvalid || kind == upstream.ErrBadParams
}

// handleRequestLevelError 透传请求级错误：不轮转、message 装上游原文、附 gateway_hint。
//   - ErrContentBlocked：内容命中网关内容防火墙，不罚账号，空 body 兜底可读文案。
//   - ErrPromptTooLong（11115）：上游原文含真实 token 数与上限值，最有价值。
//   - ErrImageInvalid：图片格式/数据无效，同一 body 换号仍是同样解析错误。
//   - ErrBadParams（11101）：请求体解析失败，继续轮转会把确定失败伪装成"号不可用"。
func (f *chatFlow) handleRequestLevelError(acct *auth.Auth, kind upstream.ErrKind, respBody []byte, uerr *upstream.Error) {
	f.h.applyErrorPolicy(acct.UID, kind, string(respBody), f.bareModel, uerr)
	f.fail(acct.UID)
	raw := string(respBody)
	code, fallback := requestLevelErrorCode(kind)
	msg := raw
	if kind == upstream.ErrPromptTooLong {
		msg = promptTooLongMessage(raw)
	} else if strings.TrimSpace(msg) == "" {
		// 空 body 兜底：无上游原文可透传，保留可读分类文案（不编造原文）。
		msg = fallback
	}
	f.out.Error(http.StatusBadRequest, code, msg,
		f.h.hintOf(kind, raw, f.bareModel, f.reqHasImage, uerr))
	f.st.status = http.StatusBadRequest
	f.st.outcome = reqlog.OutcomeHTTPError
}

// requestLevelErrorCode 返回请求级错误的 OpenAI error.code 与空 body 兜底文案。
func requestLevelErrorCode(kind upstream.ErrKind) (code, fallback string) {
	switch kind {
	case upstream.ErrContentBlocked:
		return "content_blocked", "content blocked by upstream content firewall"
	case upstream.ErrPromptTooLong:
		return "prompt_too_long", "prompt is too long"
	case upstream.ErrImageInvalid:
		return "image_invalid", "image request was rejected by upstream"
	case upstream.ErrBadParams:
		return "bad_params", "chat request body was rejected by upstream"
	default:
		return "invalid_request", ""
	}
}

// handleStreamSuccess 流式收尾：透传 SSE、按流尾形态处置账号、记录用量。
func (f *chatFlow) handleStreamSuccess(ctx context.Context, acct *auth.Auth, rc io.ReadCloser, attemptStarted time.Time) rotationAction {
	// 在途流注册（issue #19）：整个透传期间计入，关闭时据此等待排空。
	f.h.streams.enter()
	defer f.h.streams.leave()
	// 流式：透传结束后立即关闭上游 body，避免 defer 在轮转场景下堆积 fd。
	f.st.status = http.StatusOK
	stats := newChatStatsReaderSince(rc, f.st.start)
	// errFrame：上游 error 帧原文（观察者旁路采集），用于流尾的账号处置。
	var errFrame string
	// 标记修复：上游在没有 tools 的请求里会把工具调用吐成原生标记文本，这里在透传前
	// 还原成 delta.tool_calls（见 upstream/dsml.go）。第二参数 true = 名单为空时启用
	// 弱判定——tools 声明在链路上丢失是实测最常见的泄漏成因。
	repair := upstream.NewMarkupRepair(f.declaredTools, true)
	sErr := f.out.Stream(stats, upstream.FrameHintFunc(func() upstream.HintContext {
		return f.h.hintContext(f.bareModel, f.reqHasImage)
	}), repair, func(payload string) { errFrame = payload })
	switch {
	case upstream.IsEmptyStreamError(sErr):
		// 上游 200 但空流（0 有效帧）：StreamHint 已写 error 帧 + [DONE] 兜底（HTTP 头
		// 已发出只能 200），但这是上游缺陷不是成功——观测收敛到 502，与非流式空流
		// →502 upstream_parse 同语义。只认哨兵错误：客户端断连的写失败不误标。
		f.failStreamUpstream(acct, "empty upstream stream (200+0 frames)")
	case upstream.IsTruncatedStreamError(sErr):
		// 上游未发 [DONE] 就以 EOF 收尾（issue #105）：StreamHint 已向客户端补一个
		// [DONE] 兜底收尾，但截断的 content 不是完整回答——按上游失败处置账号。
		f.failStreamUpstream(acct, "upstream stream truncated (EOF without [DONE])")
	case errors.Is(sErr, upstream.ErrSSELineTooLong):
		// 上游单行超过大小上限（issue #113）：StreamHint 已写 error 帧 + [DONE] 收尾。
		// 防御上限命中 = 上游/代理异常，按上游失败处置——绝不按客户端断连记成功。
		f.failStreamUpstream(acct, sErr.Error())
	case errFrame != "":
		// 上游以 error 帧报错（6004 限流 / 内容拦截 / 审核）：按帧内容分类并处置账号
		// ——**不记成功、不清 11102 负缓存、不绑粘性**。
		kind := upstream.FrameKind(errFrame)
		f.h.applyErrorPolicy(acct.UID, kind, errFrame, f.bareModel, nil)
		f.st.status = http.StatusServiceUnavailable
		f.st.outcome = reqlog.OutcomeStreamError
		log.Printf("WARN: [server] stream acct=%s model=%s: upstream error frame kind=%s payload=%s",
			logfmt.Label(acct.UID, acct.Nickname), f.bareModel, kind, logfmt.Truncate(errFrame, 200))
	case sErr != nil && (upstream.IsClientWriteError(sErr) || ctx.Err() != nil):
		// 客户端写失败（断连 / 写超时）或请求 ctx 已取消：上游帧无恙，账号健康
		// ——账号侧照常记成功（与 default 同语义），请求日志归为 Interrupted
		// （人已走，未完成）。ctx 取消可能是客户端断开导致的上游读中断，不能
		// 误判成上游失败（issue #105 non-goal）。
		f.st.outcome = reqlog.OutcomeInterrupted
		f.noteSuccessAndBind(acct)
	case sErr != nil:
		// 上游读错误（连接重置 / idle 超时等，issue #105）：账号不记成功、不绑粘性。
		// 写路径失败已在 writeAll 包 errClientWrite，不会落到这里。
		f.failStreamUpstream(acct, "upstream stream read error: "+sErr.Error())
	default:
		// 真成功：这一跳读完且上游没有报错，才记成功并让粘性跟上。
		f.st.outcome = reqlog.OutcomeSuccess
		f.noteSuccessAndBind(acct)
	}
	f.finishStreamStats(acct, stats, repair, attemptStarted)
	rc.Close()
	return rotDone
}

// failStreamUpstream 处置流式路径的上游失败（空流 / 截断，issue #105）：
// HTTP 头已发出只能 200，但观测收敛到 502 upstream_parse 同语义；账号**不记成功、
// 不绑粘性**。已向客户端写出过帧（截断流的常态）无法换号重试，此时至少解绑粘性，
// 下次请求重新分配，不把会话钉在坏号上。
func (f *chatFlow) failStreamUpstream(acct *auth.Auth, reason string) {
	f.st.status = http.StatusBadGateway
	f.st.outcome = reqlog.OutcomeStreamError
	f.unbindSticky()
	log.Printf("WARN: [server] stream acct=%s model=%s: %s (account not marked success)",
		logfmt.Label(acct.UID, acct.Nickname), f.bareModel, reason)
}

// noteSuccessAndBind 记录账号成功、清 11102 负缓存，并让会话粘性跟随最终成功号。
func (f *chatFlow) noteSuccessAndBind(acct *auth.Auth) {
	f.h.cfg.Pool.NoteSuccess(acct.UID)
	// 11102 负缓存清命：该账号该模型实测成功，立即解除避让（不必等 TTL 到期）。
	// BlockModelClear 按 "11102" reason 前缀识别，只清 11102 条目、不碰 6004 独立冷却。
	f.h.cfg.Pool.BlockModelClear(acct.UID, f.bareModel)
	if f.sessKey != "" && f.h.cfg.Session != nil {
		f.h.cfg.Session.Bind(f.sessKey, acct.UID)
	}
}

// finishStreamStats 记录流式收尾的用量/速率/缓存/成本观测。
func (f *chatFlow) finishStreamStats(acct *auth.Auth, stats *chatStatsReader, repair *upstream.MarkupRepair, attemptStarted time.Time) {
	credit, hasCredit := stats.Credit()
	if hit, miss, ok := stats.CacheTokens(); ok {
		f.st.cacheHit, f.st.cacheMiss, f.st.hasCache = hit, miss, true
	}
	f.recordAttempt(acct.UID, stats.Usage(), credit, hasCredit, attemptStarted, stats.TTFB())
	// WARN 信号放 recordAttempt 之后：st.promptTokens 此时才是本次的观测值。
	if f.st.hasCache {
		cacheMissWarn.noteCacheTokens(f.bareModel, f.st.promptTokens, f.st.cacheHit, f.st.cacheMiss)
	}
	if n := repair.Converted(); n > 0 {
		// 修复命中：上游本来会把工具调用吐成正文标记，这里已还原成 tool_calls。
		log.Printf("INFO: [server] stream acct=%s model=%s: repaired %d native tool call(s) from assistant text (tool names known=%d)",
			logfmt.Label(acct.UID, acct.Nickname), f.bareModel, n, len(f.declaredTools))
	} else if m := repair.Seen(); m > 0 {
		// 识别到标记却没还原：单独记一行，排障时能一眼区分「没识别到」与「识别到但拒绝」。
		log.Printf("WARN: [server] stream acct=%s model=%s: saw %d native tool call block(s) but repaired none (tool names known=%d)",
			logfmt.Label(acct.UID, acct.Nickname), f.bareModel, m, len(f.declaredTools))
	}
	f.st.ttfb = stats.TTFB()
	// usage 缺失时保留 chatStat.toks 的 -1 哨兵（观测缺失 → 显示 "-"），不写入零值。
	if toks, hasUsage := stats.Tokens(); hasUsage {
		f.st.toks = toks
	}
	// 成本账本：末帧 usage 带 credit 与 token 总数时记录实测单价。
	if hasCredit {
		f.st.credit = credit
		f.st.hasCredit = true
		if total, tok := stats.TotalTokens(); tok && total > 0 {
			f.h.cfg.Pool.NoteModelCost(acct.UID, f.bareModel, credit, total)
		}
	}
}

// handleNonStreamSuccess 非流式收尾：聚合 SSE、修复标记工具调用、记录用量并写响应。
// 聚合失败（空流 / EOF 截断，issue #105）时客户端尚未收到任何字节，换号重试。
func (f *chatFlow) handleNonStreamSuccess(ctx context.Context, acct *auth.Auth, rc io.ReadCloser, attemptStarted time.Time, i int) rotationAction {
	resp, err := upstream.Aggregate(rc)
	rc.Close()
	if err != nil {
		f.recordAttempt(acct.UID, pool.TokenUsageDelta{}, 0, false, attemptStarted, 0)
		// 上游流解析失败（空流 / EOF 截断，issue #105）：截断的 content 不是完整
		// 回答，绝不当成功写回。非流式路径客户端还没看到任何字节，可以换号重试
		// （空流/截断是上游缺陷，换个号大概率能拿到完整流）；轮转耗尽才回 502。
		// fail：释放本次在途租约（否则下一轮 heldUID 被覆盖后该账号名额永不归还）
		// + 失败号正是粘性号时解绑。
		f.fail(acct.UID)
		f.st.status = http.StatusBadGateway
		f.lastErr = fmt.Errorf("upstream_parse: %w", err)
		log.Printf("WARN: [server] non-stream acct=%s model=%s: %v (rotating)",
			logfmt.Label(acct.UID, acct.Nickname), f.bareModel, err)
		if !rotateBackoff(ctx, i) {
			return rotBreak
		}
		f.rotateReason = "parse"
		return rotContinue
	}
	// 标记修复（非流式）：把正文里的原生工具调用标记还原成 message.tool_calls。
	// 只在「没有结构化 tool_calls」且「finish_reason 为 stop」时生效（见 dsml.go）；
	// 名单为空时走弱判定（tools 声明在中转环节丢失是常态）。
	mrepair := upstream.RepairAggregatedResponse(resp, f.declaredTools, true)
	if n := mrepair.Converted(); n > 0 {
		log.Printf("INFO: [server] non-stream model=%s: repaired %d native tool call(s) from assistant text (tool names known=%d)",
			f.bareModel, n, len(f.declaredTools))
	} else if m := mrepair.Seen(); m > 0 {
		log.Printf("WARN: [server] non-stream model=%s: saw %d native tool call block(s) but repaired none (tool names known=%d)",
			f.bareModel, m, len(f.declaredTools))
	}
	credit, total, hasCredit := usageCreditTotal(resp)
	if usageMap, ok := resp["usage"].(map[string]any); ok {
		if hit, okH := upstream.UsageCacheHitTokens(usageMap); okH {
			miss, _ := upstream.UsageCacheMissTokens(usageMap)
			f.st.cacheHit, f.st.cacheMiss, f.st.hasCache = int64(hit), int64(miss), true
		}
	}
	f.recordAttempt(acct.UID, usageDeltaFromResponse(resp), credit, hasCredit, attemptStarted, 0)
	if f.st.hasCache {
		cacheMissWarn.noteCacheTokens(f.bareModel, f.st.promptTokens, f.st.cacheHit, f.st.cacheMiss)
	}
	f.out.JSON(http.StatusOK, resp)
	f.st.status = http.StatusOK
	f.st.outcome = reqlog.OutcomeSuccess
	f.st.toks = completionTokens(resp)
	// 非流式同理：聚合成功（无 error 帧、非空流）才算这一跳成功，事后才记成功/绑粘性。
	f.noteSuccessAndBind(acct)
	// 成本账本（非流式）：从聚合响应的 usage 取 credit 与 token 总数。
	if hasCredit {
		f.st.credit = credit
		f.st.hasCredit = true
		f.h.cfg.Pool.NoteModelCost(acct.UID, f.bareModel, credit, total)
	}
	return rotDone
}

// notifyAccount 发送账号级告警（uid 由通知层截为 uid8；账号不存在时只带 uid）。
func (h *Handler) notifyAccount(uid, event, summary string, detail map[string]any) {
	if h.cfg.Notifier == nil {
		return
	}
	e := notify.Event{Event: event, UID: uid, Summary: summary, Detail: detail}
	if st, ok := h.cfg.Pool.Status(uid); ok {
		e.Nickname = st.Nickname
		e.Realm = st.Realm
	}
	h.notify(e)
}

// notify 发送事件（未配置时为空操作）。
func (h *Handler) notify(e notify.Event) {
	if h.cfg.Notifier != nil {
		h.cfg.Notifier.Notify(e)
	}
}

// recordAttempt 记录一次尝试的用量（唯一汇聚点：流式/非流式、成功/失败都走此，
// 与 pool 的每账号累计器同源，两条口径不会漂移）。ttfb 首 token 等待仅流式有观测，
// 非流式传 0（无观测，速率不扣减）。
func (f *chatFlow) recordAttempt(uid string, delta pool.TokenUsageDelta, credit float64, hasCredit bool, started time.Time, ttfb time.Duration) {
	f.st.attempts++
	f.applyDeltaToStat(delta)
	delta.Model = f.modelName()
	latencyMs := attemptLatencyMs(started)
	delta.HasLatencyMs = true
	delta.LatencyMs = latencyMs
	// TTFT 采样（issue #16）：仅流式有观测（首 data 帧）；非流式 ttfb=0 不写。
	if ttfb > 0 {
		ttftMs := ttfb.Milliseconds()
		if ttftMs < 1 {
			ttftMs = 1
		}
		delta.HasTTFTMs = true
		delta.TTFTMs = ttftMs
	}
	// 耗时 histogram（issue #13）：每次上游尝试观测一次，与用量同源；按是否拿到
	// usage 分成功/失败（与 recordUsage 的 ok 同口径），失败不再混入整体分位。
	f.h.metrics.observeRequestDuration(float64(latencyMs)/1000, deltaHasUsage(delta))
	// HasCompletionTokens 是「本次有没有 token 观测」的独立判断，与速率怎么算无关；
	// 速率本身的边界（TTFB 缺失/超界）收在 tokensPerSecond。
	if delta.HasCompletionTokens {
		if tps, ok := tokensPerSecond(delta.CompletionTokens, time.Duration(latencyMs)*time.Millisecond, ttfb); ok {
			delta.HasTokensPerSecond = true
			delta.TokensPerSecond = tps
		}
	}
	f.h.cfg.Pool.RecordTokenUsage(uid, delta)
	// 健康台账（issue #16）：成功口径同 usage（deltaHasUsage），TTFT 有观测才并入。
	// 只供选号降权，不改判定语义。
	f.h.cfg.Pool.RecordModelHealth(uid, delta.Model, delta.TTFTMs, delta.HasTTFTMs, deltaHasUsage(delta))
	// 用量时序记录。ok 以「上游是否给了 usage」判定：空 delta 意味着这次尝试没拿到
	// 任何 token 统计（传输错误 / >=400 / 解析失败），计为失败尝试。失败也计入请求数。
	f.recordUsage(uid, delta, credit, hasCredit)
}

// applyDeltaToStat 把本次 delta 的 token 观测写入请求统计。
func (f *chatFlow) applyDeltaToStat(delta pool.TokenUsageDelta) {
	if delta.HasPromptTokens {
		f.st.promptTokens = delta.PromptTokens
	}
	if delta.HasCompletionTokens {
		f.st.completionTokens = delta.CompletionTokens
	}
	if delta.HasTotalTokens {
		f.st.totalTokens = delta.TotalTokens
	} else if delta.HasPromptTokens || delta.HasCompletionTokens {
		f.st.totalTokens = f.st.promptTokens + f.st.completionTokens
	}
}

// attemptLatencyMs 本次尝试耗时（毫秒，下限 1）。
func attemptLatencyMs(started time.Time) int64 {
	if ms := time.Since(started).Milliseconds(); ms >= 1 {
		return ms
	}
	return 1
}

// modelName 本次尝试记录的模型名：优先裸模型名，缺省回落原始请求名。
func (f *chatFlow) modelName() string {
	if f.bareModel != "" {
		return f.bareModel
	}
	return f.rawModel
}

// recordUsage 写入逐请求用量记录（含缓存/速率/倍率维度）。
func (f *chatFlow) recordUsage(uid string, delta pool.TokenUsageDelta, credit float64, hasCredit bool) {
	if f.h.cfg.Usage == nil {
		return
	}
	realm := "cn"
	if a, ok := f.h.cfg.Pool.Status(uid); ok && a.Realm != "" {
		realm = a.Realm
	}
	f.h.cfg.Usage.Add(time.Now(), realm, uid, delta.Model, usage.Delta{
		PromptTokens:     delta.PromptTokens,
		HasPromptTokens:  delta.HasPromptTokens,
		CompletionTokens: delta.CompletionTokens,
		HasCompletion:    delta.HasCompletionTokens,
		TotalTokens:      delta.TotalTokens,
		HasTotal:         delta.HasTotalTokens,
		Credit:           credit,
		HasCredit:        hasCredit,
		HasCacheTokens:   f.st.hasCache,
		CacheHitTokens:   f.st.cacheHit,
		CacheMissTokens:  f.st.cacheMiss,
		ModelRate:        f.modelRate,
		LatencyMs:        delta.LatencyMs,
		HasLatency:       delta.HasLatencyMs,
		TTFTMs:           delta.TTFTMs,
		HasTTFT:          delta.HasTTFTMs,
		TokensPerSecond:  delta.TokensPerSecond,
		HasTPS:           delta.HasTokensPerSecond,
	}, deltaHasUsage(delta))
}

// deltaHasUsage 报告本次尝试是否拿到上游 usage（成功口径，与 recordUsage 的 ok 一致）。
func deltaHasUsage(delta pool.TokenUsageDelta) bool {
	return delta.HasTotalTokens || delta.HasCompletionTokens || delta.HasPromptTokens
}

// recordKeyUsage 把本次请求的 token 观测回填到命名密钥用量累加器（无累加器时空操作）。
// 由 chatCompletions 在出口 defer 调用，早于 withClientAuth 的落账。
func (f *chatFlow) recordKeyUsage() {
	if f.keyUsage == nil {
		return
	}
	f.keyUsage.prompt = f.st.promptTokens
	f.keyUsage.completion = f.st.completionTokens
	f.keyUsage.total = f.st.totalTokens
}

// writeFinalError 末端错误透传：轮转耗尽/提前终止后写出最终错误响应。
func (f *chatFlow) writeFinalError() {
	status, code, msg, hint, upstreamMsgPassed := f.finalErrorEnvelope()
	status, code, msg, hint = f.applyModelBlockOverride(status, code, msg, hint, upstreamMsgPassed)
	if seconds := f.retryAfterSeconds(status); seconds > 0 {
		f.out.Header("Retry-After", strconv.Itoa(seconds))
	}
	f.out.Error(status, code, msg, hint)
	f.st.status = status
	f.st.outcome = reqlog.OutcomeHTTPError
}

// retryAfterSeconds 计算 Retry-After 秒数：仅 429/503 且存在可确定的最早解封时间
// 时返回 >0；无可靠时间返回 0（不编造）。
func (f *chatFlow) retryAfterSeconds(status int) int {
	if status != http.StatusTooManyRequests && status != http.StatusServiceUnavailable {
		return 0
	}
	until := f.earliestRecovery(time.Now())
	if until.IsZero() {
		return 0
	}
	seconds := int(math.Ceil(time.Until(until).Seconds()))
	if seconds < 1 {
		seconds = 1
	}
	return seconds
}

// earliestRecovery 汇总账号/模型级冷却与上游 Retry-After，返回最早的未来解封时刻。
func (f *chatFlow) earliestRecovery(now time.Time) time.Time {
	var ue *upstream.Error
	if errors.As(f.lastErr, &ue) && ue.RetryAfter > 0 {
		return now.Add(ue.RetryAfter)
	}
	acc := f.accountRecovery(now)
	model := f.modelRecovery(now)
	switch {
	case acc.IsZero():
		return model
	case model.IsZero() || acc.Before(model):
		return acc
	default:
		return model
	}
}

// accountRecovery 最近尝试账号的账号级/模型级冷却截止（无观测/已过期 → 零值）。
func (f *chatFlow) accountRecovery(now time.Time) time.Time {
	if f.h.cfg.Pool == nil || f.st == nil || f.st.uid == "" {
		return time.Time{}
	}
	st, ok := f.h.cfg.Pool.Status(f.st.uid)
	if !ok {
		return time.Time{}
	}
	until := futureOrZero(st.Until, now)
	for _, m := range st.RateLimitedModels {
		// 只认当前模型：其他模型的冷却与本次请求无关，取它们的最早值会给出
		// 偏短且必然失败的 Retry-After。
		if f.bareModel != "" && m.Model != f.bareModel {
			continue
		}
		until = earlierFuture(until, m.Until, now)
	}
	return until
}

// earlierFuture 返回 cur 与 cand 中更早的未来时刻；两者都已过期时返回零值。
func earlierFuture(cur, cand, now time.Time) time.Time {
	t := futureOrZero(cand, now)
	if t.IsZero() || (!cur.IsZero() && !t.Before(cur)) {
		return cur
	}
	return t
}

// modelRecovery 模型级全池冷却截止（未触发 → 零值）。
func (f *chatFlow) modelRecovery(now time.Time) time.Time {
	if f.h.cfg.Pool == nil {
		return time.Time{}
	}
	mb := f.h.cfg.Pool.ModelBlocked(f.bareModel)
	if !mb.Blocked {
		return time.Time{}
	}
	return futureOrZero(mb.Until, now)
}

// futureOrZero 仅保留晚于 now 的时刻。
func futureOrZero(t, now time.Time) time.Time {
	if t.After(now) {
		return t
	}
	return time.Time{}
}

// localErrorEnvelope 识别「本地分类」的轮转终止错误并给出专属信封（ok=true）：
//   - errUpstreamTimeout：轮转已在传输层分支止损（见 isUpstreamTimeout），文案必须
//     与 no_healthy_account 区分——两者的排查方向完全不同（前者是上游慢，后者是号全挂）。
//   - 上游流截断/空流且所有号都如此（issue #105）：502 + upstream_parse，与「解析失败」
//     同口径——客户端别把「上游坏了」读成「号全挂了」而盲目重试。
func localErrorEnvelope(err error, fallbackStatus int, fallbackCode, fallbackMsg string) (status int, code, msg string, ok bool) {
	switch {
	case errors.Is(err, errUpstreamTimeout):
		return fallbackStatus, "upstream_timeout",
			"upstream timed out: rotation stopped (another account would hit the same slow upstream), please retry later",
			true
	case upstream.IsTruncatedStreamError(err), upstream.IsEmptyStreamError(err):
		return http.StatusBadGateway, "upstream_parse", err.Error(), true
	case errors.Is(err, upstream.ErrSSELineTooLong):
		// 上游单行超过防御上限（issue #113）：同样 502 upstream_parse。轮转耗尽
		// 意味着每个号都吐超大行——上游/代理异常，客户端重试同一请求无意义。
		return http.StatusBadGateway, "upstream_parse", err.Error(), true
	case errors.Is(err, upstream.ErrSSEAggregateTooLong):
		// 非流式聚合总量超限（issue #113）：同 502 upstream_parse，不落回
		// no_healthy_account（那会把「上游吐了超大响应」表述成「号全挂了」）。
		return http.StatusBadGateway, "upstream_parse", err.Error(), true
	default:
		return fallbackStatus, fallbackCode, fallbackMsg, false
	}
}

// finalErrorEnvelope 由 lastErr 推导末端错误透传的状态码 / code / message / hint。
// 上游返回（*upstream.Error）→ error.message 装**上游 body 原文**（code/msg/requestId
// 原样保留）。HTTP 状态码按 OpenAI 兼容口径映射类别：ErrSoftRate → 429（限流语义、
// 客户端应等待重试），其余保持 503。本地调度类错误 → 保留自有文案 no_healthy_account。
func (f *chatFlow) finalErrorEnvelope() (status int, code, msg, hint string, upstreamMsgPassed bool) {
	status = http.StatusServiceUnavailable
	code = "no_healthy_account"
	msg = "all accounts are temporarily unavailable, please retry later"
	// gateway_hint（末端透传）：上游错误按 Kind + 原文 + 请求形态判定；本地调度类
	// 错误（无上游原文）固定 no_healthy_account hint。
	hint = upstream.NoHealthyAccountHint()
	if s2, c2, m2, ok := localErrorEnvelope(f.lastErr, status, code, msg); ok {
		// 本地分类错误（超时 / 截断 / 空流 / 大小超限）：属于上游或传输层异常，
		// 不是号池调度失败，不透出 no_healthy_account hint（排查方向完全不同）。
		return s2, c2, m2, "", false
	}
	var ue *upstream.Error
	if !errors.As(f.lastErr, &ue) {
		return status, code, msg, hint, false
	}
	hint = f.h.hintOf(ue.Kind, ue.Msg, f.bareModel, f.reqHasImage, ue)
	switch ue.Kind {
	case upstream.ErrSoftRate:
		status = http.StatusTooManyRequests
		code = "rate_limit_exceeded"
		msg = "rate limited: all accounts are cooling down, please wait a moment and try again"
	case upstream.ErrWafBlock:
		if f.h.wafIP.active() {
			// IP 级拦截措辞（fail-fast 终止路径）：网关出口 IP 被 WAF 拦截、轮转已止损、
			// 窗口过后自动解除。客户端提前重试无意义（换号不换 IP）；有上游原文时原文优先。
			code = "waf_ip_blocked"
			msg = "waf ip-level block: upstream firewall is blocking the gateway IP, rotation stopped; retry after the block window expires"
		}
	case upstream.ErrModelBlocked:
		// 11102「该后端无此模型」：上游原文已经写清原因，但 code 此前停在默认的
		// no_healthy_account（"服务端过载"语义，客户端会不断重试）。用 400 + 明确 code
		// 把不可重试的性质讲清楚——真正该做的是换个模型。
		code = "model_unavailable"
		status = http.StatusBadRequest
	default:
		// 其余错误（ErrNone 等）：保持通用 503 + no_healthy_account 文案。
	}
	if s := strings.TrimSpace(ue.Msg); s != "" {
		// 上游原文优先：透传 code/msg/requestId，不拼接本地前缀。
		msg = s
		upstreamMsgPassed = true
	}
	return status, code, msg, hint, upstreamMsgPassed
}

// applyModelBlockOverride 模型级阻塞覆盖通用文案（放在最后 = 优先级最高）。
// 为什么能覆盖 code 而不丢信息：modelBlock.Reason 就是 BlockModelBackoff 存下的上游
// 原因，已在 hint 里原样带出，所以覆盖 message 不会丢掉上游信息，反而补上了上游不会
// 告诉客户端的两件事——「有几个号被挡」和「最早什么时候解封」。用 400 而非 503：
// 这是"你选的这个模型当前不可用"，不是"服务端暂时过载"，换号重试必然同样失败。
func (f *chatFlow) applyModelBlockOverride(status int, code, msg, hint string, upstreamMsgPassed bool) (int, string, string, string) {
	if !f.modelBlock.Blocked {
		return status, code, msg, hint
	}
	code = "model_unavailable"
	// 有上游原文时保留它（含 requestId，用户要拿去向上游反馈），只把"有几个号被挡、
	// 最早何时解封"这类本地调度信息放进 gateway_hint。
	if !upstreamMsgPassed {
		msg = "model is unavailable on every account (per-model cooldown), try another model"
	}
	// 阻塞范围（issue #29）：平台级（上游统一限流，等上游恢复）与账号级（换号/等本号
	// 恢复）对运维的处置完全不同，在 hint 里点明。
	if f.modelBlock.Scope == pool.ModelBlockScopePlatform {
		hint = fmt.Sprintf("model_blocked: upstream platform-level rate limit on this model (%d account(s) share the same reset)", f.modelBlock.Count)
	} else {
		hint = fmt.Sprintf("model_blocked: %d account(s) cooling down this model (account-level)", f.modelBlock.Count)
	}
	if !f.modelBlock.Until.IsZero() {
		hint += "; earliest unblock at " + f.modelBlock.Until.Format(time.RFC3339)
	}
	if s := strings.TrimSpace(f.modelBlock.Reason); s != "" {
		hint += "; upstream: " + s
	}
	return http.StatusBadRequest, code, msg, hint
}

// tokensPerSecond 计算吐字速率（token/s），返回 (速率, 是否有意义)。
//
// 分母用「生成耗时」= 端到端耗时 - 首 token 等待（TTFB），不是端到端耗时。
//
// 为什么要减：不减的话，首 token 等待越长、报告速率被压得越低。同一模型换个
// 上游或网络，TTFB 从 0.3s 涨到 2s，速率能凭空掉一半——读起来像"模型变慢了"，
// 其实只是排队久了。issue #34 报的正是这个，维护者也确认「速率计算时并没有减去
// 首token到达时间」。
//
// ttfb<=0 表示没有观测：非流式回复天然没有「首个 data 帧」（日志里那一列记的是
// "-"）。此时不猜、不扣——凭空假定一个 TTFB 会把分母推向零、把速率抬成虚高，
// 比不扣更糟。只有真测到才扣。
//
// 同理，ttfb 不小于总耗时时（时钟粒度、或 TTFB 落在计时终点之后）退回端到端耗时，
// 避免零/负分母。token 数为负哨兵值（-1 = 观测缺失）时返回 false。
//
// 扣除后不足 minGenWindow 也退回端到端（issue #127）：ttfb 量的是「首个 SSE 帧
// 到达」，当上游把整个响应攒到最后一次性下发（假流式/中间层攒批刷新——首帧与
// 末帧几乎同时到）时，total−ttfb 只剩几毫秒，拿它当分母会把几百 token 除成
// 上万 tok/s 的幻数。这种形态下「生成时长」根本不可测，诚实的分母只有端到端。
//
// 用量账本（handler）与控制台流水行（logging.go）都走这一个函数：两处各算一遍时
// 口径漂移过一次（流水行漏扣 TTFB、与面板数字对不上），共用是防再次分叉的唯一办法。
func tokensPerSecond(completionTokens int64, total, ttfb time.Duration) (float64, bool) {
	if completionTokens < 0 || total <= 0 {
		return 0, false
	}
	gen := total
	if ttfb > 0 {
		if g := total - ttfb; g >= minGenWindow {
			gen = g
		}
	}
	return float64(completionTokens) / gen.Seconds(), true
}

// minGenWindow 可信生成窗口的下限：扣除 TTFB 后剩余窗口不足该值即视为「生成
// 时长不可测」，退回端到端耗时（见 tokensPerSecond 注释，issue #127）。
// 真流式下首帧到末帧通常铺满剩余窗口，200ms 远低于正常生成时长，不影响真实
// 快速输出（如缓存命中后的爆发）被如实报告。
const minGenWindow = 200 * time.Millisecond

// promptTooLongMessage 11115 透传 message：上游 body 原文（含真实 token 数/
// 上限值/requestId，客户端自行排查）；空 body 兜底为可读分类短文案（不编造原文）。
func promptTooLongMessage(body string) string {
	if strings.TrimSpace(body) == "" {
		return "prompt is too long"
	}
	return body
}

// usageCreditTotal 从聚合响应取 usage.credit 与 total_tokens（成本台账非流式入口）。
// 任一字段缺失/非法 → ok=false（不记录）。
func usageCreditTotal(resp map[string]any) (credit float64, total int, ok bool) {
	usageMap, _ := resp["usage"].(map[string]any)
	if usageMap == nil {
		return 0, 0, false
	}
	c, _ := usageMap["credit"].(float64)
	t, _ := usageMap["total_tokens"].(float64)
	if t <= 0 {
		return 0, 0, false
	}
	return c, int(t), true
}

// rotateBackoff 轮转间指数退避 + 抖动（WAF 403 修复 P0-2）：第 i 次轮转失败
// （continue 换号前）等待 backoffAfter(i)（500ms·2^i 封顶 8s，±25% 抖动），
// ctx 取消（客户端断连/优雅停机）返回 false——调用方立即终止轮转（客户端已走，
// 换号重试无意义）。退避是「换号前歇一下」让上游频控窗口滑过；正常单号请求
// （首次成功）不经过本函数，零开销。
func rotateBackoff(ctx context.Context, i int) bool {
	d := backoffAfter(i)
	if d <= 0 {
		return ctx.Err() == nil
	}
	if !sleepCtx(ctx, d) {
		log.Printf("WARN: [server] rotate backoff aborted: ctx canceled")
		return false
	}
	return true
}

// errUpstreamTimeout 上游超时的哨兵：末端出口据此给出与「没号可用」可区分的文案。
var errUpstreamTimeout = errors.New("upstream timeout")

// isUpstreamTimeout 判断这一跳的失败是否属于「上游超时 / 停滞」。超时不是账号的
// 问题，换号注定白换（同一份请求撞同一个慢上游），必须止损：不轮转、不罚号。
// 三态判定见传输层错误分支的注释。
func isUpstreamTimeout(err error, clientGone bool) bool {
	if err == nil {
		return false
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return true
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, os.ErrDeadlineExceeded) {
		return true
	}
	return !clientGone && errors.Is(err, context.Canceled)
}

// applyErrorPolicy 按错误分类对账号施加冷却/禁用/熔断策略（最终版状态机）。
// kind 是唯一权威分类（来自 upstream.Classify / ChatStreamContext 的 *Error 信封），
// 此处不再按原始 status 二次判断。仅在 chatCompletions 轮转循环内调用：内容拦截
// 会立即 400 返回，其余种类 continue 换号（continue 前由 rotateBackoff 退避）。
//
// 十条路径，各司其职：
//   - ErrHardCredit → CooldownUntilTomorrow4AM：即时硬冷却到次日 04:00（等签到恢复）。
//   - ErrSoftRate → 优先对齐上游重置墙钟（带「将在 … 重置」时 6004 走模型级豁免、
//     非 6004 走账号级，均不指数堆加）；无重置时间才走有界退避。冷却时长优先采信
//     Retry-After 头（uerr.RetryAfter，body 文案墙钟之外的头形态来源）。
//   - ErrWafBlock → 账号级软冷却：**不 Disable**——WAF 403 是 IP/指纹维频控信号，
//     罚过即走、到期自愈。时长优先 Retry-After 头；缺失按 wafCooldownBase(60s)
//     起 · softStreak 指数、封顶 soft_rate_max 的既有 CooldownSoftRate 有界退避。
//     基数经 jitterDur 抖动（防多账号同相位冷却到期再聚团）。
//   - ErrNotFound → Cooldown(CoolSoft, notFoundCooldown 固定 60s)：短冷却防雪崩。
//   - ErrSessionDead → Disable：session 死亡，永久禁用（需人工重登）。
//   - ErrContentBlocked → 不罚账号；passthrough 首遇触发降级重试，最终仍拦则回 400。
//   - ErrBadParams → 不罚账号，且与 ErrPromptTooLong/ErrImageInvalid 同待遇：
//     调用方在轮转循环内即刻 400 透传原文终止（换号必然同样失败）。
//   - ErrPromptTooLong → 11115：请求的问题不是账号的问题。零动作（不冷却/不熔断/
//     不 NoteError、不喂连败），chatCompletions 已直接透传原文返回不轮转。
//   - ErrImageInvalid → 图片格式/数据无效：请求的问题不是账号的问题（同一 body
//     换任何号都会得到相同的解析错误）。零动作（不冷却/不熔断/不 NoteError、
//     不喂连败），chatCompletions 已直接透传原文返回不轮转。
//   - ErrModelBlocked → BlockModelBackoff：(账号, 模型) 11102 负缓存避让。
//   - ErrServer → NoteError：喂单一连续失败计数器 fails + 累计错误 errTotal，
//     达到 breakerThreshold 触发熔断（指数退避）。
//   - 其他（default：ErrClient/ErrNone）→ 只换号不罚（防雪崩），不喂熔断；ErrClient
//     额外喂连败计数（NoteFailures，issue #114）：未知 4xx 连败 N 次临时出池。
//
// body 仅在 ErrSoftRate/ErrAccountFault 分支用于解析重置时间/分野；model 为请求
// 携带的模型名。uerr 是 ChatStreamContext 返回的分类信封（可携带 RetryAfter）；
// 零值/防御路径下为 nil，冷却时长回落既有计算。
func (h *Handler) applyErrorPolicy(uid string, kind upstream.ErrKind, body, model string, uerr *upstream.Error) {
	switch kind {
	case upstream.ErrSoftRate:
		h.applySoftRatePolicy(uid, body, model, uerr)
	case upstream.ErrWafBlock:
		h.applyWafPolicy(uid, uerr)
	case upstream.ErrAccountFault:
		h.applyAccountFaultPolicy(uid, body)
	case upstream.ErrHardCredit, upstream.ErrSessionDead, upstream.ErrNotFound, upstream.ErrServer:
		h.applyLifecyclePolicy(uid, kind)
	case upstream.ErrModelBlocked:
		// 11102「该后端无此模型」：(账号, 模型) 负缓存避让。复用 modelCooldowns 机制
		// （与 6004 同域），选号侧 healthyForModel 对该账号自动避开该模型。
		// 立即换号（本轮 continue），该账号该模型冷却，下次选号避开。
		h.cfg.Pool.BlockModelBackoff(uid, model, upstream.ModelBlockReason)
	case upstream.ErrClient:
		// 未知 4xx 喂连败计数（issue #114）：连续 N 次该形态失败 → 账号临时出池
		// （NoteFailures 达阈降权），单次/偶发不罚（不误伤）。
		h.cfg.Pool.NoteFailures(uid)
	default:
		// ErrNone 与请求/内容类错误（ErrContentBlocked/ErrPromptTooLong/
		// ErrImageInvalid/ErrBadParams）：内容问题非账号问题，零动作不罚账号
		// （无冷却/熔断/NoteError）。ErrNone 属防御路径，语义不明不喂。
	}
}

// applySoftRatePolicy 处置 429/限流文案：优先对齐上游重置墙钟，其次 Retry-After
// 头，最后账号级有界退避。6004 走模型级豁免，非 6004 走账号级。
func (h *Handler) applySoftRatePolicy(uid, body, model string, uerr *upstream.Error) {
	// 统一对齐上游重置时间：只要 body 带「将在 … 重置」，无论业务 code 是
	// 6004 还是 11140 rate-limiting 等形态，都精确冷却到该墙钟、绝不指数堆加。
	//   - 模型级（6004）→ CooldownSoftForModel：写 modelCooldowns[model]，切模型豁免。
	//   - 账号级（非 6004）→ CooldownSoftRate：写账号级 until，不产生模型豁免。
	modelRateLimited := upstream.IsModelRateLimit(body)
	if resetAt, ok := upstream.ParseRateReset(body); ok {
		if modelRateLimited {
			h.cfg.Pool.CooldownSoftForModel(uid, h.softCooldown(), resetAt, model, "6004 model rate limit")
			return
		}
		h.cfg.Pool.CooldownSoftRate(uid, h.softCooldown(), resetAt, "429 rate limit")
		return
	}
	// body 无重置文案但带 Retry-After 头 → 冷却到该时刻（不做指数堆加）。
	// 头优先于「有界退避」，但低于 body 重置文案（文案是上游更权威的口径）。
	if uerr != nil && uerr.RetryAfter > 0 {
		h.cfg.Pool.CooldownSoftRate(uid, h.softCooldown(), time.Now().Add(uerr.RetryAfter), "429 rate limit (retry-after)")
		if modelRateLimited {
			h.cfg.Pool.RecordModelRateLimitAudit(uid, model, "6004 model rate limit (reset unknown)")
		}
		return
	}
	// 无重置时间 → 账号级有界退避（soft_rate 基数起、softStreak 翻倍、封顶
	// soft_rate_max；已在冷却中的兜底探测不翻倍）。基数取 h.softCooldown()
	// （热改优先），管理面板改 soft_rate 后立即生效。
	h.cfg.Pool.CooldownSoftRate(uid, h.softCooldown(), time.Time{}, "429 rate limit")
	if modelRateLimited {
		h.cfg.Pool.RecordModelRateLimitAudit(uid, model, "6004 model rate limit (reset unknown)")
	}
}

// applyWafPolicy WAF 403（无业务信封拦截形态）。软冷却复用 CooldownSoftRate 家族：
// 基数 wafCooldownBase（60s，抖动后落 [45s,75s]）、softStreak 指数升级、封顶
// soft_rate_max、冷却中兜底探测不翻倍——全部继承既有语义。Retry-After 头优先
// （WAF 拦截页可能带该头）。不 Disable。
func (h *Handler) applyWafPolicy(uid string, uerr *upstream.Error) {
	if uerr != nil && uerr.RetryAfter > 0 {
		h.cfg.Pool.CooldownSoftRate(uid, jitterDur(wafCooldownBase), time.Now().Add(uerr.RetryAfter), "waf 403 block (retry-after)")
		return
	}
	h.cfg.Pool.CooldownSoftRate(uid, jitterDur(wafCooldownBase), time.Time{}, "waf 403 block")
}

// applyAccountFaultPolicy 账号级授权/配额故障按 msg 分野（口径与 Classify 的
// accountFaultMarkers 一致）：
//   - "request illegal"（code 11140）→ 账号级**授权封禁**：硬禁用（Disable）。
//   - 14017（trial not activated）→ register 未完成，补完 register 后可能自愈，
//     **保持软冷却**（禁用会让用户补完 register 后仍无法用）。
//
// 大小写不敏感（与 Classify 的 marker 匹配同口径）。
func (h *Handler) applyAccountFaultPolicy(uid, body string) {
	if strings.Contains(strings.ToLower(body), "request illegal") {
		h.cfg.Pool.Disable(uid, "account banned by upstream (11140 request illegal), re-login required")
		return
	}
	h.cfg.Pool.Cooldown(uid, pool.CoolSoft, h.softCooldown(), "account fault (14017)")
}

// applyLifecyclePolicy 账号生命周期类冷却：
//   - ErrHardCredit（402 + 余额关键词即积分耗尽）→ CooldownUntilTomorrow4AM
//     （签到任务 09/21 点恢复），不需异步核查（冗余），立即换号。
//   - ErrSessionDead（12153）→ Disable：session 死亡永久禁用（需人工重登）。
//   - ErrNotFound → Cooldown(CoolSoft, notFoundCooldown 固定 60s)：短冷却防雪崩，
//     不随 soft_rate 退避（偶发路径缺失不是限流信号，不该按限流惩罚升级）。
//   - ErrServer（5xx）→ NoteError：喂单一连续失败计数器，达 breakerThreshold 熔断。
func (h *Handler) applyLifecyclePolicy(uid string, kind upstream.ErrKind) {
	switch kind {
	case upstream.ErrHardCredit:
		h.cfg.Pool.CooldownUntilTomorrow4AM(uid, "余额不足")
	case upstream.ErrSessionDead:
		h.cfg.Pool.Disable(uid, "12153 session dead")
		h.notifyAccount(uid, notify.EventSessionDead, "账号 session 失效（12153），已禁用，需重新登录",
			map[string]any{"stage": "chat"})
	case upstream.ErrNotFound:
		h.cfg.Pool.Cooldown(uid, pool.CoolSoft, notFoundCooldown, "upstream 404")
	case upstream.ErrServer:
		h.cfg.Pool.NoteError(uid)
	default:
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, v any) {
	raw, err := json.Marshal(v)
	if err != nil {
		http.Error(w, "internal encode error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(raw)
}

func writeOpenAIError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{
			"message": msg,
			"type":    "api_error",
			"code":    code,
		},
	})
}

// wafCooldownBase WAF 403 软冷却基数（建议 60s 起；抖动 ±25% 后落 [45s,75s]，
// 实际进入 CooldownSoftRate 后再按 softStreak 指数、封顶 soft_rate_max）。
// 与 SoftCooldown 分流的原因：WAF 403 是 IP/指纹维频控，信号比 429「账号级限流」轻
// （账号本身健康），但比 404 重（带粘性会连环）；60s 级的快速避让已足够让频控窗口
// 滑过。抖动复用 backoff.go jitterDur（单一来源）。
const wafCooldownBase = 60 * time.Second

// writeOpenAIErrorHint 同 writeOpenAIError，另在 error 对象上附加
// error.gateway_hint（hint 为空串时不带字段——未覆盖形态不编造）。
// message 仍是上游原文透传（hint 只做并列补充，绝不替换/包装 message）。
func writeOpenAIErrorHint(w http.ResponseWriter, status int, code, msg, hint string) {
	if hint == "" {
		writeOpenAIError(w, status, code, msg)
		return
	}
	writeJSON(w, status, map[string]any{
		"error": map[string]any{
			"message":      msg,
			"type":         "api_error",
			"code":         code,
			"gateway_hint": hint,
		},
	})
}

// hintContext 组装 chatCompletions 的 gateway_hint 判定上下文：请求裸模型名 +
// 是否带图 + 模型目录 supports_images 声明（目录未收录 → ModelInCatalog=false，
// 不做「不支持」判定，防查不到误判）。仅错误路径调用（成功请求零开销）。
//
// 目录查询只读既有缓存快照（cachedModelsSnapshot），**不触发上游拉取**：错误路径
// 加一次 FetchModels 网络调用既拖慢错误响应、又污染上游调用语义（错误风暴时放大
// 请求量——与 WAF IP fail-fast 的「不放大请求量」哲学相悖）。缓存冷（最近 10min 未
// 拉过）→ ModelInCatalog=false，11133 退中性 hint（宁缺勿滥，不编造能力事实）。
func (h *Handler) hintContext(bareModel string, hasImage bool) upstream.HintContext {
	ctx := upstream.HintContext{Model: bareModel, HasImage: hasImage}
	if bareModel == "" {
		return ctx
	}
	for _, mi := range cachedModelsSnapshot() {
		if mi.ID == bareModel {
			ctx.ModelInCatalog = true
			ctx.ModelSupportsImages = mi.SupportsModality(upstream.ModalityImage)
			return ctx
		}
	}
	return ctx
}

// hintOf 末端错误透传的统一 hint 入口：kind + 上游原文 + 请求上下文 →
// gateway_hint 文案（upstream.GatewayHint 单一事实来源）。uerr 为 nil 时回落
// body 原文判定（防御路径）。transport 层错误（lastErr 非 *upstream.Error 且
// 上游没回 body）→ 无 hint（不编造）。
func (h *Handler) hintOf(kind upstream.ErrKind, body, bareModel string, hasImage bool, uerr *upstream.Error) string {
	msg := body
	if uerr != nil && uerr.Msg != "" {
		msg = uerr.Msg
	}
	return upstream.GatewayHint(kind, msg, h.hintContext(bareModel, hasImage))
}
