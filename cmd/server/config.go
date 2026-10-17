// config.go 加载 JSON 配置 + 环境变量覆盖。
package main

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/BegoniaHe/wokbuddy-demon/internal/notify"
	"github.com/BegoniaHe/wokbuddy-demon/internal/outbound"
	"github.com/BegoniaHe/wokbuddy-demon/internal/prompt"
)

// placeholderAPIKey 是 config.example.json 里的公开占位密钥。
// 它只用于示例，任何读到它的部署都必须在启动时替换（issue #85）。
const placeholderAPIKey = "test_key"

// defaultBackgroundTarget 后台任务模型改写目标（issue #103）。
const defaultBackgroundTarget = "deepseek-v4.1-flash"

// Config 顶层配置。
type Config struct {
	Listen    string `json:"listen"`     // ":7863"
	APIKey    string `json:"api_key"`    // 空 = 不鉴权
	AuthDir   string `json:"auth_dir"`   // ./auths
	StateFile string `json:"state_file"` // ./data/state.json

	Panel struct {
		// PackageDetailLimit 积分构成页单账号默认展示的最近到期包数；<=0 回落 5。
		PackageDetailLimit int `json:"package_detail_limit"`
	} `json:"panel"`

	Logging struct {
		// RequestArchiveEnabled 请求元数据 JSONL 归档开关，缺省 true。
		RequestArchiveEnabled bool `json:"request_archive_enabled"`
		// RequestRetentionDays 归档保留天数，缺省 7；<=0 回落默认。
		RequestRetentionDays int `json:"request_retention_days"`
		// RequestArchiveMaxMB 归档总上限（MiB），缺省 100；<=0 回落默认。
		RequestArchiveMaxMB int `json:"request_archive_max_mb"`
		// RequestClientInfo 是否在请求日志（归档事件 + stdout 流水行 + 面板运行
		// 日志）里记录调用来源：客户端 IP 与 User-Agent。缺省 true。
		//
		// 为什么做成开关而不是恒开：来源信息是排查"谁在打网关"的第一手线索，
		// 但它比 token 计数敏感（IP 属个人信息），共享部署/多租户场景可能需要
		// 关掉。关闭后 Event.ClientIP/UserAgent 保持为空，归档里不出现该字段。
		// 热生效（经 livecfg 快照），无需重启。
		RequestClientInfo bool `json:"request_client_info"`
		// DecisionTrace 是否在请求归档里记录选号决策链（issue #63）；默认 false。
		DecisionTrace bool `json:"decision_trace"`
		// DecisionTraceSample 采样率：0 或 1 = 全部，0<v<1 按比例采样；>1 报错。
		DecisionTraceSample float64 `json:"decision_trace_sample"`
	} `json:"logging"`

	Notify struct {
		// WebhookURL 告警 webhook 地址；空 = 关闭。只支持 http/https，可内嵌 token。
		WebhookURL string `json:"webhook_url"`
		// Events 订阅的事件名；空 = 默认关键事件（session_dead/pool_unavailable/
		// credit_low/waf_ip_blocked/task_failed）。
		Events []string `json:"events"`
	} `json:"notify"`

	Server struct {
		// ReadTimeout 入站请求读取（含 body 上传）总时长上限（issue #100）。
		// http.Server 的 ReadTimeout 覆盖整个请求读取：大上下文/文件块请求经
		// 反代链转发时上传可超过旧固定值 60s，被掐后客户端拿到
		// 400 "read body: ... i/o timeout"。缺省 "300s"；"0" = 不限制
		// （慢速 body 可无限占用连接，自担风险）；改动需重启进程。
		ReadTimeout string `json:"read_timeout"` // "300s"；"0" = 不限制
		// MaxBodyBytes 聊天类入站请求体上限（字节，issue #86），覆盖
		// /v1/chat/completions、/v1/messages 与 count_tokens。超限在解析前返回
		// 413，不进入选号。缺省 16 MiB；0 = 不限制。改动需重启进程。
		MaxBodyBytes int64 `json:"max_body_bytes"`
		// CORSOrigins 允许跨域访问 `/v1/*` 的 Origin 列表（如
		// "http://localhost:3000"）。空 = 关闭 CORS，行为不变。只精确匹配
		// scheme://host[:port]；不支持 "*"，管理面不参与。
		CORSOrigins []string `json:"cors_origins"`
	} `json:"server"`

	Cooldown struct {
		// hard_credit / err_threshold / err_cooldown 三个历史键已退役：
		// 硬冷却固定为次日 04:00（CooldownUntilTomorrow4AM），连续错误语义并入熔断器。
		// 旧 config 中的这些键因 JSON 未知字段而自然忽略，不报错。
		SoftRate string `json:"soft_rate"` // "600s"，软限流冷却基数
		// SoftRateMax 软冷却指数退避的封顶，默认 "2h"。
		// 空值回落默认，非法值报错（处理风格同 soft_rate）。
		SoftRateMax string `json:"soft_rate_max"` // "2h"
	} `json:"cooldown"`

	Schedule struct {
		CheckinHours   []int `json:"checkin_hours"`   // [9,21]
		TravelHours    []int `json:"travel_hours"`    // [9,21]
		ActivityHours  []int `json:"activity_hours"`  // [10]
		KeepaliveHours []int `json:"keepalive_hours"` // [22]
		BlackcatHours  []int `json:"blackcat_hours"`  // [23] 夜猫子窗口（23:00–08:00 计数）
		GrowthHours    []int `json:"growth_hours"`    // [1] 成长任务队列（Sequential 族每日零点解锁，01:00 自动扫描执行）
		// CheckinEnabled/TravelEnabled/ActivityEnabled/KeepaliveEnabled/BlackcatEnabled 显式禁用开关（缺省 true）。
		//
		// 为什么用独立 bool 而不是空数组/哨兵值表意"禁用"：
		//   - 空数组与 null 在老语义里已被"未配置 → 回落默认"占用，改判会静默翻转
		//     所有老 config 的行为（用户只想删掉一行，结果关掉了签到）；bool 缺省 true
		//     则对老配置零影响，向后完全兼容。
		//   - 开关与取值解耦：禁用时仍保留用户显式配的小时，重新启用无需补配。
		//   - 无需猜测哨兵（[-1] 之类），非法小时一律报错并提示改用本开关。
		// 旧 config 里的该键因 JSON 未知字段而自然忽略，不报错。
		CheckinEnabled   bool `json:"checkin_enabled"`   // 缺省 true；false = 关签到
		TravelEnabled    bool `json:"travel_enabled"`    // 缺省 true；false = 完全停猫猫旅行
		ActivityEnabled  bool `json:"activity_enabled"`  // 缺省 true；false = 停活跃上报
		KeepaliveEnabled bool `json:"keepalive_enabled"` // 缺省 true；false = 关 token 保活
		BlackcatEnabled  bool `json:"blackcat_enabled"`  // 缺省 true；false = 关夜猫子
		GrowthEnabled    bool `json:"growth_enabled"`    // 缺省 true；false = 关成长任务自动排程

		// IncludeDisabledInTasks 让「保号类」定时任务（签到 / 活跃上报 / token 保活 /
		// 余额刷新）对**已禁用（disabled）**的账号也执行。
		//
		// 为什么需要它：面板「禁用」的语义是「不再参与选号」（见面板确认文案），但这四类
		// 任务此前一律 `if st.Disabled { continue }`，等于把「停用流量」放大成「停止一切
		// 上游保号行为」——被禁用的号拿不到签到积分、不续 token、余额也不再刷新；而
		// ReenableIfCredits 明确不复活 disabled 账号（见 pool.state.go），于是签到这条唯一
		// 的自动回血路径也断了，账号只能靠人工「解冻」回来。
		//
		// 对「一次只放开一个号、用禁用做流量开关」的轮换用法（同 IP 多号防风控），闲置
		// 待命的号恰恰是最需要签到的那批——本开关即为该用法提供出口。
		//
		// 缺省 false = 保持既有行为，对老配置零影响。打开后禁用号仍会签到 / 保活，但
		// **依旧不参与选号**：pool 选号侧的 disabled 过滤不受本开关影响。
		IncludeDisabledInTasks bool `json:"include_disabled_in_tasks"`

		// 余额后台周期刷新：两次签到时点之间 credits 也能保持新鲜（面板/状态观测用）。
		// 解冻语义同签到（余额 > 0 的冷却账号自动解冻），但不做签到不刷 token。
		BalanceRefreshEnabled bool `json:"balance_refresh_enabled"` // 缺省 true；false = 关闭
		BalanceRefreshMinutes int  `json:"balance_refresh_minutes"` // 缺省 5；<=0 回落 5
		// CheckinAccountDelay 签到任务账号间等待（issue #59）：如 "800ms"；"0" = 不限。
		CheckinAccountDelay string `json:"checkin_account_delay"`
	} `json:"schedule"`

	Models struct {
		// BackgroundNames 客户端后台任务使用的硬编码模型名（issue #103，大小写
		// 不敏感）。命中后 model 改写为 BackgroundTarget；空 = 关闭。
		BackgroundNames []string `json:"background_names"`
		// BackgroundTarget 后台模型改写目标；默认 deepseek-v4.1-flash。
		BackgroundTarget string `json:"background_target"`
		// Aliases 客户端模型别名 → 上游模型（issue #52）：
		// {"claude-sonnet-4-5": "cn:hy4", "gpt-5-codex": "hy4"}。
		// 键必须是裸名（不含 realm 前缀，大小写敏感）；值沿用 "[realm:]model"
		// 语法，省略 realm 时继承请求侧的 realm（裸请求即 cn）。
		// 命中别名后选号/粘性/成本台账/日志一律使用解析后的裸模型名。
		Aliases map[string]string `json:"aliases"`
	} `json:"models"`

	Global struct {
		// Enabled global realm 路由开关。缺省 true：Realm() 正常把 realm=global/
		// domain=workbuddy.ai 的账号判为 global 并路由 global base/路径。
		// 显式 "enabled": false 关闭（逃生门，纯 CN 锁定：即便 auth 写了 realm=global
		// 也不路由，auth.Realm() 双保险的第一道闸）。纯 CN 部署行为不变：CN 账号
		// 恒判 cn，global base 只在 realm=global 的账号上被使用。
		Enabled bool `json:"enabled"`
		// ChatBase / BillingBase 国际版上游 base 覆盖；空 = 回落内置默认
		// https://www.workbuddy.ai（internal/upstream.defaultGlobalBase）。
		ChatBase    string `json:"chat_base"`
		BillingBase string `json:"billing_base"`
	} `json:"global"`

	Upstream struct {
		// TimeoutSeconds 短 RPC（refresh/checkin/balance/FetchModels）总时长上限，默认 120。
		TimeoutSeconds int `json:"timeout_seconds"`
		// HeaderTimeoutSeconds 聊天 SSE 首字节前（响应头）上限；<=0 回落 TimeoutSeconds。
		HeaderTimeoutSeconds int `json:"header_timeout_seconds"`
		// IdleTimeoutSeconds 聊天 SSE 流中空闲上限（活跃吐数据续命不掐）；<=0 回落默认 300。
		IdleTimeoutSeconds int `json:"idle_timeout_seconds"`
		// KeepaliveInterval 入站 SSE 主动心跳间隔（issue #54）：上游静默超过该时长
		// 时向客户端写 ": keepalive" 注释帧，防反代空闲超时断流。"0"/空 = 关闭。
		KeepaliveInterval string `json:"keepalive_interval"`
		// ModelTimeouts 按模型覆盖聊天首字节/流中空闲超时（issue #58）：
		// {"cn:glm-5.2": {"header": "300s", "idle": "600s"}}。键可带 realm 前缀，
		// 未命中回落全局 header_timeout_seconds / idle_timeout_seconds。
		ModelTimeouts map[string]ModelTimeout `json:"model_timeouts"`
		// UserAgent 出站 User-Agent 显式覆盖（非空时全路径生效，优先于默认三段式）。
		// 全部出站请求生效：chat/refresh/checkin/balance/report/travel/FetchModels。
		// 默认值已对齐官方 WorkBuddy 桌面形态（三段式），用户仍可配完全自定义值改写。
		UserAgent string `json:"user_agent"`
		// ClientVersion WorkBuddy 客户端版本段（出站 UA 的 `WorkBuddy/<ver>` 与归属头
		// X-IDE-Version）。空 = 按 realm 取内置默认（cn 5.8.2 / global 5.7.6，对齐官方分发包）。
		ClientVersion string `json:"client_version"`
		// CliVersion 出站 UA 中 `CLI/<ver>` 段的版本。空 = 按 realm 取内置默认
		// （cn 2.161.0 / global 2.156.0，对齐官方内置 CLI）。
		CliVersion string `json:"cli_version"`
		// ClientName 自定义用量归属名：非空时 X-Product 与 X-IDE-Name/Type 跟随该值。
		// 空 = 官方桌面端默认（X-Product="SaaS"、X-IDE-Name/Type="WorkBuddy"）。
		ClientName string `json:"client_name"`
		// DeviceToken 设备风控 Token（X-Device-Token 头）全局兜底；空 = 不注入。
		// 每号 auth 文件的 device_token 键优先于本项。
		DeviceToken string `json:"device_token"`
		// DeviceTokenFile device token 文件路径兜底（宿主落盘的桌面端 token，5 分钟读取缓存）。
		DeviceTokenFile string `json:"device_token_file"`
		// PassthroughIP 是否透传客户端 IP 给上游（默认 false，反代安全边界）。
		PassthroughIP bool `json:"passthrough_ip"`
		// RotateMax 单请求最多换号次数，默认 3（issue #17）。
		RotateMax int `json:"rotate_max"`
		// RotateBudget 轮换总时长预算（如 "90s"）；"0"/空 = 不限（默认）。与
		// RotateMax 构成「先到先停」，避免慢上游把客户端拖久。
		RotateBudget string `json:"rotate_budget"`
		// RotateStatusCodes 非空时，HTTP 状态码错误仅这些码触发换号（如
		// [429,500,502,503,504]）；空 = 维持现有按错误分类的换号语义（默认）。
		RotateStatusCodes []int `json:"rotate_status_codes"`
		// ProxySlots 命名出站代理槽（issue #96）：账号绑定后使用独立 Transport，
		// 未绑定账号继续走 Proxies 全局池/直连。
		ProxySlots []ProxySlot `json:"proxy_slots"`
		// Proxies 出站代理列表（issue #15）：支持 http://、https://、socks5://
		// （可选 user:pass）；非空时出站按健康轮换，失败代理进入冷却。空 = 直连（默认）。
		Proxies []string `json:"proxies"`
	} `json:"upstream"`

	Features struct {
		// SanitizeBlacklistFingerprints 出站请求体黑名单指纹脱敏（默认 true；false 完全还原）。
		SanitizeBlacklistFingerprints bool `json:"sanitize_blacklist_fingerprints"`
		// MultimodalRouting 请求模态与模型能力不匹配时，在同 realm 目录内改选支持
		// 该模态的模型并重写 model（默认 false = 明确 400 model_capability）。
		// issue #109。
		MultimodalRouting bool `json:"multimodal_routing"`
		// LocalWebTools 允许 Responses 请求把 web_search/web_fetch 服务端工具交给
		// 网关本地执行（issue #104）。默认 false = 原样透传。
		LocalWebTools bool `json:"local_web_tools"`
		// LocalWebToolRounds 本地工具最多代跑轮数；<=0 回落 3，>8 钳到 8。
		LocalWebToolRounds int `json:"local_web_tool_rounds"`
	} `json:"features"`

	Prompt struct {
		// Mode passthrough（默认）= 透传客户端原始 system（降级重试仍会切到 Degraded）；
		// custom = 网关用自有系统提示词替换客户端 system/developer；
		// append = 两者并用：开头连续 system/developer 块后插网关 system，既有消息逐字不动（issue #129）。
		Mode string `json:"mode"` // "passthrough" / "custom" / "append"
		// File 提示词文件路径；空 = 内置默认 defaultprompt.md；
		// 路径非空但不可读 → 启动报错（fail fast，避免静默回落到内置默认）。
		File string `json:"file"`
	} `json:"prompt"`

	// PromptText 解析后的系统提示词文本（custom/append 模式使用）。
	PromptText string `json:"-"`

	Upstash struct {
		URL   string `json:"url"`   // 空 = 纯内存模式；支持完整 rediss:// URL 或 https://xxx.upstash.io host
		Token string `json:"token"` // url 非完整连接串时用于组装 rediss://default:<token>@<host>:6379
	} `json:"upstash"`

	// Limits 账号级每日护栏（issue #95）：全局默认 + 可选 cn/global 字段级覆盖。
	Limits struct {
		DailyTokens      int64     `json:"daily_tokens"`
		DailyCredits     float64   `json:"daily_credits"`
		ModelDailyTokens int64     `json:"model_daily_tokens"`
		CN               LimitRule `json:"cn"`
		Global           LimitRule `json:"global"`
	} `json:"limits"`

	// Pricing OpenRouter 等价价格估算（issue #99）；只读展示，不参与调度。
	Pricing struct {
		Enabled        bool    `json:"enabled"`
		RefreshMinutes int     `json:"refresh_minutes"`
		ExchangeRate   float64 `json:"exchange_rate"`
	} `json:"pricing"`

	Pool struct {
		MaxInFlight       int `json:"max_in_flight"`        // 单账号最大在途请求数，0 = 不限
		MaxInFlightGlobal int `json:"max_in_flight_global"` // global 域单账号在途上限（WAF 风控紧域压低并发），0 = 回落默认 2
		// MaxInFlightTotal 网关级全局在途上限（issue #92）；0 = 不限（默认）。
		// 满额时新请求 503，不进入选号。热生效。
		MaxInFlightTotal   int    `json:"max_in_flight_total"`
		BreakerThreshold   int    `json:"breaker_threshold"`    // 连续失败次数触发熔断，默认 3
		BreakerCooldown    string `json:"breaker_cooldown"`     // 基础熔断时长，默认 "30m"
		BreakerCooldownMax string `json:"breaker_cooldown_max"` // 指数退避封顶，默认 "6h"
		// 连败降权（issue #114）：ErrClient/传输层这类「不罚号」失败连续计数，达阈
		// 临时出池。与冷却/熔断并存取更长者不叠加。默认 5 次 / 10m。
		DegradeThreshold   int     `json:"degrade_threshold"`    // 连败次数触发降权，默认 5
		DegradeCooldown    string  `json:"degrade_cooldown"`     // 降权时长（固定，非指数退避），默认 "10m"
		DegradeCooldownMax string  `json:"degrade_cooldown_max"` // 降权时长的上限钳制，默认 "2h"（仅当 cooldown 超该值才钳制）
		IdleWeightPerHour  float64 `json:"idle_weight_per_hour"` // 闲置补偿：每小时未用 +0.5 权重
		IdleWeightMax      float64 `json:"idle_weight_max"`      // 闲置补偿封顶，默认 5.0
		// PreferExpiring 快过期积分加权开关，默认 true。开启且 expiring_soon 窗口内
		// 存在有效批次时，该账号选号权重 ×3（虚拟实例，见 pool 路由加权）；
		// 不按到期时间排序、与批次金额无关（issue #101 对齐实现口径）。
		// 关闭后完全不使用到期信息选号。
		PreferExpiring bool `json:"prefer_expiring"`
		// ExpiringSoon 快过期积分窗口（如 "168h"=7天）：签到/余额刷新时，到期时间在
		// 此窗口内的批次令账号命中上述 ×3 加权；窗口开大 → 命中账号变多、
		// 偏好被稀释。空/0 = 禁用该加权门槛。
		ExpiringSoon string `json:"expiring_soon"`
		// CostExploreInterval costTier 条件探索窗口（issue #136 方案 a′）：tier 0
		// 垄断层存在且 tier 1 有成员时，距上次探索 ≥ 窗口则本次 pick 生效层切
		// tier 1-only（探索=搭车改道，零新增上游请求；成功即毕业，失败走既有
		// 错误策略）。默认 "30m"（≤48 次/天/模型）；"0" 关停（完全回到现状行为）；
		// 空值回落默认。
		CostExploreInterval string `json:"cost_explore_interval"`
		// CreditFloor 积分保底：账号余额低于该值时，对实测收费模型（tier 2）不再
		// 参与选号——防止收费请求把余额打穿、连免费模型都 402 冷却到次日签到。
		// tier 0（免费）/ tier 1（无观测）不受限；签到回血越过 floor 自动恢复。
		// 默认 0 = 关闭；负值钳 0。
		CreditFloor int64 `json:"credit_floor"`
	} `json:"pool"`

	SessionSticky struct {
		Enabled    bool   `json:"enabled"`     // 默认 true
		TTL        string `json:"ttl"`         // 会话绑定 TTL，默认 "30m"
		GCInterval string `json:"gc_interval"` // 会话 GC 周期，默认 "5m"
	} `json:"session_sticky"`

	// 解析后
	SoftRateDur            time.Duration `json:"-"`
	SoftRateMaxDur         time.Duration `json:"-"`
	BreakerCooldownDur     time.Duration `json:"-"`
	BreakerCooldownMaxD    time.Duration `json:"-"`
	DegradeCooldownDur     time.Duration `json:"-"`
	DegradeCooldownMaxD    time.Duration `json:"-"`
	SessionTTL             time.Duration `json:"-"`
	SessionGCInterval      time.Duration `json:"-"`
	BalanceRefreshInterval time.Duration `json:"-"` // 0 = 不启动（enabled=false）
	ExpiringSoonDur        time.Duration `json:"-"`
	// CheckinAccountDelayDur 解析后的签到账号间等待（issue #59）；0 = 不限。
	CheckinAccountDelayDur time.Duration `json:"-"`
	// CostExploreIntervalDur 解析后的 costTier 探索窗口（issue #136）；0 = 关停。
	CostExploreIntervalDur time.Duration `json:"-"`
	// ServerReadTimeoutDur 解析后的入站请求读取上限（issue #100）；0 = 不限制。
	ServerReadTimeoutDur time.Duration `json:"-"`
	// RotateBudgetDur 解析后的轮换总时长预算（issue #17）；0 = 不限。
	RotateBudgetDur time.Duration `json:"-"`
	// KeepaliveIntervalDur 解析后的入站 SSE 心跳间隔（issue #54）；0 = 关闭。
	KeepaliveIntervalDur time.Duration `json:"-"`
}

// LimitRule 每日护栏的一组取值（cn/global 覆盖用）。
type LimitRule struct {
	DailyTokens      int64   `json:"daily_tokens"`
	DailyCredits     float64 `json:"daily_credits"`
	ModelDailyTokens int64   `json:"model_daily_tokens"`
}

// ProxySlot 命名出站代理槽（issue #96）。
type ProxySlot struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	URL     string `json:"url"`
	Enabled bool   `json:"enabled"`
}

// ModelTimeout 单个模型的超时覆盖；空/0 = 回落全局值。
type ModelTimeout struct {
	Header string `json:"header"`
	Idle   string `json:"idle"`
	// 解析后（normalize 填充，不参与 JSON）。
	HeaderDur time.Duration `json:"-"`
	IdleDur   time.Duration `json:"-"`
}

// Default 默认配置。
func Default() *Config {
	c := &Config{
		Listen:    ":7863",
		APIKey:    "",
		AuthDir:   "./auths",
		StateFile: "./data/state.json",
	}
	c.Cooldown.SoftRate = "600s"
	c.Cooldown.SoftRateMax = "2h"
	c.Server.ReadTimeout = "300s"
	c.Server.MaxBodyBytes = 16 << 20
	c.Models.BackgroundTarget = defaultBackgroundTarget
	c.Panel.PackageDetailLimit = 5
	c.Logging.RequestArchiveEnabled = true
	c.Logging.RequestRetentionDays = 7
	c.Logging.RequestArchiveMaxMB = 100
	// 缺省 true 靠显式赋值实现（同 Schedule 开关）：JSON 里键缺席时字段保留此值，
	// 只有显式 false 才关闭来源记录。
	c.Logging.RequestClientInfo = true
	// 与 README / config.example.json 对齐：开启决策链时默认只采样 10%，避免归档写入量突增。
	c.Logging.DecisionTraceSample = 0.1
	c.Schedule.CheckinHours = []int{9, 21}
	c.Schedule.TravelHours = []int{9, 21}
	c.Schedule.ActivityHours = []int{10}
	c.Schedule.KeepaliveHours = []int{22}
	c.Schedule.BlackcatHours = []int{23}
	c.Schedule.GrowthHours = []int{1}
	// 开关「缺省 true」靠这几行实现：Load 先取 Default() 再 json.Unmarshal 覆盖，
	// 键缺席（或为 null）时字段原样保留 true，只有显式 false 才关。
	c.Schedule.CheckinEnabled = true
	c.Schedule.GrowthEnabled = true
	c.Schedule.TravelEnabled = true
	c.Schedule.ActivityEnabled = true
	c.Schedule.KeepaliveEnabled = true
	c.Schedule.BlackcatEnabled = true
	c.Schedule.BalanceRefreshEnabled = true
	c.Schedule.BalanceRefreshMinutes = 5
	c.Schedule.CheckinAccountDelay = "800ms"
	c.Upstream.TimeoutSeconds = 120
	// HeaderTimeoutSeconds/IdleTimeoutSeconds 默认 0（未设置态），回落见 normalize()。
	c.Upstream.HeaderTimeoutSeconds = 0
	c.Upstream.IdleTimeoutSeconds = 0
	// Global.Enabled 缺省 true（纯 CN 行为不变：CN 账号恒判 cn，global base 不被使用）；
	// ChatBase/BillingBase 缺省空（回落内置默认）。
	c.Global.Enabled = true
	c.Features.SanitizeBlacklistFingerprints = true
	c.Features.LocalWebToolRounds = 3
	c.Prompt.Mode = "passthrough" // 缺省 passthrough：透传客户端原始 system（对齐上游；custom 由用户显式选择）
	c.Pool.MaxInFlight = 3
	// MaxInFlightGlobal 缺省 2：global 域 WAF 风控更紧，压低单号并发（WAF 403 修复
	// P1-1）；0/负数 normalize 回落默认（与 max_in_flight 的 0=不限语义不同，分档键
	// 的 0 没有合理语义，回退分档默认最稳）。
	c.Pool.MaxInFlightGlobal = 2
	c.Pool.BreakerThreshold = 3
	c.Pool.BreakerCooldown = "30m"
	c.Pool.BreakerCooldownMax = "6h"
	c.Pool.DegradeThreshold = 5
	c.Pool.DegradeCooldown = "10m"
	c.Pool.DegradeCooldownMax = "2h"
	c.Pool.IdleWeightPerHour = 0.5
	c.Pool.IdleWeightMax = 5.0
	c.Pool.PreferExpiring = true
	c.Pool.ExpiringSoon = "168h" // 快过期窗口默认 7 天：官方活动奖励积分多在两周内过期
	// costTier 探索默认 30m（issue #136：垄断破除 + 搭车改道零新增请求）；"0" 关停。
	c.Pool.CostExploreInterval = "30m"
	c.SessionSticky.Enabled = true
	c.SessionSticky.TTL = "30m"
	c.SessionSticky.GCInterval = "5m"
	return c
}

// Load 从文件读，再用 WB2A_* env 覆盖。
func Load(path string) (*Config, error) {
	c := Default()
	if path != "" {
		// 目录检查：Docker bind mount 在宿主机文件缺失时会静默创建同名目录，
		// 直接 ReadFile 会报 "Incorrect function" 之类晦涩错误，这里给出可操作提示。
		if st, statErr := os.Stat(path); statErr == nil && st.IsDir() {
			return nil, fmt.Errorf("config %s 是目录而非文件——"+
				"Docker 部署时若宿主机缺少 config.json，bind mount 会创建同名目录。"+
				"请先 `cp config.example.json config.json` 或删除该目录（程序会自动生成配置）", path)
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read config: %w", err)
		}
		if _, err := ParseConfigInto(raw, c); err != nil {
			return nil, err
		}
	}
	applyEnv(c)
	if err := c.normalize(); err != nil {
		return nil, err
	}
	return c, nil
}

// ParseConfigInto 把 JSON 覆盖到 c 上并 normalize（不做 env、不读文件）。
// 面板保存配置走这条路径：与 Load 完全同一套解析/校验逻辑，避免两处漂移。
func ParseConfigInto(raw []byte, c *Config) (*Config, error) {
	if err := json.Unmarshal(raw, c); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if err := c.normalize(); err != nil {
		return nil, err
	}
	return c, nil
}

// ParseConfig 基于默认值解析一段配置 JSON（等价于 Load 的文件分支，但不读环境变量）。
func ParseConfig(raw []byte) (*Config, error) {
	return ParseConfigInto(raw, Default())
}

// parseConfigWithEnv 解析配置后再叠加环境变量覆盖，供面板比较「当前生效值」。
// 与 Load 同序：JSON → env → normalize，避免 env 覆盖的字段被误报为已变化。
func parseConfigWithEnv(raw []byte) (*Config, error) {
	c := Default()
	if _, err := ParseConfigInto(raw, c); err != nil {
		return nil, err
	}
	applyEnv(c)
	if err := c.normalize(); err != nil {
		return nil, err
	}
	return c, nil
}

// WriteDefault 在 path 落一份推荐配置（首次运行自动生成，双击即开免手工复制样例）。
// 值取自 Default()（含超时/熔断/签到排程等推荐值），api_key 用 crypto/rand 随机生成：
// 安全默认优于示例占位符（listen 绑定 0.0.0.0，空 key 会把网关裸暴露给局域网）。
// 返回生成的 key 供启动日志透出。已存在时经 O_EXCL 原子拒绝，绝不改写用户配置。
func WriteDefault(path string) (string, error) {
	key, err := randomAPIKey()
	if err != nil {
		return "", err
	}
	c := Default()
	c.APIKey = key
	_ = c.normalize() // Default() 全合法，normalize 仅补齐 header/idle 超时的展示值
	out, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshal config: %w", err)
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", fmt.Errorf("mkdir config dir: %w", err)
		}
	}
	// O_EXCL 原子拒绝覆盖：即使调用方漏判"不存在"，也绝不悄悄改写用户已有配置。
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", fmt.Errorf("write config: %w", err)
	}
	defer f.Close()
	if _, err := f.Write(out); err != nil {
		return "", fmt.Errorf("write config: %w", err)
	}
	return key, nil
}

// randomAPIKey 生成 sk- 前缀的随机网关密钥。
func randomAPIKey() (string, error) {
	raw := make([]byte, 18)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("gen api_key: %w", err)
	}
	return "sk-" + base64.RawURLEncoding.EncodeToString(raw), nil
}

// ReplacePlaceholderAPIKey 把 path 中仍为公开占位值的 api_key 替换成随机密钥并
// 原子写回。只精确匹配 placeholderAPIKey：显式配置的其他值（含空串）一律不动，
// 避免把用户有意设置的密钥静默改掉。
//
// 返回新密钥与实际是否发生替换。文件不存在时返回 fs.ErrNotExist，由调用方决定
// 是否走首次运行生成路径；非普通文件（如 Docker bind mount 误建目录）不处理，
// 交由 Load 给出更具体的错误。
func ReplacePlaceholderAPIKey(path string) (string, bool, error) {
	doc, current, err := readConfigDoc(path)
	if err != nil || current != placeholderAPIKey {
		return "", false, err
	}
	key, err := randomAPIKey()
	if err != nil {
		return "", false, err
	}
	encoded, err := json.Marshal(key)
	if err != nil {
		return "", false, fmt.Errorf("marshal api_key: %w", err)
	}
	doc["api_key"] = encoded
	if err := writeConfigDoc(path, doc); err != nil {
		return "", false, err
	}
	return key, true, nil
}

// readConfigDoc 读取配置文件为可按顶层键修改的文档，并返回当前 api_key。
func readConfigDoc(path string) (map[string]json.RawMessage, string, error) {
	st, err := os.Stat(path)
	if err != nil {
		return nil, "", err
	}
	if !st.Mode().IsRegular() {
		return nil, "", nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, "", fmt.Errorf("read config: %w", err)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, "", fmt.Errorf("parse config: %w", err)
	}
	var current string
	if v, ok := doc["api_key"]; ok {
		if err := json.Unmarshal(v, &current); err != nil {
			return nil, "", fmt.Errorf("parse api_key: %w", err)
		}
	}
	return doc, current, nil
}

// writeConfigDoc 原子写回配置文档（tmp + rename），保留所有未修改的顶层键。
func writeConfigDoc(path string, doc map[string]json.RawMessage) error {
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, out, 0o600); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	return replaceConfigFile(tmp, path, out)
}

func applyEnv(c *Config) {
	applyEnvCore(c)
	applyEnvTimeouts(c)
	applyEnvIdentity(c)
	applyEnvFeatures(c)
}

// applyEnvCore 覆盖监听 / 鉴权 / 路径与冷却基数。
func applyEnvCore(c *Config) {
	if v := os.Getenv("WB2A_LISTEN"); v != "" {
		c.Listen = v
	}
	if v := os.Getenv("WB2A_API_KEY"); v != "" {
		c.APIKey = v
	}
	if v := os.Getenv("WB2A_AUTH_DIR"); v != "" {
		c.AuthDir = v
	}
	if v := os.Getenv("WB2A_STATE_FILE"); v != "" {
		c.StateFile = v
	}
	if v := os.Getenv("WB2A_SOFT_RATE"); v != "" {
		c.Cooldown.SoftRate = v
	}
	if v := os.Getenv("WB2A_SOFT_RATE_MAX"); v != "" {
		c.Cooldown.SoftRateMax = v
	}
}

// applyEnvTimeouts 覆盖出站 HTTP 三段超时（短 RPC / 首字节前 / 流中空闲）。
func applyEnvTimeouts(c *Config) {
	if n, ok := envInt("WB2A_TIMEOUT_SECONDS"); ok {
		c.Upstream.TimeoutSeconds = n
	}
	if n, ok := envInt("WB2A_HEADER_TIMEOUT_SECONDS"); ok {
		c.Upstream.HeaderTimeoutSeconds = n
	}
	if n, ok := envInt("WB2A_IDLE_TIMEOUT_SECONDS"); ok {
		c.Upstream.IdleTimeoutSeconds = n
	}
}

// applyEnvIdentity 覆盖出站 UA / 归属头 / 设备 token 与 IP 透传开关。
func applyEnvIdentity(c *Config) {
	if v := os.Getenv("WB2A_USER_AGENT"); v != "" {
		c.Upstream.UserAgent = v
	}
	if v := os.Getenv("WB2A_CLIENT_VERSION"); v != "" {
		c.Upstream.ClientVersion = v
	}
	if v := os.Getenv("WB2A_CLI_VERSION"); v != "" {
		c.Upstream.CliVersion = v
	}
	if v := os.Getenv("WB2A_CLIENT_NAME"); v != "" {
		c.Upstream.ClientName = v
	}
	if v := os.Getenv("WB2A_DEVICE_TOKEN"); v != "" {
		c.Upstream.DeviceToken = v
	}
	if v := os.Getenv("WB2A_DEVICE_TOKEN_FILE"); v != "" {
		c.Upstream.DeviceTokenFile = v
	}
	if b, ok := envBool("WB2A_PASSTHROUGH_IP"); ok {
		c.Upstream.PassthroughIP = b
	}
}

// applyEnvFeatures 覆盖脱敏开关、prompt 模式 / 文件与积分过期相关项。
func applyEnvFeatures(c *Config) {
	if b, ok := envBool("WB2A_SANITIZE_FINGERPRINTS"); ok {
		c.Features.SanitizeBlacklistFingerprints = b
	}
	if b, ok := envBool("WB2A_MULTIMODAL_ROUTING"); ok {
		c.Features.MultimodalRouting = b
	}
	if v := os.Getenv("WB2A_PROMPT_MODE"); v != "" {
		c.Prompt.Mode = v
	}
	if v := os.Getenv("WB2A_PROMPT_FILE"); v != "" {
		c.Prompt.File = v
	}
	if v := os.Getenv("WB2A_EXPIRING_SOON"); v != "" {
		c.Pool.ExpiringSoon = v
	}
	if b, ok := envBool("WB2A_PREFER_EXPIRING"); ok {
		c.Pool.PreferExpiring = b
	}
}

// envInt 读取整型环境变量：键缺席或解析失败一律视为未设置（ok=false），
// 与旧内联写法「仅 Atoi 成功才覆盖」语义一致。
func envInt(key string) (int, bool) {
	v := os.Getenv(key)
	if v == "" {
		return 0, false
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, false
	}
	return n, true
}

// envBool 读取布尔环境变量：键缺席或解析失败一律视为未设置（ok=false）。
func envBool(key string) (bool, bool) {
	v := os.Getenv(key)
	if v == "" {
		return false, false
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return false, false
	}
	return b, true
}

// normalize 归一化配置：补齐默认值、解析时长、校验排程。
// 顺序刻意保持「时长解析 → 静态默认值 → 排程校验 → prompt」：原实现里
// pool.degrade_cooldown(_max) 是「先解析、后空值回落」，显式空串会在解析阶段
// fail fast；若把静态默认值提前，空串会被先补成默认值而吞掉该错误。
func (c *Config) normalize() error {
	if err := c.normalizeDurations(); err != nil {
		return err
	}
	c.normalizeStaticDefaults()
	steps := []func() error{
		c.normalizeCORS, c.normalizeModelAliases, c.normalizeBackgroundModels,
		c.normalizeNotify, c.normalizeCheckinDelay, c.normalizeLogging,
		c.normalizeSchedule, c.normalizePrompt, c.normalizeProxies,
	}
	for _, step := range steps {
		if err := step(); err != nil {
			return err
		}
	}
	// 归一化完成后做显式校验：非法 / 自相矛盾的配置在写入前即被拒绝（不落盘）。
	return c.Validate()
}

// normalizeModelAliases 校验并归一化模型别名（issue #52）：键必须是裸名，
// 值必须是可解析的非空 "[realm:]model"；首尾空白去掉，去空白后重名直接报错
// （否则 map 覆盖顺序不确定，配置会静默丢一项）。
func (c *Config) normalizeModelAliases() error {
	if len(c.Models.Aliases) == 0 {
		return nil
	}
	out := make(map[string]string, len(c.Models.Aliases))
	for key, value := range c.Models.Aliases {
		name := strings.TrimSpace(key)
		if name == "" {
			return errors.New("models.aliases: 别名不能为空")
		}
		if strings.Contains(name, ":") {
			return fmt.Errorf("models.aliases[%q]: 别名不得含 realm 前缀，请写成裸模型名", name)
		}
		target, err := normalizeAliasTarget(name, value)
		if err != nil {
			return err
		}
		if _, dup := out[name]; dup {
			return fmt.Errorf("models.aliases: 别名 %q 去掉首尾空白后重复", name)
		}
		out[name] = target
	}
	c.Models.Aliases = out
	return nil
}

// normalizeBackgroundModels 规范化后台模型名单（issue #103）：去空白、转小写、
// 去重；目标缺省 deepseek-v4.1-flash。名单为空时仍保留目标，便于后续热开。
func (c *Config) normalizeBackgroundModels() error {
	if strings.TrimSpace(c.Models.BackgroundTarget) == "" {
		c.Models.BackgroundTarget = defaultBackgroundTarget
	}
	c.Models.BackgroundTarget = strings.TrimSpace(c.Models.BackgroundTarget)
	if len(c.Models.BackgroundNames) == 0 {
		c.Models.BackgroundNames = nil
		return nil
	}
	seen := make(map[string]bool, len(c.Models.BackgroundNames))
	out := make([]string, 0, len(c.Models.BackgroundNames))
	for _, raw := range c.Models.BackgroundNames {
		name := strings.ToLower(strings.TrimSpace(raw))
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	c.Models.BackgroundNames = out
	return nil
}

// normalizeAliasTarget 去掉目标首尾空白并校验 "[realm:]model"：冒号前只允许
// cn/global（未知前缀会与裸模型名混淆，必须在启动期拒绝），裸名部分必须非空。
func normalizeAliasTarget(name, value string) (string, error) {
	target := strings.TrimSpace(value)
	if target == "" {
		return "", fmt.Errorf("models.aliases[%q]: 目标模型不能为空", name)
	}
	model := target
	if i := strings.IndexByte(model, ':'); i >= 0 {
		prefix := model[:i]
		if prefix != "cn" && prefix != "global" {
			return "", fmt.Errorf("models.aliases[%q]: 目标模型 %q 的 realm 前缀必须是 cn 或 global", name, target)
		}
		model = model[i+1:]
	}
	if model == "" {
		return "", fmt.Errorf("models.aliases[%q]: 目标模型 %q 缺少裸模型名", name, target)
	}
	return target, nil
}

// normalizeNotify 校验 webhook 与事件名，并去掉事件首尾空白/重复项。
func (c *Config) normalizeNotify() error {
	webhook := strings.TrimSpace(c.Notify.WebhookURL)
	if err := notify.ValidateURL(webhook); err != nil {
		return fmt.Errorf("notify.webhook_url: %w", err)
	}
	c.Notify.WebhookURL = webhook
	if len(c.Notify.Events) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(c.Notify.Events))
	events := make([]string, 0, len(c.Notify.Events))
	for _, raw := range c.Notify.Events {
		name := strings.TrimSpace(raw)
		if !notify.ValidEvent(name) {
			return fmt.Errorf("notify.events: 未知事件 %q", raw)
		}
		if seen[name] {
			continue
		}
		seen[name] = true
		events = append(events, name)
	}
	c.Notify.Events = events
	return nil
}

// normalizeLogging 校验决策链采样率（issue #63）：0/1 = 全部，0<v<1 采样，>1 报错。
func (c *Config) normalizeLogging() error {
	if c.Logging.DecisionTraceSample < 0 || c.Logging.DecisionTraceSample > 1 {
		return fmt.Errorf("logging.decision_trace_sample: 必须在 [0,1]，当前 %v", c.Logging.DecisionTraceSample)
	}
	return nil
}

// normalizeProxies 校验出站代理 URL（issue #15）：任一非法即拒绝（避免运行时才暴露）。
func (c *Config) normalizeProxies() error {
	for _, raw := range c.Upstream.Proxies {
		if _, err := outbound.ParseProxy(raw); err != nil {
			return fmt.Errorf("upstream.proxies: %w", err)
		}
	}
	seen := make(map[string]bool, len(c.Upstream.ProxySlots))
	for i := range c.Upstream.ProxySlots {
		slot := &c.Upstream.ProxySlots[i]
		slot.ID = strings.TrimSpace(slot.ID)
		slot.Name = strings.TrimSpace(slot.Name)
		if slot.ID == "" {
			return fmt.Errorf("upstream.proxy_slots[%d]: id 不能为空", i)
		}
		if seen[slot.ID] {
			return fmt.Errorf("upstream.proxy_slots: id %q 重复", slot.ID)
		}
		seen[slot.ID] = true
		if _, err := outbound.ParseProxy(slot.URL); err != nil {
			return fmt.Errorf("upstream.proxy_slots[%s]: %w", slot.ID, err)
		}
	}
	return nil
}

// Validate 对归一化后的配置做字段范围与跨字段一致性校验（Provision → Validate 的
// 第二段）。由 normalize() 末尾调用，因此覆盖启动加载（含 env 覆盖）与面板保存两条
// 路径：非法配置在写入前返回可读错误、不落盘。
//
// 只拒绝语义上非法的组合，不改变合法配置的行为；normalize 里既有的「空 / 0 回落默认」
// 语义保持不变（那些是缺省，不是错误）。
func (c *Config) Validate() error {
	if err := c.validateBackoffOrder(); err != nil {
		return err
	}
	if err := c.validateRanges(); err != nil {
		return err
	}
	return c.validateGlobalMutex()
}

// validateBackoffOrder 跨字段：指数退避封顶不得小于基数（否则封顶形同虚设）。
func (c *Config) validateBackoffOrder() error {
	if c.SoftRateDur > 0 && c.SoftRateMaxDur > 0 && c.SoftRateMaxDur < c.SoftRateDur {
		return fmt.Errorf("cooldown.soft_rate_max (%s) 不得小于 cooldown.soft_rate (%s)",
			c.Cooldown.SoftRateMax, c.Cooldown.SoftRate)
	}
	if c.BreakerCooldownDur > 0 && c.BreakerCooldownMaxD > 0 && c.BreakerCooldownMaxD < c.BreakerCooldownDur {
		return fmt.Errorf("pool.breaker_cooldown_max (%s) 不得小于 pool.breaker_cooldown (%s)",
			c.Pool.BreakerCooldownMax, c.Pool.BreakerCooldown)
	}
	if c.DegradeCooldownDur > 0 && c.DegradeCooldownMaxD > 0 && c.DegradeCooldownMaxD < c.DegradeCooldownDur {
		return fmt.Errorf("pool.degrade_cooldown_max (%s) 不得小于 pool.degrade_cooldown (%s)",
			c.Pool.DegradeCooldownMax, c.Pool.DegradeCooldown)
	}
	return nil
}

// validateRanges 字段范围：0 有明确语义（不限 / 关闭）的键不接受负值。
func (c *Config) validateRanges() error {
	if err := validateLimitRule("limits", LimitRule{
		DailyTokens: c.Limits.DailyTokens, DailyCredits: c.Limits.DailyCredits, ModelDailyTokens: c.Limits.ModelDailyTokens,
	}); err != nil {
		return err
	}
	if err := validateLimitRule("limits.cn", c.Limits.CN); err != nil {
		return err
	}
	if err := validateLimitRule("limits.global", c.Limits.Global); err != nil {
		return err
	}
	if c.Pool.MaxInFlight < 0 {
		return fmt.Errorf("pool.max_in_flight: 负值 %d 无意义（0 = 不限）", c.Pool.MaxInFlight)
	}
	if c.Server.MaxBodyBytes < 0 {
		return fmt.Errorf("server.max_body_bytes: 负值 %d 无意义（0 = 不限制）", c.Server.MaxBodyBytes)
	}
	if c.Pool.MaxInFlightTotal < 0 {
		return fmt.Errorf("pool.max_in_flight_total: 负值 %d 无意义（0 = 不限）", c.Pool.MaxInFlightTotal)
	}
	if c.SessionTTL < 0 {
		return fmt.Errorf("session_sticky.ttl: 负时长 %q 无意义", c.SessionSticky.TTL)
	}
	if c.SessionGCInterval <= 0 {
		return fmt.Errorf("session_sticky.gc_interval: 必须为正时长，当前 %q", c.SessionSticky.GCInterval)
	}
	return nil
}

// validateLimitRule 校验一组每日限额非负（0 = 关闭）。
func validateLimitRule(name string, r LimitRule) error {
	if r.DailyTokens < 0 || r.DailyCredits < 0 || r.ModelDailyTokens < 0 {
		return fmt.Errorf("%s: 每日限额不能为负（0 = 关闭）", name)
	}
	return nil
}

// validateGlobalMutex 互斥：global 路由关闭时再配 global base 自相矛盾（base 永不会被使用）。
func (c *Config) validateGlobalMutex() error {
	if !c.Global.Enabled && (strings.TrimSpace(c.Global.ChatBase) != "" || strings.TrimSpace(c.Global.BillingBase) != "") {
		return errors.New("global.enabled=false 与 global.chat_base / global.billing_base 同时设置自相矛盾：请清空 base 或开启 global")
	}
	return nil
}

// normalizeStaticDefaults 补齐面板 / 日志 / 池 / 上游的数值型缺省值。
// 这些字段互不依赖、也不参与时长解析，合并处理不改变可观测行为。
func (c *Config) normalizeStaticDefaults() {
	if c.Panel.PackageDetailLimit <= 0 {
		c.Panel.PackageDetailLimit = 5
	}
	if c.Logging.RequestRetentionDays <= 0 {
		c.Logging.RequestRetentionDays = 7
	}
	if c.Logging.RequestArchiveMaxMB <= 0 {
		c.Logging.RequestArchiveMaxMB = 100
	}
	c.normalizeLocalWebToolRounds()
	c.normalizePoolDefaults()
	c.normalizeUpstreamDefaults()
}

// normalizeCORS 规范化并校验 server.cors_origins：只接受 scheme://host[:port]，
// 去重并忽略大小写；通配、路径、查询、凭证一律拒绝（避免配置出静默不生效的源）。
func (c *Config) normalizeCORS() error {
	if len(c.Server.CORSOrigins) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(c.Server.CORSOrigins))
	out := make([]string, 0, len(c.Server.CORSOrigins))
	for _, raw := range c.Server.CORSOrigins {
		norm, err := normalizeCORSOrigin(raw)
		if err != nil {
			return err
		}
		if seen[norm] {
			continue
		}
		seen[norm] = true
		out = append(out, norm)
	}
	c.Server.CORSOrigins = out
	return nil
}

// normalizeCORSOrigin 把单个 Origin 规范化为小写 scheme://host[:port]；非法即报错。
func normalizeCORSOrigin(raw string) (string, error) {
	origin := strings.TrimSpace(raw)
	if origin == "*" {
		return "", errors.New(`server.cors_origins: 不支持通配 "*"，请逐项列出允许的 Origin`)
	}
	u, err := url.Parse(origin)
	if err != nil {
		return "", fmt.Errorf("server.cors_origins: 非法 Origin %q: %w", raw, err)
	}
	if reason := invalidOriginReason(u); reason != "" {
		return "", fmt.Errorf("server.cors_origins: 非法 Origin %q（%s）", raw, reason)
	}
	host := strings.ToLower(u.Host)
	switch u.Scheme {
	case "http":
		host = strings.TrimSuffix(host, ":80")
	case "https":
		host = strings.TrimSuffix(host, ":443")
	}
	return u.Scheme + "://" + host, nil
}

// invalidOriginReason 返回 Origin URL 不合规的原因；合规时返回空串。
func invalidOriginReason(u *url.URL) string {
	switch {
	case u.Scheme != "http" && u.Scheme != "https":
		return "scheme 必须是 http 或 https"
	case u.Host == "":
		return "缺少 host"
	case u.User != nil:
		return "不能包含用户凭证"
	case u.RawQuery != "":
		return "不能包含查询参数"
	case u.Fragment != "":
		return "不能包含 fragment"
	case u.Path != "" && u.Path != "/":
		return "不能包含路径"
	default:
		return ""
	}
}

// normalizeLocalWebToolRounds 规范化本地上网工具轮数（issue #104）：<=0 回落 3，
// >8 钳到 8（每轮都消耗一次上游额度，必须硬上限）。
func (c *Config) normalizeLocalWebToolRounds() {
	if c.Features.LocalWebToolRounds <= 0 {
		c.Features.LocalWebToolRounds = 3
	}
	if c.Features.LocalWebToolRounds > 8 {
		c.Features.LocalWebToolRounds = 8
	}
}

// normalizePoolDefaults 补齐池相关的数值型缺省值（阈值 / 权重 / 上限）。
func (c *Config) normalizePoolDefaults() {
	// 积分保底：负值钳 0（= 关闭）。0 是合法默认（关闭），无需空值回落。
	if c.Pool.CreditFloor < 0 {
		c.Pool.CreditFloor = 0
	}
	if c.Pool.BreakerThreshold <= 0 {
		c.Pool.BreakerThreshold = 3
	}
	// 连败降权参数缺省归一（非法/未设置回落默认，与 breaker_threshold 同风格）。
	if c.Pool.DegradeThreshold <= 0 {
		c.Pool.DegradeThreshold = 5
	}
	if c.Pool.DegradeCooldown == "" {
		c.Pool.DegradeCooldown = "10m"
	}
	if c.Pool.DegradeCooldownMax == "" {
		c.Pool.DegradeCooldownMax = "2h"
	}
	// global 在途分档：0/负数视为未设置回落默认 2（WAF 403 修复 P1-1）。
	if c.Pool.MaxInFlightGlobal <= 0 {
		c.Pool.MaxInFlightGlobal = 2
	}
	if c.Pool.IdleWeightPerHour <= 0 {
		c.Pool.IdleWeightPerHour = 0.5
	}
	if c.Pool.IdleWeightMax <= 0 {
		c.Pool.IdleWeightMax = 5.0
	}
}

// normalizeUpstreamDefaults 补齐出站超时缺省值（header 回落 timeout、idle 走内置大值）。
func (c *Config) normalizeUpstreamDefaults() {
	if c.Upstream.TimeoutSeconds <= 0 {
		c.Upstream.TimeoutSeconds = 120
	}
	// header 缺省回落 timeout（保"首字节前换号"既有语义）；idle 缺省走内置大值。
	// 任务书约定：0 一律视为"未设置"走默认，真正的"禁用"留待后续（避免歧义）。
	if c.Upstream.HeaderTimeoutSeconds <= 0 {
		c.Upstream.HeaderTimeoutSeconds = c.Upstream.TimeoutSeconds
	}
	if c.Upstream.IdleTimeoutSeconds <= 0 {
		c.Upstream.IdleTimeoutSeconds = 300
	}
	if c.Upstream.RotateMax <= 0 {
		c.Upstream.RotateMax = 3
	}
	if c.Upstream.RotateBudget == "" {
		c.Upstream.RotateBudget = "0"
	}
	if c.Upstream.KeepaliveInterval == "" {
		c.Upstream.KeepaliveInterval = "0"
	}
}

// normalizeDurations 把字符串时长解析到对应的 *Dur 字段，并按需回落默认值。
// 子步骤按原实现顺序串联，任一项非法即短路返回（保持错误信息与优先级不变）。
func (c *Config) normalizeDurations() error {
	if err := c.normalizeServerDuration(); err != nil {
		return err
	}
	if err := c.normalizeCooldownDurations(); err != nil {
		return err
	}
	if err := c.normalizePoolDurations(); err != nil {
		return err
	}
	if err := c.normalizeSessionDurations(); err != nil {
		return err
	}
	if err := c.normalizeUpstreamRotate(); err != nil {
		return err
	}
	if err := c.normalizeUpstreamKeepalive(); err != nil {
		return err
	}
	if err := c.normalizeModelTimeouts(); err != nil {
		return err
	}
	return c.normalizeExpiringDurations()
}

// normalizeModelTimeouts 校验并解析按模型超时（issue #58）：键去空白后不得为空；
// header/idle 非空时必须是合法非负时长，至少配置一项。
func (c *Config) normalizeModelTimeouts() error {
	if len(c.Upstream.ModelTimeouts) == 0 {
		return nil
	}
	out := make(map[string]ModelTimeout, len(c.Upstream.ModelTimeouts))
	for key, spec := range c.Upstream.ModelTimeouts {
		name := strings.TrimSpace(key)
		if name == "" {
			return errors.New("upstream.model_timeouts: 模型名不能为空")
		}
		parsed, err := parseModelTimeout(name, spec)
		if err != nil {
			return err
		}
		if _, dup := out[name]; dup {
			return fmt.Errorf("upstream.model_timeouts: 模型 %q 去空白后重复", name)
		}
		out[name] = parsed
	}
	c.Upstream.ModelTimeouts = out
	return nil
}

// parseModelTimeout 解析单个模型的 header/idle 覆盖。
func parseModelTimeout(name string, spec ModelTimeout) (ModelTimeout, error) {
	header, err := parseOptionalDuration("upstream.model_timeouts["+name+"].header", spec.Header)
	if err != nil {
		return ModelTimeout{}, err
	}
	idle, err := parseOptionalDuration("upstream.model_timeouts["+name+"].idle", spec.Idle)
	if err != nil {
		return ModelTimeout{}, err
	}
	if header == 0 && idle == 0 {
		return ModelTimeout{}, fmt.Errorf("upstream.model_timeouts[%q]: header/idle 至少配置一项", name)
	}
	spec.Header = strings.TrimSpace(spec.Header)
	spec.Idle = strings.TrimSpace(spec.Idle)
	spec.HeaderDur = header
	spec.IdleDur = idle
	return spec, nil
}

// parseOptionalDuration 解析可空时长；空 = 0，负值报错。
func parseOptionalDuration(field, raw string) (time.Duration, error) {
	if strings.TrimSpace(raw) == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(strings.TrimSpace(raw))
	if err != nil {
		return 0, fmt.Errorf("%s: %w", field, err)
	}
	if d < 0 {
		return 0, fmt.Errorf("%s: 负时长 %q 无意义", field, raw)
	}
	return d, nil
}

// normalizeUpstreamRotate 解析轮换总时长预算（issue #17）：空 = "0"（不限）；
// 负时长无意义 fail fast。
func (c *Config) normalizeUpstreamRotate() error {
	budget := c.Upstream.RotateBudget
	if budget == "" {
		budget = "0"
	}
	d, err := time.ParseDuration(budget)
	if err != nil {
		return fmt.Errorf("upstream.rotate_budget: %w", err)
	}
	if d < 0 {
		return fmt.Errorf("upstream.rotate_budget: 负时长 %q 无意义", c.Upstream.RotateBudget)
	}
	c.RotateBudgetDur = d
	return nil
}

// normalizeUpstreamKeepalive 解析入站 SSE 心跳间隔（issue #54）：空 = "0"（关闭）；
// 负时长无意义 fail fast。
func (c *Config) normalizeUpstreamKeepalive() error {
	raw := c.Upstream.KeepaliveInterval
	if raw == "" {
		raw = "0"
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return fmt.Errorf("upstream.keepalive_interval: %w", err)
	}
	if d < 0 {
		return fmt.Errorf("upstream.keepalive_interval: 负时长 %q 无意义", c.Upstream.KeepaliveInterval)
	}
	c.KeepaliveIntervalDur = d
	return nil
}

// normalizeServerDuration 解析入站读取上限。
func (c *Config) normalizeServerDuration() error {
	// 入站读取上限（issue #100）：空值回落默认 300s；"0" 合法（不限制）；
	// 负值无语义，fail fast（静默钳 0 会把保护悄悄关掉）。
	if c.Server.ReadTimeout == "" {
		c.Server.ReadTimeout = "300s"
	}
	var err error
	if c.ServerReadTimeoutDur, err = time.ParseDuration(c.Server.ReadTimeout); err != nil {
		return fmt.Errorf("server.read_timeout: %w", err)
	}
	if c.ServerReadTimeoutDur < 0 {
		return fmt.Errorf("server.read_timeout: 负时长 %q 无意义", c.Server.ReadTimeout)
	}
	return nil
}

// normalizeCooldownDurations 解析软冷却基数与封顶。
func (c *Config) normalizeCooldownDurations() error {
	var err error
	if c.SoftRateDur, err = time.ParseDuration(c.Cooldown.SoftRate); err != nil {
		return fmt.Errorf("cooldown.soft_rate: %w", err)
	}
	// 空值回落默认 2h（Default() 已置值；此兜底覆盖显式 "" 与 Default() 被绕过的场景）。
	if c.Cooldown.SoftRateMax == "" {
		c.Cooldown.SoftRateMax = "2h"
	}
	if c.SoftRateMaxDur, err = time.ParseDuration(c.Cooldown.SoftRateMax); err != nil {
		return fmt.Errorf("cooldown.soft_rate_max: %w", err)
	}
	return nil
}

// normalizePoolDurations 解析熔断与连败降权的时长参数。
func (c *Config) normalizePoolDurations() error {
	var err error
	if c.BreakerCooldownDur, err = time.ParseDuration(c.Pool.BreakerCooldown); err != nil {
		return fmt.Errorf("pool.breaker_cooldown: %w", err)
	}
	if c.BreakerCooldownMaxD, err = time.ParseDuration(c.Pool.BreakerCooldownMax); err != nil {
		return fmt.Errorf("pool.breaker_cooldown_max: %w", err)
	}
	if c.DegradeCooldownDur, err = time.ParseDuration(c.Pool.DegradeCooldown); err != nil {
		return fmt.Errorf("pool.degrade_cooldown: %w", err)
	}
	if c.DegradeCooldownMaxD, err = time.ParseDuration(c.Pool.DegradeCooldownMax); err != nil {
		return fmt.Errorf("pool.degrade_cooldown_max: %w", err)
	}
	return nil
}

// normalizeSessionDurations 解析会话粘性 TTL 与 GC 周期。
func (c *Config) normalizeSessionDurations() error {
	var err error
	if c.SessionTTL, err = time.ParseDuration(c.SessionSticky.TTL); err != nil {
		return fmt.Errorf("session_sticky.ttl: %w", err)
	}
	if c.SessionGCInterval, err = time.ParseDuration(c.SessionSticky.GCInterval); err != nil {
		return fmt.Errorf("session_sticky.gc_interval: %w", err)
	}
	return nil
}

// normalizeExpiringDurations 解析快过期窗口与 costTier 探索窗口。
func (c *Config) normalizeExpiringDurations() error {
	var err error
	// 快过期窗口：空 = 禁用（ExpiringSoonDur 0）；非空必须可解析（拼写错误 fail fast）。
	if c.Pool.ExpiringSoon != "" {
		if c.ExpiringSoonDur, err = time.ParseDuration(c.Pool.ExpiringSoon); err != nil {
			return fmt.Errorf("pool.expiring_soon: %w", err)
		}
	}
	if c.ExpiringSoonDur < 0 {
		c.ExpiringSoonDur = 0
		c.Pool.ExpiringSoon = "0"
	}
	// costTier 探索窗口（issue #136）：空值回落默认 30m（Default 已置；此兜底覆盖
	// 显式 ""）；"0" 是合法值（关停，完全回到现状行为），不回落；负值钳 0 同关停
	// （"−5m" 无合理语义）。
	if c.Pool.CostExploreInterval == "" {
		c.Pool.CostExploreInterval = "30m"
	}
	if c.CostExploreIntervalDur, err = time.ParseDuration(c.Pool.CostExploreInterval); err != nil {
		return fmt.Errorf("pool.cost_explore_interval: %w", err)
	}
	if c.CostExploreIntervalDur < 0 {
		c.CostExploreIntervalDur = 0
	}
	return nil
}

// normalizeCheckinDelay 解析签到账号间等待（issue #59）：空回落 800ms，
// "0" = 不限，负时长报错。
func (c *Config) normalizeCheckinDelay() error {
	raw := strings.TrimSpace(c.Schedule.CheckinAccountDelay)
	if raw == "" {
		raw = "800ms"
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return fmt.Errorf("schedule.checkin_account_delay: %w", err)
	}
	if d < 0 {
		return fmt.Errorf("schedule.checkin_account_delay: 负时长 %q 无意义", c.Schedule.CheckinAccountDelay)
	}
	c.Schedule.CheckinAccountDelay = raw
	c.CheckinAccountDelayDur = d
	return nil
}

// normalizeSchedule 归一化监听地址与排程：小时数组空值回落默认、余额刷新周期，
// 最后校验小时合法区间（0-23）。
func (c *Config) normalizeSchedule() error {
	c.normalizeListenAndHours()
	c.normalizeBalanceRefresh()
	return c.validateScheduleHours()
}

// normalizeListenAndHours 补齐 listen 前缀与六个排程小时数组的缺省值。
func (c *Config) normalizeListenAndHours() {
	if !strings.HasPrefix(c.Listen, ":") && !strings.Contains(c.Listen, ":") {
		c.Listen = ":" + c.Listen
	}
	// 空数组与 null 反序列化后覆盖掉 Default() 的排程值（键缺席才保留），在此补齐。
	// 空 = 未配置 → 回落默认；「禁用」一律走 *_enabled=false，两者互不混淆。
	if len(c.Schedule.CheckinHours) == 0 {
		c.Schedule.CheckinHours = []int{9, 21}
	}
	if len(c.Schedule.TravelHours) == 0 {
		c.Schedule.TravelHours = []int{9, 21}
	}
	if len(c.Schedule.ActivityHours) == 0 {
		c.Schedule.ActivityHours = []int{10}
	}
	if len(c.Schedule.KeepaliveHours) == 0 {
		c.Schedule.KeepaliveHours = []int{22}
	}
	if len(c.Schedule.BlackcatHours) == 0 {
		c.Schedule.BlackcatHours = []int{23}
	}
	if len(c.Schedule.GrowthHours) == 0 {
		c.Schedule.GrowthHours = []int{1}
	}
}

// normalizeBalanceRefresh 计算余额后台刷新周期：启用时 minutes<=0 回落默认 5；
// 关闭时 interval 保持 0（不启动）。
func (c *Config) normalizeBalanceRefresh() {
	if c.Schedule.BalanceRefreshEnabled {
		if c.Schedule.BalanceRefreshMinutes <= 0 {
			c.Schedule.BalanceRefreshMinutes = 5
		}
		c.BalanceRefreshInterval = time.Duration(c.Schedule.BalanceRefreshMinutes) * time.Minute
	}
}

// normalizePrompt 校验 prompt.mode 并按 file 加载提示词文本（custom/append 模式）。
//
// mode 非法（非 passthrough/custom/append）启动报错，避免静默回落到某一分支；
// custom/append 模式下 file 非空但不可读 → 报错（fail fast），file 空 → 用内置默认
// （两模式共用同一加载路径，PromptText 均非空）。
// passthrough 模式不加载文本（透传客户端原始 system，文本在降级时用 prompt.Degraded）。
func (c *Config) normalizePrompt() error {
	switch m := strings.ToLower(strings.TrimSpace(c.Prompt.Mode)); m {
	case "", "passthrough":
		c.Prompt.Mode = "passthrough"
	case "custom":
		c.Prompt.Mode = "custom"
	case "append":
		c.Prompt.Mode = "append"
	default:
		return fmt.Errorf("prompt.mode: %q 不是合法值（passthrough / custom / append）", c.Prompt.Mode)
	}
	if c.Prompt.Mode == "custom" || c.Prompt.Mode == "append" {
		text, err := prompt.Load(c.Prompt.Mode, c.Prompt.File)
		if err != nil {
			return err
		}
		c.PromptText = text
	}
	return nil
}

// validateScheduleHours 校验排程小时落在 0-23。
//
// 为什么不用 `[-1]` 之类的哨兵值表意"禁用"：非法小时被静默吞掉时，用户以为关掉了签到，
// 实际可能被当成另一个整点照常执行；这里直接快速失败，并在错误信息里指向正确的开关
// （checkin_enabled / keepalive_enabled），避免用户靠猜哨兵值来配。
func (c *Config) validateScheduleHours() error {
	if err := checkHourRange("schedule.checkin_hours", "checkin_enabled", c.Schedule.CheckinHours); err != nil {
		return err
	}
	if err := checkHourRange("schedule.travel_hours", "travel_enabled", c.Schedule.TravelHours); err != nil {
		return err
	}
	if err := checkHourRange("schedule.activity_hours", "activity_enabled", c.Schedule.ActivityHours); err != nil {
		return err
	}
	if err := checkHourRange("schedule.keepalive_hours", "keepalive_enabled", c.Schedule.KeepaliveHours); err != nil {
		return err
	}
	if err := checkHourRange("schedule.blackcat_hours", "blackcat_enabled", c.Schedule.BlackcatHours); err != nil {
		return err
	}
	return checkHourRange("schedule.growth_hours", "growth_enabled", c.Schedule.GrowthHours)
}

func checkHourRange(field, switchKey string, hours []int) error {
	for _, h := range hours {
		if h < 0 || h > 23 {
			return fmt.Errorf("%s: %d 不是合法小时（0-23）；如要关闭该任务请设 schedule.%s=false", field, h, switchKey)
		}
	}
	return nil
}
