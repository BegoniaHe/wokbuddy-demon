// Package panel 内嵌式 Web 管理面板：账号池总览、单号运维（解冻/禁用/签到/
// 刷新余额/移除）、浏览器内 OAuth 添加账号（免重启热加载进池）、手动批量
// 签到/保活，以及运行日志环形缓冲（镜像 log 包与 chat 表格日志）。
//
// 设计约束：
//   - 前端为 web/ 工程（Rsbuild + React）的构建产物，输出到 dist/ 后 go:embed 打进
//     二进制，与二进制同体部署；
//   - 管理面鉴权走 httpauth.AuthenticateManagement（Bearer）；admin_api_key /
//     readonly_api_key 任一非空时默认 api_key 不再有管理权限，两者都为空时回落
//     api_key。面板 HTML 本身无秘密，可匿名加载，密钥只发给 /panel/api/*；
//   - 不改写既有池语义：所有运维操作落到 pool 已有入口（Revive/Disable/Remove...），
//     添加账号走 auth.SaveAtomic + pool.Add，重启后与 auths/ 目录天然对齐。
package panel

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/BegoniaHe/wokbuddy-demon/internal/agentcfg"
	"github.com/BegoniaHe/wokbuddy-demon/internal/apikey"
	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
	"github.com/BegoniaHe/wokbuddy-demon/internal/credithist"
	"github.com/BegoniaHe/wokbuddy-demon/internal/httpauth"
	"github.com/BegoniaHe/wokbuddy-demon/internal/livecfg"
	"github.com/BegoniaHe/wokbuddy-demon/internal/pool"
	"github.com/BegoniaHe/wokbuddy-demon/internal/pricing"
	"github.com/BegoniaHe/wokbuddy-demon/internal/quota"
	"github.com/BegoniaHe/wokbuddy-demon/internal/reqlog"
	"github.com/BegoniaHe/wokbuddy-demon/internal/scheduler"
	"github.com/BegoniaHe/wokbuddy-demon/internal/upstream"
	"github.com/BegoniaHe/wokbuddy-demon/internal/usage"
)

// Config 面板依赖（main 装配注入）。
type Config struct {
	Pool      *pool.Pool
	Upstream  *upstream.Client
	Scheduler *scheduler.Scheduler // 手动触发签到/保活；nil 时对应接口返回 501
	AuthDir   string               // OAuth 登录完成后凭证落盘目录
	AuditDir  string               // 管理面写操作审计目录（空 = 关闭审计）
	APIKey    string               // 数据面默认密钥；管理面回落用（与 Live 同时给出时 Live 优先）
	// AdminAPIKey / ReadonlyAPIKey 管理面凭据。任一非空时默认 api_key 不再有
	// 管理权限；两者都为空时 api_key 兼管理面（单密钥默认）。
	AdminAPIKey    string
	ReadonlyAPIKey string
	// AuthLimiter 鉴权失败限速器；与主服务共享同一实例（nil 时面板自建）。
	AuthLimiter *httpauth.Limiter
	// TrustedProxies 可信反向代理网段；只有 TCP 对端命中时才采信转发头。
	// 空 = 只认 RemoteAddr（直连部署的安全默认）。
	TrustedProxies []*net.IPNet
	RedisMode      string // "upstash" / "noop"，仅观测透出
	Version        string // 面板版本号（展示用）

	// Live 运行期可变配置（在线改配置立即生效）。
	Live *livecfg.Holder

	// ConfigPath config.json 路径与加载器（配置页读写用）。
	// LoadConfig 返回解析后的配置对象（前端展示/校验用，具体类型由 main 注入的闭包决定）；
	// nil 时配置页返回 501。
	ConfigPath string
	// ProxySlots 可供账号绑定的出站代理槽（issue #96）。provider 形态（issue #39）：
	// 槽配置热生效，闭包每次读当前生效配置，不捕获装配期快照；nil 安全（= 无槽）。
	ProxySlots func() []ProxySlotInfo
	// Pricing 等价价格缓存（issue #99）；nil = 关闭或未初始化。
	Pricing     *pricing.Catalog
	PricingRate float64
	// Quota 撞线事件与预算估算（issue #101）；nil = 未启用。
	Quota *quota.Store
	// Agents 本机智能体配置管理器（issue #102）；nil = 不可用。
	Agents     *agentcfg.Manager
	LoadConfig func() (any, error)
	// SaveConfig 校验并落盘配置，返回需要重启才能生效的字段列表；随后由 main 注入的
	// ApplyConfig 闭包完成热生效（池参数/排程/密钥/脱敏）。error 时配置不写盘。
	SaveConfig func(raw []byte) (restartRequired []string, err error)
	// RestoreConfig 用单代备份覆盖当前配置，返回仍需重启的字段；nil 时接口返回 501。
	RestoreConfig func() (restartRequired []string, err error)

	// StickyCount 返回粘性会话绑定数；nil 时报告 0。
	StickyCount func() int

	// CreditHistory 本地积分变动账本（nil = 积分历史接口返回 501）。
	CreditHistory *credithist.Ledger
	// Usage 逐请求用量记录器（nil = 用量接口返回 501）。
	Usage *usage.Recorder
	// RequestLog 请求指标与归档（nil = 对应接口返回 501）。
	RequestLog *reqlog.Recorder
	// Keys 命名密钥存储（nil = 密钥接口返回 501）。仅面板管理；面板自身鉴权
	// 仍只认默认 api_key（命名密钥是给网关客户端的，不应获得管理权限）。
	Keys *apikey.Store

	// ProbeFile 模型输出上限探测结果文件（scripts/probe_max_tokens.py --panel-out
	// 写入；空或文件不存在 = model_probes 端点返回空集，面板不显示任何实测标注）。
	// 只读展示：网关不解析、不依赖其内容做任何路由/出站决策。
	ProbeFile string
	// ProbeStateFile 面板主动探测（TTFT/成功失败历史）的状态文件（issue #110）。
	// 与 ProbeFile 分离：不覆盖脚本生成的实测上限文件。空 = 探测可执行但不落盘。
	ProbeStateFile string
}

// ProxySlotInfo 账号可绑定的代理槽展示信息。
type ProxySlotInfo struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Panel 管理面板 handler。挂载方式：外层 mux Handle("/panel/", panel)，
// 本 mux 的 pattern 均带 /panel 前缀（外层不做前缀剥离）。
type Panel struct {
	cfg          Config
	mux          *http.ServeMux
	started      time.Time
	logs         *Ring
	probeTimeout time.Duration

	// logins 进行中的 OAuth 设备授权会话（state → 会话信息）。
	// poll 成功或超时（loginTTL）后剔除；面板常驻进程，容量天然有界。
	loginMu sync.Mutex
	logins  map[string]loginSession

	// taskMu/taskLocks 一键完成任务的 per-account 互斥：同一账号的任务动作
	// （单任务 / 全量）同时只允许一条在跑。重复点击直接返回 409"仍在执行"，
	// 而不是并发跑两遍浪费上游请求（动作虽幂等，expert 系每遍含 8 次真实对话）。
	// 不同账号之间不互斥（并行照旧）。TryLock 语义，锁条目常驻（账号数有界）。
	taskMu    sync.Mutex
	taskLocks map[string]*sync.Mutex

	// 任务中心执行队列（taskcenter.go）。
	queueOnce sync.Once
	q         *queueState

	// authLimiter 鉴权失败按来源 IP 限速（issue #61）。
	authLimiter *httpauth.Limiter

	// audit 管理面写操作审计（JSONL，按大小轮转）。nil = 未配置目录。
	audit *auditLog

	// 模型探测（modelhealth.go，issue #110）：单飞 + 全局并发名额 + 状态文件锁。
	probeMu       sync.Mutex
	probeInflight map[string]bool
	probeSem      chan struct{}
	probeStateMu  sync.Mutex
}

// tryLockAccount 尝试锁定账号的任务执行；已在执行返回 false。
func (p *Panel) tryLockAccount(uid string) bool {
	p.taskMu.Lock()
	if p.taskLocks == nil {
		p.taskLocks = make(map[string]*sync.Mutex)
	}

	mu := p.taskLocks[uid]
	if mu == nil {
		mu = &sync.Mutex{}
		p.taskLocks[uid] = mu
	}
	p.taskMu.Unlock()

	return mu.TryLock()
}

// unlockAccount 释放账号任务锁（与 tryLockAccount 配对）。
func (p *Panel) unlockAccount(uid string) {
	p.taskMu.Lock()
	mu := p.taskLocks[uid]
	p.taskMu.Unlock()

	if mu != nil {
		mu.Unlock()
	}
}

// loginTTL 授权 URL 的最长有效期：超时的 state 直接回收，
// 防止"开了添加账号弹窗就走开"的会话永久滞留。
const loginTTL = 15 * time.Minute

// loginSession 进行中的 OAuth 会话：创建时刻 + realm（cn/global，用于落盘与端点切换）。
type loginSession struct {
	created time.Time
	realm   string // "cn" / "global"，缺省 cn
	base    string // 发起登录命中的 origin（CN 回落时 poll 优先同源）

	// pending 日志去重：同一 state 的上游 code/msg 未变化时不重复打印。
	pendingLogged bool
	pendingCode   int
	pendingMsg    string
}

// New 构建面板。
func New(cfg *Config) *Panel {
	config := *cfg
	if config.RedisMode == "" {
		config.RedisMode = "noop"
	}

	panel := &Panel{
		cfg:          config,
		mux:          http.NewServeMux(),
		started:      time.Now(),
		logs:         NewRing(500),
		probeTimeout: modelProbeTimeout,
		logins:       map[string]loginSession{},
		authLimiter:  config.AuthLimiter,
	}
	if panel.authLimiter == nil {
		panel.authLimiter = httpauth.NewLimiter(httpauth.DefaultFailLimit, httpauth.DefaultFailWindow)
	}

	panel.audit = newAuditLog(config.AuditDir)
	panel.audit.initAuditSize()
	panel.routes()

	return panel
}

// Logs 返回日志环形缓冲（main 经 MultiWriter 镜像 log 与 chat 表格日志进来）。
func (p *Panel) Logs() *Ring { return p.logs }

func (p *Panel) routes() {
	p.routesPages()
	p.routesAccounts()
	p.routesTasks()
	p.routesImport()
	p.routesUsage()
	p.routesKeys()
	p.routesConfig()
	p.routesModels()
}

func (p *Panel) routesPages() {
	p.mux.HandleFunc("GET /panel/{$}", p.index)
	p.mux.HandleFunc("GET /panel/{path...}", p.asset)
	p.mux.HandleFunc("GET /panel/api/overview", p.withReadAuth(p.overview))
	p.mux.HandleFunc("GET /panel/api/logs", p.withReadAuth(p.logsHandler))
	p.mux.HandleFunc("GET /panel/api/error_summary", p.withReadAuth(p.errorSummaryHandler))
	p.mux.HandleFunc("GET /panel/api/request_metrics", p.withReadAuth(p.requestMetrics))
	p.mux.HandleFunc("GET /panel/api/request_logs", p.withReadAuth(p.requestLogs))
	p.mux.HandleFunc("GET /panel/api/audit", p.withReadAuth(p.auditList))
	p.mux.HandleFunc("GET /panel/api/agents", p.withReadAuth(p.agentsStatus))
	p.mux.HandleFunc("POST /panel/api/agents/{client}/apply", p.withAdminAuth(p.agentsApply))
	p.mux.HandleFunc("POST /panel/api/agents/{client}/restore", p.withAdminAuth(p.agentsRestore))
}

//nolint:dupl // 路由注册逐条平铺，形状相近但端点与 handler 各异。
func (p *Panel) routesAccounts() {
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/revive", p.withAdminAuth(p.accountRevive))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/disable", p.withAdminAuth(p.accountDisable))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/pause", p.withAdminAuth(p.accountPause))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/resume", p.withAdminAuth(p.accountResume))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/proxy_slot", p.withAdminAuth(p.accountProxySlot))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/checkin", p.withAdminAuth(p.accountCheckin))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/balance", p.withAdminAuth(p.accountBalance))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/travel", p.withAdminAuth(p.accountTravel))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/remove", p.withAdminAuth(p.accountRemove))
}

func (p *Panel) routesTasks() {
	p.mux.HandleFunc("GET /panel/api/accounts/{uid}/tasks", p.withReadAuth(p.accountTasks))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/tasks/accept", p.withAdminAuth(p.accountTaskAccept))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/tasks/accept_all", p.withAdminAuth(p.taskAcceptAll))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/tasks/claim", p.withAdminAuth(p.accountTaskClaim))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/tasks/auto", p.withAdminAuth(p.accountTaskAuto))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/tasks/auto_all", p.withAdminAuth(p.accountTaskAutoAll))
	p.mux.HandleFunc("POST /panel/api/tasks/scan_all", p.withAdminAuth(p.tasksScanAll))
	p.mux.HandleFunc("POST /panel/api/tasks/run_queue", p.withAdminAuth(p.tasksRunQueue))
	p.mux.HandleFunc("GET /panel/api/tasks/queue", p.withReadAuth(p.tasksQueueStatus))
	p.mux.HandleFunc("GET /panel/api/tasks/runs", p.withReadAuth(p.tasksRuns))
	p.mux.HandleFunc("POST /panel/api/checkin_all", p.withAdminAuth(p.checkinAll))
	p.mux.HandleFunc("POST /panel/api/travel_all", p.withAdminAuth(p.travelAll))
	p.mux.HandleFunc("POST /panel/api/activity_all", p.withAdminAuth(p.activityAll))
	p.mux.HandleFunc("POST /panel/api/keepalive_all", p.withAdminAuth(p.keepaliveAll))
	p.mux.HandleFunc("POST /panel/api/blackcat_all", p.withAdminAuth(p.blackcatAll))
	p.mux.HandleFunc("POST /panel/api/streak_bonus_all", p.withAdminAuth(p.streakBonusAll))
	p.mux.HandleFunc("POST /panel/api/balance_all", p.withAdminAuth(p.balanceAll))
}

func (p *Panel) routesImport() {
	p.mux.HandleFunc("POST /panel/api/login/start", p.withAdminAuth(p.loginStart))
	p.mux.HandleFunc("GET /panel/api/login/poll", p.withAdminAuth(p.loginPoll))
	p.mux.HandleFunc("GET /panel/api/login/regions", p.withAdminAuth(p.loginRegions))
	p.mux.HandleFunc("POST /panel/api/import/cockpit", p.withAdminAuth(p.importCockpit))
	p.mux.HandleFunc("GET /panel/api/import/desktop/scan", p.withAdminAuth(p.desktopScan))
	p.mux.HandleFunc("POST /panel/api/import/desktop", p.withAdminAuth(p.desktopImport))
	p.mux.HandleFunc("POST /panel/api/export/encrypted", p.withAdminAuth(p.exportEncrypted))
	p.mux.HandleFunc("POST /panel/api/import/encrypted", p.withAdminAuth(p.importEncrypted))
}

//nolint:dupl // 同 routesAccounts：逐条平铺比抽象注册表更易检索。
func (p *Panel) routesUsage() {
	p.mux.HandleFunc("GET /panel/api/packages", p.withReadAuth(p.packages))
	p.mux.HandleFunc("GET /panel/api/credit_history", p.withReadAuth(p.creditHistoryHandler))
	// 官方补偿（上游同步窗口 issue #204）：只读状态 + 逐号/全量「先查后领」。
	p.mux.HandleFunc("GET /panel/api/compensation", p.withReadAuth(p.compensationStatus))
	p.mux.HandleFunc("POST /panel/api/compensation_all", p.withAdminAuth(p.compensationAll))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/compensation", p.withAdminAuth(p.accountCompensation))
	p.mux.HandleFunc("GET /panel/api/usage", p.withReadAuth(p.usage))
	p.mux.HandleFunc("POST /panel/api/usage/save", p.withAdminAuth(p.usageSave))
	p.mux.HandleFunc("GET /panel/api/usage/remaining", p.withReadAuth(p.usageRemaining))
	p.mux.HandleFunc("POST /panel/api/usage/backfill", p.withAdminAuth(p.usageBackfill))
}

func (p *Panel) routesKeys() {
	p.mux.HandleFunc("GET /panel/api/keys", p.withReadAuth(p.keysList))
	p.mux.HandleFunc("POST /panel/api/keys", p.withAdminAuth(p.keysCreate))
	p.mux.HandleFunc("POST /panel/api/keys/{id}/revoke", p.withAdminAuth(p.keysRevoke))
	p.mux.HandleFunc("POST /panel/api/keys/{id}/delete", p.withAdminAuth(p.keysDelete))
	p.mux.HandleFunc("GET /panel/api/pricing", p.withReadAuth(p.pricingHandler))
	p.mux.HandleFunc("POST /panel/api/keys/{id}/limits", p.withAdminAuth(p.keysUpdateLimits))
	p.mux.HandleFunc("POST /panel/api/keys/{id}/realm", p.withAdminAuth(p.keysUpdateRealm))
}

func (p *Panel) routesModels() {
	p.mux.HandleFunc("GET /panel/api/models", p.withReadAuth(p.models))
	p.mux.HandleFunc("GET /panel/api/model_probes", p.withReadAuth(p.modelProbes))
	p.mux.HandleFunc("POST /panel/api/model_probes", p.withAdminAuth(p.modelProbe))
	p.mux.HandleFunc("GET /panel/api/model_health", p.withReadAuth(p.modelHealth))
}

func (p *Panel) routesConfig() {
	p.mux.HandleFunc("GET /panel/api/config", p.withAdminAuth(p.getConfig))
	p.mux.HandleFunc("POST /panel/api/config", p.withAdminAuth(p.saveConfig))
	p.mux.HandleFunc("POST /panel/api/config/restore", p.withAdminAuth(p.restoreConfig))
}

// ServeHTTP 统一入口：先写安全响应头再分发，保证页面、静态资源、API
// 与 401 错误响应全都带上（API 也可能在浏览器里被直接打开）。
// panelAPIMaxBodyBytes 管理面请求体统一上限（字节）。个别导入端点另有更小的
// 内部上限；这里兜住未来新增的 handler，避免未设上限的读 body 占满内存。
const panelAPIMaxBodyBytes = 16 << 20

func (p *Panel) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	setSecurityHeaders(w)

	if strings.HasPrefix(r.URL.Path, "/panel/api/") {
		// 敏感数据（配置、账号、密钥）不允许浏览器或中间代理缓存。
		w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate")
		w.Header().Set("Pragma", "no-cache")
		limitPanelBody(w, r)
	}

	if p.audit != nil && r.Method != http.MethodGet && r.Method != http.MethodHead &&
		strings.HasPrefix(r.URL.Path, "/panel/api/") {
		auditedWriter := &auditResponseWriter{w: w}

		// panic 也要留审计：handler 可能已经写了部分状态变更，直接 abort 连接
		// 会让这次写操作不可追溯。记录后原样抛回，保持 net/http 的中止语义。
		defer func() {
			if rec := recover(); rec != nil {
				p.audit.Append(p.auditEntryFor(auditedWriter, r, true))
				panic(rec)
			}

			p.audit.Append(p.auditEntryFor(auditedWriter, r, false))
		}()

		p.mux.ServeHTTP(auditedWriter, r)

		return
	}

	p.mux.ServeHTTP(w, r)
}

// limitPanelBody 给管理面请求体套上统一上限。超限在 handler 读取时返回错误
// （MaxBytesReader 负责），不会先把整个 body 缓冲进内存。
func limitPanelBody(w http.ResponseWriter, r *http.Request) {
	if r.Body == nil {
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, panelAPIMaxBodyBytes)
}

// withReadAuth 管理面只读入口：admin 或 readonly 均可。默认 api_key 在未配置
// admin/readonly 时回落为 admin（单密钥默认）。
//
// 鉴权统一走 httpauth.AuthenticateManagement（常量时间、全候选遍历）。失败预算
// 只在凭据校验失败时消耗；作用域不足返回 403，不消耗预算。
func (p *Panel) withReadAuth(next http.HandlerFunc) http.HandlerFunc {
	return p.withManagementAuth(next, false)
}

// withAdminAuth 管理面写入口：只接受 admin；readonly 命中返回 403。
func (p *Panel) withAdminAuth(next http.HandlerFunc) http.HandlerFunc {
	return p.withManagementAuth(next, true)
}

// withManagementAuth 管理面统一鉴权。requireAdmin=true 时 readonly 命中返回 403。
func (p *Panel) withManagementAuth(next http.HandlerFunc, requireAdmin bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ip := httpauth.ClientIP(r, p.cfg.TrustedProxies)
		if ok, retry := p.authLimiter.Allow(ip); !ok {
			seconds := max(int((retry+time.Second-1)/time.Second), 1)
			w.Header().Set("Retry-After", strconv.Itoa(seconds))
			writeErr(w, http.StatusTooManyRequests, "too_many_auth_failures")

			return
		}

		res := httpauth.AuthenticateManagement(r, p.apiKey(), p.adminKey(), p.readonlyKey())
		if !res.OK {
			p.authLimiter.Fail(ip)
			writeErr(w, http.StatusUnauthorized, "invalid_api_key")

			return
		}

		p.authLimiter.Reset(ip)

		if requireAdmin && res.Scope != httpauth.ScopeAdmin {
			// 凭据有效但作用域不足：泛化 403，不区分不存在/已撤销/作用域不足。
			writeErr(w, http.StatusForbidden, "forbidden")

			return
		}

		next(w, r)
	}
}

// apiKey 当前生效的默认数据面密钥（Live 优先，回落静态字段）。
func (p *Panel) apiKey() string {
	if p.cfg.Live != nil {
		return p.cfg.Live.Load().APIKey
	}

	return p.cfg.APIKey
}

// adminKey 当前生效的 admin 凭据（热更新经 Live 快照）。
func (p *Panel) adminKey() string {
	if p.cfg.Live != nil {
		return p.cfg.Live.Load().AdminAPIKey
	}

	return p.cfg.AdminAPIKey
}

// readonlyKey 当前生效的 readonly 凭据。
func (p *Panel) readonlyKey() string {
	if p.cfg.Live != nil {
		return p.cfg.Live.Load().ReadonlyAPIKey
	}

	return p.cfg.ReadonlyAPIKey
}

// managementAuthRequired 报告管理面是否启用鉴权：三把凭据任一非空即可。
func (p *Panel) managementAuthRequired() bool {
	return p.apiKey() != "" || p.adminKey() != "" || p.readonlyKey() != ""
}

// expiringSoonWindow 返回调度器当前生效的快过期路由窗口；测试面板无调度器时返回 0。
func (p *Panel) expiringSoonWindow() time.Duration {
	if p.cfg.Scheduler == nil {
		return 0
	}

	return p.cfg.Scheduler.ExpiringSoonWindow()
}

// ---------------------------------------------------------------------------
// 只读接口
// ---------------------------------------------------------------------------

// overview 总览：池计数 + 每账号状态 + 面板元信息。
func (p *Panel) overview(w http.ResponseWriter, r *http.Request) {
	// 用 WithPaused 而不是 CountsDetailed：后者把暂停号并进 disabled（/status 的
	// 「不可用」口径），而概况卡片要分开展示（issue #125）。
	total, healthy, cooling, disabled, paused, inFlightFull := p.cfg.Pool.CountsDetailedWithPaused()

	sticky := 0
	if p.cfg.StickyCount != nil {
		sticky = p.cfg.StickyCount()
	}

	// 余额快照新鲜度（issue #200）：周期 + 最近一次完成时刻，供到期提醒卡片标注
	// 「每 N 分钟后台刷新、x 分钟前更新」。未刷新过时时间为零值（前端按 — 渲染）。
	balanceRefreshSec, balanceRefreshedAt := p.cfg.Pool.BalanceRefreshMeta()

	writeJSON(w, http.StatusOK, map[string]any{
		"version":              p.cfg.Version,
		"balance_refresh_sec":  balanceRefreshSec,
		"balance_refreshed_at": balanceRefreshedAt,
		"uptime_sec":           int(time.Since(p.started).Seconds()),
		"auth_required":        p.managementAuthRequired(),
		"redis_mode":           p.cfg.RedisMode,
		"sticky_sessions":      sticky,
		"total":                total,
		"healthy":              healthy,
		"cooling":              cooling,
		"disabled":             disabled,
		"paused":               paused,
		"in_flight_full":       inFlightFull,
		"in_flight_total":      p.cfg.Pool.InFlightTotal(),
		"max_in_flight_total":  p.cfg.Pool.MaxInFlightTotal(),
		"daily_limited":        p.cfg.Pool.DailyLimitedUIDs(),
		"daily_reset_at":       nextLocalMidnight(),
		"proxy_slots":          p.proxySlots(),
		"accounts":             p.cfg.Pool.List(),
		// model_locks 模型级限流全清单（哪些模型不能用、锁了几个号、还要锁多久）：
		// 与 accounts 的账号池视图互补，前端「模型锁池」表直接渲染。无锁时为 null。
		"model_locks": p.cfg.Pool.ModelLockView(),
	})
}

// logsHandler 返回日志环形缓冲快照（时间升序，含频道标记 chat/task/sys）。
func (p *Panel) logsHandler(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"entries": p.logs.Snapshot()})
}

// requestMetrics 返回进程内请求指标、最近 100 条与归档状态。
func (p *Panel) requestMetrics(w http.ResponseWriter, r *http.Request) {
	if p.cfg.RequestLog == nil {
		writeErr(w, http.StatusNotImplemented, "request logger not available")
		return
	}

	writeJSON(w, http.StatusOK, p.cfg.RequestLog.Snapshot())
}

// requestLogs 从 JSONL 归档读取最近请求；limit 默认 200、最大 1000。
// 支持按 outcome/account/model/client_ip/user_agent 过滤（字符串字段为包含匹配）
// 与 from/to 时间区间（闭区间，unix 秒或 RFC3339）——面板「运行日志」的筛选框、
// 来源查询与「今天 / 自定义区间」都走这里。
func (p *Panel) requestLogs(w http.ResponseWriter, r *http.Request) {
	if p.cfg.RequestLog == nil {
		writeErr(w, http.StatusNotImplemented, "request logger not available")
		return
	}

	limit := 200

	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}

	if limit > 1000 {
		limit = 1000
	}

	values := r.URL.Query()

	rows, err := p.cfg.RequestLog.ReadArchive(limit, &reqlog.Filter{
		Outcome:   values.Get("outcome"),
		Account:   values.Get("account"),
		KeyID:     values.Get("key_id"),
		Model:     values.Get("model"),
		ClientIP:  values.Get("client_ip"),
		UserAgent: values.Get("user_agent"),
		From:      parseTimeParam(values.Get("from")),
		To:        parseTimeParam(values.Get("to")),
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	// 空结果回 []（而不是 JSON null）：前端把 null 与"归档关闭"混在一起会走错分支，
	// 显示成不满足筛选条件的最近请求。
	if rows == nil {
		rows = []reqlog.Event{}
	}

	writeJSON(w, http.StatusOK, map[string]any{"entries": rows, "limit": limit})
}

// models 实时查询上游模型列表与 reasoning 实际档位（直连上游，不读路由层 1h 缓存）：
// 回答"该模型到底支持哪几档思考"。顺带刷新 client 的 effort 降级能力缓存。
// 与 /v1/models 同口径的双域输出：CN 域模型加 "cn:" 前缀、global 域加 "global:" 前缀
// （gateway 路由协议，前端显示的 id 就是调用时要填的完整 model 值）。
// 各域独立探测、独立容错：某域无可用账号则整域跳过；两域全空时才报错
// （有错误明细回 502，一个账号都没有回 503）。
func (p *Panel) models(w http.ResponseWriter, r *http.Request) {
	// 目录错误与繁忙度错误分开收集（issue #401）：catalogErrs 决定「整份视图是否可信」
	// （为空时进 502 文案），busyErrs 只影响一列观测（少一列 busy，不影响目录）。
	out, catalogErrs := p.collectAllModels()

	var busyErrs []string
	if len(out) > 0 {
		p.attachModelBusy(out, &busyErrs)
	}

	if len(out) == 0 {
		writeModelsEmpty(w, catalogErrs)

		return
	}

	// fetch_errors 只讲目录（视图是否可信）：繁忙度明细另走 busy_errors，避免「少一列
	// 观测」把「目录拿不到」的原因淹掉。
	resp := map[string]any{"ok": true, "models": out, "fetch_errors": catalogErrs}
	if len(busyErrs) > 0 {
		resp["busy_errors"] = busyErrs
	}

	writeJSON(w, http.StatusOK, resp)
}

// collectAllModels 双域目录收集：各域独立探测、独立容错，错误只收目录面（cn:/global:）。
func (p *Panel) collectAllModels() ([]map[string]any, []string) {
	out := make([]map[string]any, 0)

	var errs []string

	if entries, errMsg := p.collectCNModels(); len(entries) > 0 || errMsg != "" {
		out = append(out, entries...)
		if errMsg != "" {
			errs = append(errs, errMsg)
		}
	}

	if entries, errMsg := p.collectGlobalModels(); len(entries) > 0 || errMsg != "" {
		out = append(out, entries...)
		if errMsg != "" {
			errs = append(errs, errMsg)
		}
	}

	return out, errs
}

// writeModelsEmpty 两域全空：有目录错误明细回 502，一个账号都没有回 503。
func writeModelsEmpty(w http.ResponseWriter, catalogErrs []string) {
	if len(catalogErrs) > 0 {
		writeErr(w, http.StatusBadGateway, "fetch models: "+strings.Join(catalogErrs, "; "))

		return
	}

	writeErr(w, http.StatusServiceUnavailable, "没有可用账号：请先在面板添加账号再查询")
}

// collectCNModels 查 CN 域模型：无可用 CN 账号或账号缺失时返回空；查询失败返回
// 错误描述（前缀 "cn:"）。此前无条件 Pool.Pick()+FetchModels——选中 global 账号时
// 打 CN 端点必然失败，混合池表现为偶发 502，纯 global 池必炸。
func (p *Panel) collectCNModels() ([]map[string]any, string) {
	uids := p.cfg.Pool.AvailableUIDsForRealm("cn")
	if len(uids) == 0 {
		return nil, ""
	}

	acct := p.cfg.Pool.AuthByUID(uids[0])
	if acct == nil {
		return nil, ""
	}

	infos, err := p.cfg.Upstream.FetchModels(acct)
	if err != nil {
		return nil, "cn: " + err.Error()
	}

	out := make([]map[string]any, 0, len(infos))
	for i := range infos {
		out = append(out, panelModelEntry(
			"cn", &infos[i], infos[i].Efforts, infos[i].DefaultEffort, p.cfg.Upstream.HTTP,
		))
	}

	return out, ""
}

// collectGlobalModels 查 global 域模型：路由开关关、无可用 global 账号或账号缺失时
// 返回空；上游返回空目录时返回错误描述。Upstream.GlobalEnabled 是探测侧同一道闸，
// 与 main 装配的 config global.enabled 一致。
func (p *Panel) collectGlobalModels() ([]map[string]any, string) {
	if !p.cfg.Upstream.GlobalEnabled {
		return nil, ""
	}

	uids := p.cfg.Pool.AvailableUIDsForRealm("global")
	if len(uids) == 0 {
		return nil, ""
	}

	acct := p.cfg.Pool.AuthByUID(uids[0])
	if acct == nil {
		return nil, ""
	}

	infos := p.cfg.Upstream.FetchGlobalModelInfos(acct)
	if len(infos) == 0 {
		return nil, "global: 上游未返回可用模型"
	}

	efforts, defaults := p.cfg.Upstream.GlobalEffortSnapshot()

	out := make([]map[string]any, 0, len(infos))
	for i := range infos {
		out = append(out, panelModelEntry(
			"global", &infos[i], efforts[infos[i].ID], defaults[infos[i].ID], p.cfg.Upstream.HTTP,
		))
	}

	return out, ""
}

// panelModelEntry 构造单个模型条目（两域共用）：id 带 realm 前缀（调用值即显示值），
// context_length / max_output_tokens 走四级查找链，effort 档位按 realm 域取
// EffortListing（远端权威 ∪ 静态兜底表）——与 /v1/models 同一口径，两侧不再漂移。
//
//nolint:funlen,maintidx // 单模型条目字段密集，平铺便于与 /v1/models 对照。
func panelModelEntry(
	realm string,
	modelInfo *upstream.ModelInfo,
	remoteEfforts []string,
	remoteDefault string,
	httpc *http.Client,
) map[string]any {
	entry := map[string]any{
		"id":                   realm + ":" + modelInfo.ID,
		"name":                 modelInfo.Name,
		"default_effort":       modelInfo.DefaultEffort,
		"supported_efforts":    modelInfo.Efforts,
		"can_disable_thinking": modelInfo.CanDisableThinking,
		"supports_reasoning":   modelInfo.SupportsReasoning,
		"supports_images":      modelInfo.SupportsImages,
		"credits":              modelInfo.Credits,
		"description":          modelInfo.Description,
		"tags":                 modelInfo.Tags,
		"vendor":               modelInfo.Vendor,
		"is_default":           modelInfo.IsDefault,
		"supports_tool_call":   modelInfo.SupportsToolCall,
		"only_reasoning":       modelInfo.OnlyReasoning,
		"reasoning_effort":     modelInfo.ReasoningEffort,
		"reasoning_summary":    modelInfo.ReasoningSummary,
	}
	// 限时优惠（modelPromotions）：credits 是牌价，promo_* 是当前生效折扣
	// （WorkBuddy 客户端显示的就是这个生效价）。前端据此显示「生效价+标签+划线牌价」。
	if modelInfo.PromoFactor != nil {
		entry["promo_factor"] = *modelInfo.PromoFactor
		entry["promo_credits"] = modelInfo.PromoCredits
	}

	if modelInfo.PromoLabel != "" {
		entry["promo_label"] = modelInfo.PromoLabel
	}

	if modelInfo.PromoNote != "" {
		entry["promo_note"] = modelInfo.PromoNote
	}

	// 官方档位徽章（issue #274）：与 /v1/models 同口径，仅展示。
	applyPanelModelTier(entry, modelInfo)

	if mods := modelInfo.EffectiveInputModalities(); len(mods) > 0 {
		// 输入模态（issue #109）：与 /v1/models 同口径，未知时省略（不宣称）。
		entry["input_modalities"] = mods
	}

	if modelInfo.MaxAllowedSize > 0 {
		entry["max_allowed_size"] = modelInfo.MaxAllowedSize
	}

	entry["context_length"] = upstream.ContextWindowListingV4(modelInfo.ID, modelInfo.ContextWindow, httpc)
	if mo, ok := upstream.MaxOutputTokensListingV4(modelInfo.ID, modelInfo.MaxTokens, httpc); ok {
		entry["max_output_tokens"] = mo
	}

	if efforts, def := upstream.EffortListing(realm, modelInfo.ID, remoteEfforts, remoteDefault); efforts != nil {
		entry["supported_efforts"] = efforts
		if def != "" {
			entry["default_effort"] = def
		}
	}

	return entry
}

// applyPanelModelTier 挂载官方档位展示字段（nil 时省略）。
func applyPanelModelTier(entry map[string]any, modelInfo *upstream.ModelInfo) {
	if modelInfo.Tier != nil {
		entry["tier"] = modelInfo.Tier
	}
}

// modelProbes 返回模型输出上限的探测结果（scripts/probe_max_tokens.py --panel-out
// 写入的契约文件），供前端在「模型与档位」的实测列做风险标注。
//
// 设计边界：纯只读透传——文件缺失/未配置返回空集（面板退化为无标注，与历史行为
// 一致），网关自身不解析字段语义、不据此做任何路由或出站决策；上游改了限制后
// 重跑一次工具、下次查询即刷新，无需重启网关。
func (p *Panel) modelProbes(w http.ResponseWriter, r *http.Request) {
	out := map[string]any{"probes": map[string]json.RawMessage{}, "exists": false}
	if p.cfg.ProbeFile == "" {
		writeJSON(w, http.StatusOK, out)
		return
	}

	raw, err := os.ReadFile(p.cfg.ProbeFile)
	if err != nil {
		if os.IsNotExist(err) {
			writeJSON(w, http.StatusOK, out)
			return
		}

		writeErr(w, http.StatusInternalServerError, "read probes: "+err.Error())

		return
	}

	var item struct {
		Version int                        `json:"version"`
		Probes  map[string]json.RawMessage `json:"probes"`
	}
	if err := json.Unmarshal(raw, &item); err != nil {
		writeErr(w, http.StatusBadGateway, "parse probes: "+err.Error())
		return
	}

	if item.Probes == nil {
		item.Probes = map[string]json.RawMessage{}
	}

	out["probes"] = item.Probes

	out["exists"] = true
	if fi, err := os.Stat(p.cfg.ProbeFile); err == nil {
		out["updated_at"] = fi.ModTime().Format(time.RFC3339)
	}

	writeJSON(w, http.StatusOK, out)
}

// ---------------------------------------------------------------------------
// 账号运维
// ---------------------------------------------------------------------------

// accountRevive 手动复活：清禁用 + 冷却 + 熔断（运维口径无条件恢复）。
// accountProxySlot 绑定/解绑账号的出站代理槽（issue #96）；空串 = 解绑。
func (p *Panel) accountProxySlot(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Slot string `json:"slot"`
	}
	if err := decodeJSONBody(r.Body, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}

	slot := strings.TrimSpace(body.Slot)
	if slot != "" && !p.hasProxySlot(slot) {
		writeErr(w, http.StatusBadRequest, "unknown proxy slot")
		return
	}

	uid := r.PathValue("uid")
	if !p.cfg.Pool.SetProxySlot(uid, slot) {
		writeErr(w, http.StatusNotFound, "account not found")
		return
	}

	log.Printf("panel: 账号 %s 绑定代理槽 %q", uid, slot)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// proxySlots 返回当前生效的代理槽视图（provider 为空时返回 nil）。
func (p *Panel) proxySlots() []ProxySlotInfo {
	if p.cfg.ProxySlots == nil {
		return nil
	}

	return p.cfg.ProxySlots()
}

func (p *Panel) hasProxySlot(id string) bool {
	for _, slot := range p.proxySlots() {
		if slot.ID == id {
			return true
		}
	}

	return false
}

func (p *Panel) accountRevive(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	if _, ok := p.cfg.Pool.Status(uid); !ok {
		writeErr(w, http.StatusNotFound, "account not found")
		return
	}

	p.cfg.Pool.Revive(uid)
	log.Printf("panel: revive uid=%s（人工清除禁用/冷却/熔断）", uid)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// accountDisable 人工禁用（不再参与选号，需面板 revive 或重登恢复）。
func (p *Panel) accountDisable(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	if _, ok := p.cfg.Pool.Status(uid); !ok {
		writeErr(w, http.StatusNotFound, "account not found")
		return
	}

	p.cfg.Pool.Disable(uid, "manual disable (panel)")
	log.Printf("panel: disable uid=%s（人工禁用）", uid)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// accountPause 暂停选号：账号退出选号候选，但**照常参与**签到 / 活跃上报 / 保活 /
// 余额刷新。与 disable 的区别：不写 reason、不清冷却域、不重置计数——账号是「临时
// 让位」而非「判死」，点「恢复选号」即可立刻回到池子（无需重登或解冻）。
func (p *Panel) accountPause(w http.ResponseWriter, r *http.Request) {
	p.accountToggle(w, r, p.cfg.Pool.Pause, "pause uid=%s（暂停选号，保号任务照常）")
}

// accountResume 解除暂停选号（幂等，对未暂停账号为空操作）。
func (p *Panel) accountResume(w http.ResponseWriter, r *http.Request) {
	p.accountToggle(w, r, p.cfg.Pool.Resume, "resume uid=%s（恢复参与选号）")
}

// accountToggle 切换账号选号状态并输出统一回执。
func (*Panel) accountToggle(
	w http.ResponseWriter, r *http.Request, toggle func(string) bool, logFormat string,
) {
	uid := r.PathValue("uid")
	if !toggle(uid) {
		writeErr(w, http.StatusNotFound, "account not found")

		return
	}

	log.Printf("panel: "+logFormat, uid)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// runCheckin 执行单号签到并返回 (跳过原因, 是否已签, 说明文案)。
// 跳过原因非空表示企业版无个人签到体系（issue #108）：不发起上游调用。
// global 账号与调度器同口径正常签到（upstream.DailyCheckin 按 realm 分流）。
// 其余情况按签到结果分类呈现：幂等拒绝同样是「今日已签」；活动未开启是上游
// 非错误态，不标记已签、给出可区分文案（issue #27）；其余才是真失败。
func (p *Panel) runCheckin(
	ctx context.Context, account *auth.Auth, uid string,
) (denyReason string, done bool, msg string) {
	if denyReason = checkinDenyReason(account); denyReason != "" {
		return denyReason, false, denyReason
	}

	if err := p.cfg.Upstream.DailyCheckin(ctx, account); err != nil {
		switch upstream.ClassifyCheckin(err) {
		case upstream.CheckinAlready:
			p.cfg.Pool.NoteCheckinDone(uid)
			return "", true, "今天已签到"
		case upstream.CheckinInactive:
			return "", false, "签到活动未开启（非失败）"
		default:
			return "", false, err.Error() // 业务错误照常查余额
		}
	}

	p.cfg.Pool.NoteCheckinDone(uid)

	return "", true, ""
}

// accountCheckin 单号签到：DailyCheckin + 余额查询解冻（已签到等业务错误不阻塞余额刷新），
// 与 scheduler.RunCheckinNow 的单号语义一致。
func (p *Panel) accountCheckin(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")

	account := p.cfg.Pool.AuthByUID(uid)
	if account == nil {
		writeErr(w, http.StatusNotFound, "account not found")
		return
	}
	// 账号体系门控（issue #108）：企业版无个人签到体系（上游 400 code 10001），
	// 不发起签到调用，端点退化为「刷新额度」；global 账号与调度器一致走签到。
	// 额度查询按 realm/企业口径分流（upstream.UserResourceSnapshot）。
	// 前端对企业版账号不渲染「签到」按钮；此处是 API 侧防御（外部脚本/旧缓存
	// 前端仍可能调用）。
	denyReason, checkinDone, checkinMsg := p.runCheckin(r.Context(), account, uid)

	resp := map[string]any{"ok": true, "checkin_done": checkinDone}
	if denyReason != "" {
		// 明确「跳过」而非「签到成功」：checkin_done 保持 false，reason 可读。
		resp["skipped"] = true
		resp["reason"] = denyReason
	}

	if account.IsEnterprise() {
		resp["enterprise"] = true
	}

	if checkinMsg != "" {
		resp["checkin_message"] = checkinMsg
	}

	// 用含逐日到期批次的快照写入：单号签到同样保留完整批次，而不是把后台刷新
	// 刚写入的批次清单清空（SetCreditsDetailed 无批次信息，会清旧批次）。
	snap, err := p.cfg.Upstream.UserResourceSnapshot(r.Context(), account, p.expiringSoonWindow())
	if err != nil {
		resp["balance_error"] = err.Error()
		writeJSON(w, http.StatusOK, resp)

		return
	}

	p.cfg.Pool.ReenableIfCredits(uid, snap.Remain, snap.Total)
	p.cfg.Pool.SetCreditsSnapshot(uid, snap.Remain, snap.Total, snap.Expiring, poolExpiryBuckets(snap.Buckets))
	resp["credits"] = snap.Remain
	resp["credits_total"] = snap.Total
	log.Printf("panel: checkin uid=%s msg=%q credits=%d/%d", uid, checkinMsg, snap.Remain, snap.Total)
	writeJSON(w, http.StatusOK, resp)
}

// accountBalance 单号余额刷新：更新余额与到期快照，不触碰冷却状态。
func (p *Panel) accountBalance(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")

	account := p.cfg.Pool.AuthByUID(uid)
	if account == nil {
		writeErr(w, http.StatusNotFound, "account not found")
		return
	}

	// 与签到/后台刷新同口径：写完整批次，单号刷新不清空到期明细。
	snap, err := p.cfg.Upstream.UserResourceSnapshot(r.Context(), account, p.expiringSoonWindow())
	if err != nil {
		writeErr(w, http.StatusBadGateway, "user resource: "+err.Error())
		return
	}

	p.cfg.Pool.SetCreditsSnapshot(uid, snap.Remain, snap.Total, snap.Expiring, poolExpiryBuckets(snap.Buckets))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "credits": snap.Remain, "credits_total": snap.Total})
}

// poolExpiryBuckets upstream → pool 类型转换（pool 不依赖 upstream 包）。
func poolExpiryBuckets(buckets []upstream.ExpiryBucket) []pool.ExpiryBucket {
	if len(buckets) == 0 {
		return nil
	}

	out := make([]pool.ExpiryBucket, 0, len(buckets))
	for _, bucket := range buckets {
		out = append(out, pool.ExpiryBucket{Date: bucket.Date, ExpireAt: bucket.ExpireAt, Remain: bucket.Remain})
	}

	return out
}

// accountRemove 移除账号：先出池（池内退役凭证并立即落盘 state），再删 auth 文件。
func (p *Panel) accountRemove(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")

	account := p.cfg.Pool.Remove(uid)
	if account == nil {
		writeErr(w, http.StatusNotFound, "account not found")
		return
	}

	fileMsg := ""

	if account.FilePath != "" {
		if err := os.Remove(account.FilePath); err != nil && !os.IsNotExist(err) {
			fileMsg = err.Error()
		}
		// safefile.WriteKeep 会留一份上一版凭证（含仍可能有效的 access token）；
		// 删除账号时一并清掉，否则凭证残留在 auth 目录。
		if err := os.Remove(account.FilePath + ".bak"); err != nil && !os.IsNotExist(err) && fileMsg == "" {
			fileMsg = err.Error()
		}
	}

	if fileMsg != "" {
		log.Printf("panel: remove uid=%s（auth 文件删除失败: %s）", uid, fileMsg)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "file_error": fileMsg})

		return
	}

	log.Printf("panel: remove uid=%s（已出池并删除凭证文件）", uid)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ---------------------------------------------------------------------------
// 批量任务
// ---------------------------------------------------------------------------

// startBatchTask 批量任务手动触发的统一入口（issue #112）：
// 调度器与面板共用运行级守卫——同一任务族已在跑时返回 409，不并行第二轮。
// 启动成功后立即返回 {ok,started,running}；本轮状态/结果经 /panel/api/tasks/runs 查询。
func (p *Panel) startBatchTask(w http.ResponseWriter, task, label string) {
	if p.cfg.Scheduler == nil {
		writeErr(w, http.StatusNotImplemented, "scheduler not available")
		return
	}

	if !p.cfg.Scheduler.StartTask(task, scheduler.TriggerManual) {
		writeJSON(w, http.StatusConflict, map[string]any{
			"ok": false, "started": false, "running": true, "task": task,
			"error": label + "正在运行中，请等本轮结束",
		})

		return
	}

	log.Printf("panel: 手动全量%s已触发", label)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "started": true, "task": task})
}

// checkinAll 手动触发全量签到（异步执行；含连登管家）。
func (p *Panel) checkinAll(w http.ResponseWriter, r *http.Request) {
	p.startBatchTask(w, scheduler.TaskCheckin, "签到")
}

// travelAll 手动触发全量猫猫旅行巡检（异步执行）。
func (p *Panel) travelAll(w http.ResponseWriter, r *http.Request) {
	p.startBatchTask(w, scheduler.TaskTravel, "旅行巡检")
}

// activityAll 手动触发全量活跃上报（异步执行；点亮连登 + 解锁领养前置）。
func (p *Panel) activityAll(w http.ResponseWriter, r *http.Request) {
	p.startBatchTask(w, scheduler.TaskActivity, "活跃上报")
}

// keepaliveAll 手动触发全量 token 保活（异步执行）。
func (p *Panel) keepaliveAll(w http.ResponseWriter, r *http.Request) {
	p.startBatchTask(w, scheduler.TaskKeepalive, "保活")
}

// blackcatAll 手动触发夜猫子补足（异步执行；窗口外由调度器跳过）。
func (p *Panel) blackcatAll(w http.ResponseWriter, r *http.Request) {
	p.startBatchTask(w, scheduler.TaskBlackcat, "夜猫子")
}

// streakBonusAll 手动触发连登兑换 + 抽奖（异步执行）。
func (p *Panel) streakBonusAll(w http.ResponseWriter, r *http.Request) {
	p.startBatchTask(w, scheduler.TaskStreak, "连登管家")
}

// tasksRuns 返回各批量任务族的当前/最近一轮运行状态（issue #112）。
func (p *Panel) tasksRuns(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Scheduler == nil {
		writeErr(w, http.StatusNotImplemented, "scheduler not available")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "runs": p.cfg.Scheduler.RunTaskStates()})
}

// balanceAll 手动全量刷新余额：并发查上游、写回池内 credits（含解冻语义），
// 完成后返回——面板紧接着拉 overview 即是最新值。同步等待（逐号并发有界）
// 比"触发后盲刷"体验更确定；客户端断开则停止派发剩余账号，不空跑上游。
func (p *Panel) balanceAll(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Scheduler == nil {
		writeErr(w, http.StatusNotImplemented, "scheduler not available")
		return
	}

	ctx := r.Context()
	p.cfg.Scheduler.RunBalanceRefreshNowCtx(ctx)

	if ctx.Err() != nil {
		return // 客户端已断开：不再同步昵称或写回执
	}

	p.syncNicknames(ctx)

	if ctx.Err() != nil {
		return // 昵称同步期间客户端断开：不再写回执
	}

	log.Printf("panel: 手动全量余额刷新完成")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "accounts": p.cfg.Pool.List()})
}

// acquireSem 在 ctx 取消前占用一个信号量名额；已取消或等待中取消返回 false。
// 已有取消时不能只靠 select：sem 有空位会与 ctx.Done 同时就绪，随机选择仍可能派发。
func acquireSem(ctx context.Context, sem chan struct{}) bool {
	if ctx.Err() != nil {
		return false
	}

	select {
	case sem <- struct{}{}:
		// select 同时就绪时随机选择：拿到名额后再确认一次，覆盖「检查后、
		// 取名额前」刚好取消的窗口；已取消则立即归还名额。
		if ctx.Err() != nil {
			<-sem
			return false
		}

		return true
	case <-ctx.Done():
		return false
	}
}

// syncNicknames 手动刷新时的昵称同步（issue #94：上游改名免重登）。
// 只在面板手动「刷新」路径调用——后台余额定时器不触发（用户明确要求资料接口
// 仅手动触达）。逐号拉 /console/account，只取 nickname（手机号等敏感字段在
// upstream.FetchAccountProfile 内即被丢弃）；单号失败静默跳过，不打断余额刷新
// 的既有结果。ctx 取消后不再派发剩余账号（客户端断开时不必空跑上游）。
func (p *Panel) syncNicknames(ctx context.Context) {
	jobs := p.collectNicknameJobs()
	if len(jobs) == 0 {
		return
	}

	var (
		mu      sync.Mutex
		updated int
		failed  int
		sem     = make(chan struct{}, 3)
		wg      sync.WaitGroup
	)

	for _, j := range jobs {
		// 先取名额再起 goroutine：账号多时不在信号量上堆积阻塞协程。
		if !acquireSem(ctx, sem) {
			break
		}

		wg.Add(1)
		go func(j nicknameJob) {
			defer wg.Done()
			defer func() { <-sem }()

			numVal, num2 := p.fetchNickname(j)
			if numVal == 0 && num2 == 0 {
				return
			}

			mu.Lock()
			updated += numVal
			failed += num2
			mu.Unlock()
		}(j)
	}

	wg.Wait()

	if ctx.Err() != nil {
		return
	}

	if updated > 0 || failed > 0 {
		log.Printf("panel: 昵称同步：更新 %d 个，失败 %d 个（未变化不计数）", updated, failed)
	}
}

// nicknameJob 一次昵称同步任务：目标 uid 与其 auth 句柄。
type nicknameJob struct {
	uid string
	a   *auth.Auth
}

// collectNicknameJobs 收集需要同步昵称的账号：跳过禁用号与无 access token 的号。
func (p *Panel) collectNicknameJobs() []nicknameJob {
	var jobs []nicknameJob

	all := p.cfg.Pool.List()
	for i := range all {
		if all[i].Disabled {
			continue
		}

		a := p.cfg.Pool.AuthByUID(all[i].UID)
		if a == nil || a.AccessTokenValue() == "" {
			continue
		}

		jobs = append(jobs, nicknameJob{uid: all[i].UID, a: a})
	}

	return jobs
}

// fetchNickname 拉取单号昵称并写回池；返回 (更新数, 失败数)，均未变化时为 (0,0)。
func (p *Panel) fetchNickname(j nicknameJob) (int, int) {
	nick, err := p.cfg.Upstream.FetchAccountProfile(j.a)
	if err != nil {
		return 0, 1
	}

	if p.cfg.Pool.SetNickname(j.uid, nick) {
		return 1, 0
	}

	return 0, 0
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// usage 返回逐请求用量聚合。统计窗口三选一：
//   - from/to（unix 秒）：显式区间，用于「今天」与「自定义」——区间由浏览器按
//     本地时区算好再发，服务端时区与浏览器不一致时「今天」才不会被算错；
//   - hours：滚动窗口（默认 72，上限 1440=60 天），卡片汇总/按域/按账号/按模型/
//     时序**全部**按该窗口统计；显式 hours=0 表示全部历史（含 90 天前折叠出的日桶）；
//   - 都不给：等同于 hours=72（保持旧调用方行为）。
func (p *Panel) usage(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Usage == nil {
		writeErr(w, http.StatusNotImplemented, "usage recorder not available")
		return
	}

	win := parseUsageWindow(r)
	// 昵称仅用于展示，取自池快照（不含任何凭证）。
	nicks := map[string]string{}

	all := p.cfg.Pool.List()
	for i := range all {
		if all[i].Nickname != "" {
			nicks[all[i].UID] = all[i].Nickname
		}
	}

	var currentRate func(realm, model string) string
	if p.cfg.Upstream != nil {
		currentRate = p.cfg.Upstream.ModelRate
	}

	writeJSON(w, http.StatusOK, p.cfg.Usage.SnapshotWindow(win, nicks, currentRate))
}

// parseUsageWindow 解析用量统计窗口：from/to 任一给出则用显式区间；否则用 hours
// 滚动窗口（默认 72、上限 1440），显式 hours=0 表示全部历史。区间与 hours 互斥。
func parseUsageWindow(r *http.Request) usage.Window {
	values := r.URL.Query()
	win := usage.Window{Hours: 0, From: time.Time{}, To: time.Time{}}
	win.From = parseTimeParam(values.Get("from"))

	win.To = parseTimeParam(values.Get("to"))
	if !win.From.IsZero() || !win.To.IsZero() {
		return win
	}

	win.Hours = 72

	if v := values.Get("hours"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			win.Hours = n
		}
	}

	if win.Hours > maxUsageWindowHours {
		win.Hours = maxUsageWindowHours
	}

	return win
}

// parseTimeParam 解析时间查询参数：unix 秒（前端默认）或 RFC3339（便于手工调
// 接口/写脚本）。空串与非法值都返回零值 = 该侧不设界，不报错——区间参数是可选
// 增强，拼错一个 from 不该让整页用量打不开。
func parseTimeParam(str string) time.Time {
	str = strings.TrimSpace(str)
	if str == "" {
		return time.Time{}
	}

	if n, err := strconv.ParseInt(str, 10, 64); err == nil {
		if n <= 0 {
			return time.Time{}
		}
		// 兼容秒与毫秒（前端可能直接把 Date.now() 传上来）。
		if n > epochMillisThreshold {
			return time.UnixMilli(n)
		}

		return time.Unix(n, 0)
	}

	if t, err := time.Parse(time.RFC3339, str); err == nil {
		return t
	}

	if t, err := time.ParseInLocation("2006-01-02T15:04", str, time.Local); err == nil {
		return t
	}

	return time.Time{}
}

// usageSave 立即把内存中的用量桶落盘（正常由后台 30s 防抖刷新负责）。
func (p *Panel) usageSave(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Usage == nil {
		writeErr(w, http.StatusNotImplemented, "usage recorder not available")
		return
	}

	p.cfg.Usage.Save()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// packages 返回全部账号的积分包构成，供「积分构成」视图对比。
//
// 逐个账号向上游查（并发有上限，避免瞬时打满上游限流），失败只在对应账号上
// 标 error，不影响其它账号——一个号 token 失效不该让整页空白。客户端断开后
// 不再派发剩余账号。
//
//nolint:funlen // 并发拉包/合并/错误隔离线性展开。
func (p *Panel) packages(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	accts := p.cfg.Pool.List()
	out := make([]packageAccountRow, len(accts))

	sem := make(chan struct{}, 3)

	var wg sync.WaitGroup

	for i := range accts {
		status := &accts[i]

		// 先取名额再起 goroutine：账号多时不在信号量上堆积阻塞协程。
		if !acquireSem(ctx, sem) {
			break
		}

		wg.Add(1)
		go func(i int, status *pool.Status) {
			defer wg.Done()
			defer func() { <-sem }()

			packageAccountRow2 := packageAccountRow{UID: status.UID, Nickname: status.Nickname, Realm: status.Realm}

			account := p.cfg.Pool.AuthByUID(status.UID)
			if account == nil {
				packageAccountRow2.Error = "account not loaded"
				out[i] = packageAccountRow2

				return
			}

			packs, remain, size, err := p.cfg.Upstream.CreditPackages(account)
			if err != nil {
				packageAccountRow2.Error = err.Error()
				out[i] = packageAccountRow2

				return
			}

			packageAccountRow2.Packages = packs
			packageAccountRow2.Remain = remain
			packageAccountRow2.Size = size
			out[i] = packageAccountRow2
		}(i, status)
	}

	wg.Wait()

	if ctx.Err() != nil {
		return // 客户端已断开：不必排序与写回执
	}

	// 余额降序：多的在前，便于和少的对比。
	slices.SortStableFunc(out, func(a, b packageAccountRow) int { return cmp.Compare(b.Remain, a.Remain) })
	writeJSON(w, http.StatusOK, map[string]any{"accounts": out, "grants": creditGrants(out)})
}

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

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"ok": false, "error": msg})
}

var errTrailingJSON = errors.New("unexpected data after JSON body")

// decodeJSONBody 解码请求体中的单个 JSON 值：首个值之后的非空数据同样视为
// 畸形请求，避免 `{"force":true} garbage` 被静默接受并执行写操作。
func decodeJSONBody(body io.Reader, v any) error {
	dec := json.NewDecoder(body)
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("decode json body: %w", err)
	}

	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errTrailingJSON
	}

	return nil
}

// nextLocalMidnight 返回本地次日 0 点（日限额恢复时间，issue #95）。
func nextLocalMidnight() time.Time {
	now := time.Now()
	y, m, d := now.Date()

	return time.Date(y, m, d+1, 0, 0, 0, 0, now.Location())
}

// pricingHandler 返回只读价格快照（issue #99）。
func (p *Panel) pricingHandler(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Pricing == nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"enabled":       false,
			"exchange_rate": p.cfg.PricingRate,
		})

		return
	}

	snap := p.cfg.Pricing.Snapshot()
	writeJSON(w, http.StatusOK, map[string]any{
		"enabled":       true,
		"exchange_rate": p.cfg.PricingRate,
		"updated_at":    snap.UpdatedAt,
		"last_error":    snap.LastError,
		"prices":        snap.Prices,
	})
}

// packageAccountRow packages 接口的逐账号行。
type packageAccountRow struct {
	UID      string                   `json:"uid"`
	Nickname string                   `json:"nickname"`
	Realm    string                   `json:"realm"`
	Remain   int64                    `json:"remain"`
	Size     int64                    `json:"size"`
	Packages []upstream.CreditPackage `json:"packages"`
	Error    string                   `json:"error,omitempty"`
}

// creditGrant 积分包获取历史的一行（issue #100，只读快照）。
type creditGrant struct {
	UID       string `json:"uid"`
	Nickname  string `json:"nickname"`
	Realm     string `json:"realm"`
	Name      string `json:"name"`
	Source    string `json:"source"`
	Inferred  bool   `json:"inferred"`
	Size      int64  `json:"size"`
	Remain    int64  `json:"remain"`
	CreatedAt string `json:"created_at,omitempty"`
	ExpiresAt int64  `json:"expires_at,omitempty"`
	Status    string `json:"status"`
}

const (
	maxUsageWindowHours  = 24 * 60
	epochMillisThreshold = 1e12
	largePackageMinSize  = 1500
)

// creditGrants 把全部账号的积分包摊平成获取历史，按发放时间倒序。
func creditGrants(accounts []packageAccountRow) []creditGrant {
	now := time.Now()
	out := make([]creditGrant, 0)

	for i := range accounts {
		for j := range accounts[i].Packages {
			pkg := &accounts[i].Packages[j]
			out = append(out, creditGrant{
				UID: accounts[i].UID, Nickname: accounts[i].Nickname, Realm: accounts[i].Realm,
				Name: pkg.Name, Source: inferGrantSource(pkg), Inferred: true,
				Size: pkg.Size, Remain: pkg.Remain, CreatedAt: pkg.CreatedAt,
				ExpiresAt: pkg.ExpiresAt, Status: grantStatus(pkg, now),
			})
		}
	}

	slices.SortStableFunc(out, func(a, b creditGrant) int { return cmp.Compare(b.CreatedAt, a.CreatedAt) })

	return out
}

// inferGrantSource 按包名/编码推断来源；上游没给来源字段，故明确标注推断。
func inferGrantSource(pkg *upstream.CreditPackage) string {
	text := strings.ToLower(pkg.Name + " " + pkg.PackageCode + " " + pkg.SubProductName)

	for _, rule := range grantSourceRules {
		for _, needle := range rule.needles {
			if strings.Contains(text, needle) {
				return rule.source
			}
		}
	}

	if pkg.Size >= largePackageMinSize {
		return "首次授权（按面额推断）"
	}

	return "未知"
}

var grantSourceRules = []struct {
	source  string
	needles []string
}{
	{"签到/连登", []string{"签到", "checkin", "streak"}},
	{"活动奖励", []string{"活动", "运营", "裂变", "拉新"}},
	{"首次授权", []string{"首登", "首次", "授权", "新手"}},
}

// grantStatus 按剩余量与到期时间归类；"生效中"无法从上游快照判断，不编造。
func grantStatus(pkg *upstream.CreditPackage, now time.Time) string {
	if pkg.ExpiresAt > 0 && pkg.ExpiresAt < now.UnixMilli() {
		return "已过期"
	}

	if pkg.Remain <= 0 {
		return "已用完"
	}

	if pkg.ExpiresAt == 0 {
		return "可用（不过期）"
	}

	return "可用"
}

// remainingRow 单账号×模型的剩余用量估算（issue #101）。
type remainingRow struct {
	UID         string  `json:"uid"`
	Nickname    string  `json:"nickname"`
	Realm       string  `json:"realm"`
	Model       string  `json:"model"`
	Used        int64   `json:"used"`
	Budget      float64 `json:"budget"`
	Remaining   float64 `json:"remaining"`
	WindowStart string  `json:"window_start,omitempty"`
	Cooling     bool    `json:"cooling"`
	Estimated   bool    `json:"estimated"`
	Samples     int     `json:"samples"`
}

// usageRemaining 只读剩余用量估算；短 TTL + ETag（issue #101）。
func (p *Panel) usageRemaining(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Quota == nil || p.cfg.Usage == nil {
		writeErr(w, http.StatusNotImplemented, "quota store or usage recorder not available")
		return
	}

	now := time.Now()
	rows, budgets, events := p.remainingRows(now)
	etag := remainingETag(rows, events)
	w.Header().Set("ETag", etag)

	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"generated_at": now, "ttl_sec": 5, "rows": rows, "budgets": budgets,
	})
}

// remainingRows 汇总账号×模型的已用/预算/窗口；同一 24h 窗口内按 resetAt-24h 起算。
func (p *Panel) remainingRows(now time.Time) ([]remainingRow, map[string]quota.Budget, int) {
	budgets := p.cfg.Quota.Budgets()
	events := p.cfg.Quota.Events()
	y, m, d := now.Date()
	daily := p.cfg.Usage.TokensByAccountModel(time.Date(y, m, d, 0, 0, 0, 0, now.Location()))
	resetByKey := map[string]time.Time{}
	modelsByUID := map[string]map[string]bool{}

	for _, item := range events {
		key := item.UID + "|" + item.Model
		if item.ResetAt.After(resetByKey[key]) {
			resetByKey[key] = item.ResetAt
		}

		if modelsByUID[item.UID] == nil {
			modelsByUID[item.UID] = map[string]bool{}
		}

		modelsByUID[item.UID][item.Model] = true
	}

	rows := make([]remainingRow, 0)

	all := p.cfg.Pool.List()
	for i := range all {
		acct := &all[i]

		models := modelsByUID[acct.UID]
		if models == nil {
			models = map[string]bool{}
		}

		for model := range daily[acct.UID] {
			models[model] = true
		}

		for model := range models {
			rows = append(rows, p.remainingRow(
				acct, model, daily[acct.UID][model], resetByKey[acct.UID+"|"+model], budgets, now,
			))
		}
	}

	slices.SortStableFunc(rows, func(a, b remainingRow) int { return remainingRowCompare(&a, &b) })

	return rows, budgets, len(events)
}

// remainingRowCompare 剩余量升序；同量按 realm/uid/model 固定次序。
// 行集合来自 map 遍历，缺少次级键时同量行序会随机变化，令 ETag 抖动、304 失效。
func remainingRowCompare(left, right *remainingRow) int {
	if c := cmp.Compare(left.Remaining, right.Remaining); c != 0 {
		return c
	}

	if c := cmp.Compare(left.Realm, right.Realm); c != 0 {
		return c
	}

	if c := cmp.Compare(left.UID, right.UID); c != 0 {
		return c
	}

	return cmp.Compare(left.Model, right.Model)
}

// remainingRow 组装单行；有撞线重置时间时改用同窗口用量。
func (p *Panel) remainingRow(
	acct *pool.Status,
	model string,
	used int64,
	reset time.Time,
	budgets map[string]quota.Budget,
	now time.Time,
) remainingRow {
	row := remainingRow{UID: acct.UID, Nickname: acct.Nickname, Realm: acct.Realm, Model: model, Used: used}
	if !reset.IsZero() {
		row.WindowStart = reset.Add(-24 * time.Hour).Format(time.RFC3339)
		row.Cooling = reset.After(now)
		used = p.cfg.Usage.TokensSince(acct.Realm, acct.UID, model, reset.Add(-24*time.Hour))
		row.Used = used
	}

	budget, ok := budgets[acct.Realm+"|"+model]
	if !ok {
		row.Estimated = true
		row.Budget = float64(used)
		row.Remaining = 0

		return row
	}

	row.Budget = budget.Used
	row.Remaining = max(0, budget.Used-float64(used))
	row.Samples = budget.Samples

	return row
}

// remainingETag 依据行与事件数生成弱 ETag。
func remainingETag(rows []remainingRow, events int) string {
	raw, err := json.Marshal(struct {
		Rows   []remainingRow `json:"rows"`
		Events int            `json:"events"`
	}{Rows: rows, Events: events})
	if err != nil {
		return `"0"`
	}

	sum := sha256.Sum256(raw)

	return fmt.Sprintf(`"%x"`, sum[:8])
}
