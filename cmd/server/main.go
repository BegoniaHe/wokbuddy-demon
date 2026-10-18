// main.go wokbuddy-demon 入口：加载配置、构建 pool、起调度器与 HTTP 服务。
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/BegoniaHe/wokbuddy-demon/internal/agentcfg"
	"github.com/BegoniaHe/wokbuddy-demon/internal/apikey"
	"github.com/BegoniaHe/wokbuddy-demon/internal/atomicwrite"
	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
	"github.com/BegoniaHe/wokbuddy-demon/internal/httpauth"
	"github.com/BegoniaHe/wokbuddy-demon/internal/livecfg"
	"github.com/BegoniaHe/wokbuddy-demon/internal/notify"
	"github.com/BegoniaHe/wokbuddy-demon/internal/outbound"
	"github.com/BegoniaHe/wokbuddy-demon/internal/panel"
	"github.com/BegoniaHe/wokbuddy-demon/internal/pool"
	"github.com/BegoniaHe/wokbuddy-demon/internal/pricing"
	"github.com/BegoniaHe/wokbuddy-demon/internal/quota"
	"github.com/BegoniaHe/wokbuddy-demon/internal/redisstore"
	"github.com/BegoniaHe/wokbuddy-demon/internal/reqlog"
	"github.com/BegoniaHe/wokbuddy-demon/internal/scheduler"
	"github.com/BegoniaHe/wokbuddy-demon/internal/server"
	"github.com/BegoniaHe/wokbuddy-demon/internal/session"
	"github.com/BegoniaHe/wokbuddy-demon/internal/upstream"
	"github.com/BegoniaHe/wokbuddy-demon/internal/usage"
)

// appVersion 构建标识：编译时经 -ldflags -X 注入（缺省 "dev"），透出到 /panel/api/overview。
// 本项目滚动更新、无版本号——这里只是运行时自述用的构建串，不是可比的发布版本。
var appVersion = "dev"

// usagePathFor 由 state 文件路径推出用量文件路径：同目录、文件名 usage.json。
// 这样 config 里改 state_file 时用量数据跟着走，不需要额外配置项。
func usagePathFor(stateFile string) string { return stateSibling(stateFile, "usage.json") }

// stateSibling 返回与 state 文件同目录的指定文件名路径（相对路径场景回落当前目录）。
// usage.json（用量记录）与 output_probes.json（模型上限探测）共用本规则。
func stateSibling(stateFile, name string) string {
	dir := filepath.Dir(stateFile)
	if dir == "" || dir == "." {
		return name
	}

	return filepath.Join(dir, name)
}

// preflightAPIKey 在加载前替换公开占位密钥；无法写回时返回仅供本进程使用的
// 内存随机 key（重启后需重新设置），避免把 test_key 带进运行时。
func preflightAPIKey(path string) (memoryKey string) {
	_, replaced, err := ReplacePlaceholderAPIKey(path)
	if err == nil && replaced {
		log.Printf("config %s 使用公开占位 api_key，已替换为随机密钥（记录在该文件里，可自行修改）", path)
		return ""
	}

	if err == nil || errors.Is(err, fs.ErrNotExist) {
		return ""
	}

	key, kerr := randomAPIKey()
	if kerr != nil {
		return ""
	}

	log.Printf("WARN: 无法写回 config %s（%v）；本次进程临时使用随机 api_key（重启后需重新设置）", path, err)

	return key
}

// loadConfig 加载配置，处理首次运行自动生成推荐配置的兜底：
//   - 文件不存在 → 落一份推荐配置（含随机 api_key）再加载；
//   - 生成失败（目录只读等）→ 退回纯默认 + env（旧行为兜底），不阻塞启动。
//
// errors.Is 才能看穿 Load 里 fmt.Errorf("%w") 的包装；os.IsNotExist 不行。
func loadConfig(path string) (*Config, error) {
	memoryKey := preflightAPIKey(path)

	cfg, err := Load(path)
	if err == nil {
		if memoryKey != "" && cfg.APIKey == placeholderAPIKey {
			cfg.APIKey = memoryKey
		}

		return cfg, nil
	}

	if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	// 首次运行：目录下没有配置 → 自动落一份推荐配置再加载。
	// 双击 exe / 裸跑 docker 即开，无需先手工复制样例。
	if _, werr := WriteDefault(path); werr == nil {
		log.Printf("config %s 不存在，已生成推荐配置（api_key 记录在该文件里，可自行修改）", path)

		if cfg, err = Load(path); err == nil {
			return cfg, nil
		}
	}
	// 生成失败：退回纯默认 + env（旧行为兜底），不阻塞启动。
	log.Printf("config %s not found (auto-generate failed), using defaults+env: %v", path, err)

	return Load("")
}

func main() {
	cfgPath := flag.String("config", "config.json", "配置文件路径（默认当前目录 config.json；不存在时自动生成推荐配置）")

	flag.Parse()

	cfg, auths, err := loadConfigAndAuths(*cfgPath)
	if err != nil {
		log.Fatalf("%v", err)
	}

	// 可信反代网段：解析一次，供网关与面板共用。配置已在 Load 阶段校验。
	trustedProxies, err := httpauth.ParseTrustedProxies(cfg.Server.TrustedProxies)
	if err != nil {
		log.Fatalf("parse trusted_proxies: %v", err)
	}

	state, store, sessRouter, redisMode, sessCount := setupPool(cfg, auths)
	// 停机序：先 pool.Close()（最后一次 Flush → SaveState 已提交到 store），
	// 再 store.Close() 排空在途异步写（最后一笔 Redis 镜像必须写完才关连接）。
	defer func() {
		state.Close()
		_ = store.Close()
	}()

	if sessRouter != nil {
		defer sessRouter.StopGC()
	}

	client, notifier, sch := setupUpstream(cfg, state)
	defer notifier.Close()

	obs := setupObservability(cfg, state)
	defer obs.usage.Stop()
	defer obs.keys.Stop()
	defer obs.reqlog.Close()

	panel2 := newPanel(cfg, *cfgPath, state, client, sch, redisMode, sessCount, obs, trustedProxies)
	// 成长任务队列每日自动执行（与「执行全部待办」同管线）：Sequential 族零点解锁后
	// 无需手动扫描；hook 返回即启动（异步执行），已在跑时内部跳过。
	sch.SetGrowthHook(panel2.RunGrowthQueueOnce)
	log.SetOutput(io.MultiWriter(os.Stderr, panel2.Logs()))
	server.SetChatLogOutput(io.MultiWriter(os.Stdout, panel2.Logs()))

	handler := buildHandler(&handlerDeps{
		cfg:            cfg,
		pool:           state,
		up:             client,
		keys:           obs.keys,
		session:        sessRouter,
		stickyCount:    sessCount,
		redisMode:      redisMode,
		panel:          panel2,
		live:           obs.live,
		usage:          obs.usage,
		requestLog:     obs.reqlog,
		notifier:       notifier,
		trustedProxies: trustedProxies,
	})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	startPricingRefresh(ctx, cfg, obs.pricing)
	serve(ctx, cfg, handler, state, client, sch)
}

// loadConfigAndAuths 加载配置与账号目录；任一失败都带上可定位的上下文。
func loadConfigAndAuths(path string) (*Config, []*auth.Auth, error) {
	cfg, err := loadConfig(path)
	if err != nil {
		return nil, nil, fmt.Errorf("load config: %w", err)
	}

	auths, err := auth.LoadDir(cfg.AuthDir)
	if err != nil {
		return nil, nil, fmt.Errorf("load auths: %w", err)
	}

	log.Printf("loaded %d account(s) from %s", len(auths), cfg.AuthDir)

	return cfg, auths, nil
}

// setupPool 装配账号池、Redis 镜像与会话粘性路由。
func setupPool(cfg *Config, auths []*auth.Auth) (*pool.Pool, redisstore.Store, *session.Router, string, func() int) {
	// redisstore：未配置/连接失败 → Noop（纯内存模式，一切功能照常）。
	store := redisstore.New(cfg.Upstash.URL, cfg.Upstash.Token)
	state := pool.New(cfg.StateFile)
	state.SetStore(store)
	state.RestoreFromSnapshot() // 择新恢复：Redis 快照比本地新才采用，否则本地优先
	state.SyncToDir(auths)      // 与 auths 目录对齐：新账号加入、已删除文件账号剔除（状态保留）

	// 熔断器 + 在途上限（含 global 分档）+ 连败降权 + 闲置补偿调优（从 config 注入，
	// 非正值回退默认）。
	state.SetBreaker(cfg.Pool.BreakerThreshold, cfg.BreakerCooldownDur, cfg.BreakerCooldownMaxD)
	state.SetMaxInFlight(cfg.Pool.MaxInFlight)
	state.SetMaxInFlightGlobal(cfg.Pool.MaxInFlightGlobal) // global 域 WAF 风控分档（P1-1）
	state.SetMaxInFlightTotal(cfg.Pool.MaxInFlightTotal)   // 网关级全局在途上限（issue #92）
	applyDailyLimits(state, cfg)
	state.SetDegrade(cfg.Pool.DegradeThreshold, cfg.DegradeCooldownDur, cfg.DegradeCooldownMaxD)
	state.SetSoftRateMax(cfg.SoftRateMaxDur)                 // 软冷却指数退避封顶（soft_rate_max，默认 2h）
	state.SetCostExploreInterval(cfg.CostExploreIntervalDur) // costTier 探索窗口（issue #136，默认 30m；0 关停）
	state.SetCreditFloor(cfg.Pool.CreditFloor)               // 积分保底（默认 0 = 关闭）
	state.SetWeights(cfg.Pool.IdleWeightPerHour, cfg.Pool.IdleWeightMax)
	state.SetPreferExpiring(cfg.Pool.PreferExpiring)

	// 会话粘性路由（可配关闭）。
	sessRouter, redisMode, sessCount := setupSessionRouter(cfg, store, state)

	return state, store, sessRouter, redisMode, sessCount
}

// setupUpstream 装配上游客户端、通知器与调度器，并注入账号路由与倍率读取。
func setupUpstream(cfg *Config, state2 *pool.Pool) (*upstream.Client, *notify.Notifier, *scheduler.Scheduler) {
	client := upstream.New()

	// 积分保底的「收费」兜底判据：接上游模型目录的积分倍率表。本地实测台账无观测
	// 时用它判收费——否则「没学过」恒等于「放行」，高价新模型会把触底号一笔打穿
	// （kimi-k3-1 实案：全池无观测 → 保底全放行 → 两笔打穿并硬冷却到次日 04:00）。
	// 位于 up 装配之后：倍率表由探测下发，闭包每次调用读实时快照。
	state2.SetModelRateOf(func(realm, model string) string { return client.ModelRate(realm, model) })

	configureUpstream(cfg, client)
	configureAccountRouting(state2, client, cfg)

	notifier := notify.New(notify.Config{URL: cfg.Notify.WebhookURL, Events: cfg.Notify.Events})

	sch := scheduler.New(&scheduler.Config{
		Pool:           state2,
		Upstream:       client,
		Notifier:       notifier,
		CheckinHours:   cfg.Schedule.CheckinHours,
		TravelHours:    cfg.Schedule.TravelHours,
		ActivityHours:  cfg.Schedule.ActivityHours,
		KeepaliveHours: cfg.Schedule.KeepaliveHours,
		BlackcatHours:  cfg.Schedule.BlackcatHours,
		GrowthHours:    cfg.Schedule.GrowthHours,
		// 快过期积分优先消耗：签到/余额刷新按此窗口分桶（issue:积分过期）。
		ExpiringSoonWindow: cfg.ExpiringSoonDur,
		CheckinDisabled:    !cfg.Schedule.CheckinEnabled,
		TravelDisabled:     !cfg.Schedule.TravelEnabled,
		ActivityDisabled:   !cfg.Schedule.ActivityEnabled,
		KeepaliveDisabled:  !cfg.Schedule.KeepaliveEnabled,
		BlackcatDisabled:   !cfg.Schedule.BlackcatEnabled,
		GrowthDisabled:     !cfg.Schedule.GrowthEnabled,
		// 保号类四任务是否覆盖禁用账号（缺省 false = 禁用即跳过，保持既有行为）。
		IncludeDisabledInTasks: cfg.Schedule.IncludeDisabledInTasks,
		CheckinAccountDelay:    cfg.CheckinAccountDelayDur,
	})
	logScheduleConfig(cfg)

	return client, notifier, sch
}

// observability 运行期可观测组件（日志镜像、用量、密钥、请求归档、定价与智能体配置）。
type observability struct {
	live    *livecfg.Holder
	usage   *usage.Recorder
	quota   *quota.Store
	keys    *apikey.Store
	reqlog  *reqlog.Recorder
	pricing *pricing.Catalog
	agents  *agentcfg.Manager
}

// newPanel 构造管理面板：所有可热改字段经 live 快照读取，重启项由 cfg 静态持有。
func newPanel(
	cfg *Config, cfgPath string, state *pool.Pool, client4 *upstream.Client, sch *scheduler.Scheduler,
	redisMode string, sessCount func() int, obs *observability, trustedProxies []*net.IPNet,
) *panel.Panel {
	return panel.New(&panel.Config{
		Pool:           state,
		Usage:          obs.usage,
		RequestLog:     obs.reqlog,
		Keys:           obs.keys,
		Upstream:       client4,
		Scheduler:      sch,
		AuthDir:        cfg.AuthDir,
		AuditDir:       stateSibling(cfg.StateFile, "audit"),
		APIKey:         cfg.APIKey,
		TrustedProxies: trustedProxies,
		RedisMode:      redisMode,
		StickyCount:    sessCount,
		Version:        appVersion,
		Live:           obs.live,
		// 模型上限探测数据（scripts/probe_max_tokens.py --panel-out 写入）：
		// 与 state 文件同目录，缺省 data/output_probes.json。
		ProbeFile:      stateSibling(cfg.StateFile, "output_probes.json"),
		ProbeStateFile: stateSibling(cfg.StateFile, "model_probes.json"),
		ConfigPath:     cfgPath,
		ProxySlots:     proxySlotInfos(cfg),
		Pricing:        obs.pricing,
		PricingRate:    cfg.Pricing.ExchangeRate,
		Quota:          obs.quota,
		Agents:         obs.agents,
		LoadConfig: func() (any, error) {
			return Load(cfgPath)
		},
		SaveConfig: func(raw []byte) ([]string, error) {
			return saveConfig(raw, cfgPath, obs.live, state, client4, sch)
		},
		RestoreConfig: func() ([]string, error) {
			return restoreConfig(cfgPath, obs.live, state, client4, sch)
		},
	})
}

// setupObservability 装配日志镜像、用量记录、密钥库、请求归档与定价目录。
func setupObservability(cfg *Config, state8 *pool.Pool) *observability {
	// 管理面板日志镜像：标准 log（stderr）与 chat 表格日志（stdout）双路复制进
	// 面板环形缓冲，供 /panel/api/logs 读取；控制台输出行为完全不变。
	// live 承载可热改字段（api_key/soft_rate/脱敏开关），面板保存配置时在线替换。
	live := livecfg.New(livecfg.Snapshot{
		APIKey:               cfg.APIKey,
		SoftCooldown:         cfg.SoftRateDur,
		SanitizeFingerprints: cfg.Features.SanitizeBlacklistFingerprints,
		RecordClientInfo:     cfg.Logging.RequestClientInfo,
		MultimodalRouting:    cfg.Features.MultimodalRouting,
		LocalWebTools:        cfg.Features.LocalWebTools,
		LocalWebToolRounds:   cfg.Features.LocalWebToolRounds,
		BackgroundNames:      backgroundNameSet(cfg.Models.BackgroundNames),
		BackgroundTarget:     cfg.Models.BackgroundTarget,
	})
	// 用量记录器：与 state 文件同目录，随 state_file 配置一起搬移。
	// datapath 由 state 文件路径推出，避免再加一个配置项。
	usagePath := usagePathFor(cfg.StateFile)
	rec := usage.New(usagePath)
	rec.Start()
	setDailyUsageProvider(state8, rec)
	quotaStore := setupQuota(state8, rec, cfg)
	log.Printf("[usage] 逐请求用量记录已启用: %s (%s)", usagePath, rec.Describe())

	// 命名密钥库：与 state 文件同目录（data/api_keys.json）。面板签发/撤销，
	// 网关 /v1/* 校验（默认 api_key 照旧，命名密钥只作用于 API 面）。
	keyStore := apikey.New(stateSibling(cfg.StateFile, "api_keys.json"))
	keyStore.Start()
	log.Printf("[apikey] 命名密钥已启用: %s（%d 把）", stateSibling(cfg.StateFile, "api_keys.json"), len(keyStore.List()))

	// 请求指标始终启用；JSONL 归档只写脱敏元数据，写盘失败不影响聊天请求。
	requestLog := reqlog.New(reqlog.Config{
		Dir:           stateSibling(cfg.StateFile, "request-logs"),
		Enabled:       cfg.Logging.RequestArchiveEnabled,
		RetentionDays: cfg.Logging.RequestRetentionDays,
		MaxBytes:      int64(cfg.Logging.RequestArchiveMaxMB) << 20,
	})

	rs := requestLog.Snapshot().Archive
	if rs.Enabled {
		log.Printf("[reqlog] 请求指标已启用；JSONL 归档 %s（保留 %d 天，上限 %d MiB）",
			rs.Dir, cfg.Logging.RequestRetentionDays, cfg.Logging.RequestArchiveMaxMB)
	} else {
		log.Printf("[reqlog] 请求指标已启用；JSONL 归档已关闭")
	}

	priceCat := newPricingCatalog(cfg)
	home, _ := os.UserHomeDir()
	agents := agentcfg.New(home, stateSibling(cfg.StateFile, "agent-backups"))

	return &observability{
		live:    live,
		usage:   rec,
		quota:   quotaStore,
		keys:    keyStore,
		reqlog:  requestLog,
		pricing: priceCat,
		agents:  agents,
	}
}

// serve 启动定时任务与 HTTP 监听，并在收到退出信号后等待在途流排空。
func serve(
	ctx context.Context,
	cfg *Config,
	handler *server.Handler,
	state *pool.Pool,
	upClient *upstream.Client,
	sch *scheduler.Scheduler,
) {
	go sch.Run(ctx)

	sch.StartBalanceRefresh(ctx, cfg.BalanceRefreshInterval)

	// 启动即预热模型积分倍率表：倍率只在 FetchModels/FetchGlobalModelInfos 成功时
	// 填充（两者均懒触发），重启后到首次 /v1/models 或面板模型页被访问之前，
	// ModelRate 恒返回空串——积分保底的目录兜底在这段空窗期内形同虚设，触底号
	// 会被当成「收费未知」放行并打穿（实测：重启后 2 分钟，97 分的账号打收费
	// 模型归零；倍率表当时尚未建立）。
	// 异步执行：不阻塞监听启动；失败仅记日志（下一轮懒触发或本轮重试仍可补上）。
	go warmModelRates(ctx, upClient, state)

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           handler,
		ReadHeaderTimeout: 30 * time.Second,
		// ReadTimeout 覆盖整个请求读取（含 body 上传）：防慢速 body 拖死连接。
		// 请求体已无网关侧上限（max_body_mb 移除）。缺省 300s（issue #100：旧固定
		// 60s 会掐掉大上下文/文件块经反代链的慢速上传，客户端收到
		// 400 "read body: ... i/o timeout"）；server.read_timeout="0" 显式关闭。
		// 改动需重启进程。
		ReadTimeout: cfg.ServerReadTimeoutDur,
		// IdleTimeout keep-alive 空闲连接回收：配合 chat 出站 ctx 传播防连接泄漏堆积。
		// 注意：SSE 流式响应期间连接非空闲，不受此项掐断；不设全局 WriteTimeout
		// （长流式生成合法时长可达数分钟，全局 WriteTimeout 会误杀在途 SSE）。
		IdleTimeout: 120 * time.Second,
	}

	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)

		<-ctx.Done()
		// 两阶段停机（issue #147）：先把 /healthz 置 503 让反代/LB 摘流，
		// 宽限期内 listener 仍受理请求；之后再 Shutdown 等待在途 SSE 排空。
		handler.BeginShutdown()
		state.Flush() // 信号触发：先落盘再做优雅停机

		if grace := time.Duration(cfg.Server.RequestAcceptGraceSeconds) * time.Second; grace > 0 {
			log.Printf("Draining: /healthz -> 503, keeping listener open for %s before shutdown", grace)
			time.Sleep(grace)
		}
		// 优雅停机（issue #19）：等待在途 SSE 长流排空，最多 shutdown_grace_seconds；
		// 超时则记录未排空流数后由进程退出强制结束，不静默丢弃。
		// 注意：Shutdown 关闭 listener 后 ListenAndServe 会立即返回 ErrServerClosed，
		// 但排空等待仍在进行，main 必须等 shutdownDone 再退出，否则在途长流被直接丢弃。
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx),
			time.Duration(cfg.Server.ShutdownGraceSeconds)*time.Second)
		defer cancel()

		_ = srv.Shutdown(shutdownCtx)

		if n := handler.ActiveStreams(); n > 0 {
			log.Printf("WARN: [server] graceful shutdown timed out with %d in-flight SSE stream(s) still active; forcing exit", n)
		}
	}()

	log.Printf("wokbuddy-demon listening on %s (api_key=%v)，管理面板 http://127.0.0.1%s/panel/",
		cfg.Listen, cfg.APIKey != "", panelListenPath(cfg.Listen))

	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Printf("http: %v", err)
		return
	}
	// ErrServerClosed 由 Shutdown 触发：等排空 goroutine 收尾后再退出。
	<-shutdownDone
	log.Printf("bye")
}

// warmModelRates 启动预热各域模型积分倍率表（供积分保底的目录兜底判定）。
//
// 为什么需要：倍率表只在 FetchModels（CN）/ FetchGlobalModelInfos（global）成功时
// 填充，两者都是懒触发（被 /v1/models 或面板模型页访问才跑）。重启后到首次触发
// 之间的空窗期里 ModelRate 恒返回空串，保底的目录兜底判不出收费，触底号会被
// 当成「收费未知」放行并打穿（实测：重启后 2 分钟，97 分的账号打收费模型归零）。
//
// 失败处理：单域失败只记 WARN（不阻塞、不致命——后续懒触发仍会补上）；global 域
// 仅在其路由开关开启时预热（逃生门关锁时按 CN 处理，无需探测）。
func warmModelRates(ctx context.Context, client *upstream.Client, state7 *pool.Pool) {
	// 预热不得拖住进程退出：ctx 取消（SIGINT/SIGTERM）时立刻放弃剩余域。
	if ctx.Err() != nil {
		return
	}
	// CN：有可用 CN 账号才拉（与面板 models 同口径，避免无谓上游调用）。
	if uids := state7.AvailableUIDsForRealm("cn"); len(uids) > 0 {
		if a := state7.AuthByUID(uids[0]); a != nil {
			if _, err := client.FetchModels(a); err != nil {
				log.Printf("WARN: [upstream] warm model rates (cn): %v", err)
			} else {
				log.Printf("[upstream] warm model rates: cn ok")
			}
		}
	}
	// global：独立目录端点（workbuddy.ai），倍率按 "global" 域键存储。
	// 仅在其路由开关开启时预热（逃生门关锁时按 CN 处理，无需探测）。
	if client.GlobalEnabled && ctx.Err() == nil {
		warmGlobalModelRates(client, state7)
	}
}

// warmGlobalModelRates 预热 global 域倍率表：有可用 global 账号才拉，按结果条数
// 判断是否拿到目录（FetchGlobalModelInfos 无错误返回，内部负缓存自行节流）。
func warmGlobalModelRates(client2 *upstream.Client, state4 *pool.Pool) {
	uids := state4.AvailableUIDsForRealm("global")
	if len(uids) == 0 {
		return
	}

	a := state4.AuthByUID(uids[0])
	if a == nil {
		return
	}

	if infos := client2.FetchGlobalModelInfos(a); len(infos) == 0 {
		log.Printf("WARN: [upstream] warm model rates (global): empty model list")
	} else {
		log.Printf("[upstream] warm model rates: global ok (%d models)", len(infos))
	}
}

// setupSessionRouter 按配置装配会话粘性路由，并返回 redis 模式串与粘性计数闭包。
// 关闭时不创建路由（返回 nil），redisMode 仍按 store 类型判定。
func setupSessionRouter(cfg *Config, store redisstore.Store, state5 *pool.Pool) (*session.Router, string, func() int) {
	redisMode := "noop"
	if _, ok := store.(redisstore.Noop); !ok {
		redisMode = "upstash"
	}

	var sessRouter *session.Router
	if cfg.SessionSticky.Enabled {
		sessRouter = session.New(session.Config{
			TTL:        cfg.SessionTTL,
			GCInterval: cfg.SessionGCInterval,
			Store:      store,
			Available:  state5.AvailableUIDs,
			// realm 感知闭包：带前缀模型名按 realm 过滤可用账号（跨 realm 不泄漏）；
			// 裸名走 cn（现状零回归）。闭包内部 resolveModel 剥前缀，再按 realm 过滤。
			AvailableForModel: realmAwareAvailableForModel(state5),
			LowLoad:           realmAwareStickyLowLoad(state5),
		})
		sessRouter.LoadFromStore() // 启动时从 Redis 恢复粘性（读操作仅此处）
		sessRouter.StartGC()
	}

	count := func() int {
		if sessRouter != nil {
			return sessRouter.Count()
		}

		return 0
	}

	return sessRouter, redisMode, count
}

// configureUpstream 把 config 的 HTTP 超时、脱敏开关、UA / 归属头、global 路由与
// model.json 缓存路径注入出站客户端。装配期一次性捕获，改动需重启。
func configureUpstream(cfg *Config, client *upstream.Client) {
	// 短 RPC 总时长上限（refresh/checkin/balance/FetchModels），语义不变。
	client.HTTP.Timeout = time.Duration(cfg.Upstream.TimeoutSeconds) * time.Second
	// 聊天 SSE 首字节前（响应头）上限：cfg 已 normalize（缺省回落 timeout_seconds）。
	client.HeaderTimeout = time.Duration(cfg.Upstream.HeaderTimeoutSeconds) * time.Second
	if tr, ok := client.ChatHTTP.Transport.(*http.Transport); ok {
		tr.ResponseHeaderTimeout = client.HeaderTimeout
	}
	// 聊天 SSE 流中空闲上限（S3 空闲监控读取）。
	client.IdleTimeout = time.Duration(cfg.Upstream.IdleTimeoutSeconds) * time.Second
	// 按模型超时覆盖（issue #58）：装配期一次性注入，改后需重启。
	if len(cfg.Upstream.ModelTimeouts) > 0 {
		client.ModelTimeouts = make(map[string]upstream.ModelTimeout, len(cfg.Upstream.ModelTimeouts))
		for name, spec := range cfg.Upstream.ModelTimeouts {
			client.ModelTimeouts[name] = upstream.ModelTimeout{Header: spec.HeaderDur, Idle: spec.IdleDur}
		}
	}

	client.SanitizeFingerprints.Store(cfg.Features.SanitizeBlacklistFingerprints)
	// 出站 UA 与归属头（issue #42 + 上游同步）：
	// UserAgent 非空则完全覆盖；ClientVersion/CliVersion 缺省对齐官方形态；
	// ClientName 非空时 chat 路径注入 X-IDE-* 四头（用量归因对齐官方桌面端）。
	client.UserAgent = cfg.Upstream.UserAgent
	client.ClientVersion = cfg.Upstream.ClientVersion
	client.CliVersion = cfg.Upstream.CliVersion
	client.ClientName = cfg.Upstream.ClientName
	client.DeviceToken = cfg.Upstream.DeviceToken
	client.DeviceTokenFile = cfg.Upstream.DeviceTokenFile
	client.PassthroughIP = cfg.Upstream.PassthroughIP
	// global realm 路由（config global 段）：上游侧开关（第一道闸）+ base 覆盖；
	// auth 侧开关（auth.SetGlobalEnabled）是第二道闸，两者同 config global.enabled。
	client.GlobalEnabled = cfg.Global.Enabled
	client.ChatBaseGlobal = cfg.Global.ChatBase
	client.BillingBaseGlobal = cfg.Global.BillingBase
	auth.SetGlobalEnabled(cfg.Global.Enabled)
	// model.json 本地缓存接线（context_length/max_output_tokens 四级查找链第 3 级）：
	// 数据目录与 state.json 同风格（Docker volume 持久化路径）。首次缺失/损坏自动
	// 回落仓库内嵌种子；models.dev 按需拉取成功后原子写回。
	upstream.SetModelCatalogPath(stateSibling(cfg.StateFile, "model.json"))
	// 出站代理池（issue #15）：非空时按健康轮换，失败代理进入冷却；空 = 直连。
	// URL 合法性已在 config 解析阶段校验（非法即拒绝启动），此处构造理论上不失败。
	if len(cfg.Upstream.Proxies) > 0 {
		proxyPool, err := outbound.New(cfg.Upstream.Proxies)
		if err != nil {
			log.Printf("WARN: [server] 出站代理配置无效，回退直连: %v", err)
		} else {
			client.SetDialer(proxyPool.DialContext)
			log.Printf("出站代理池已启用：%d 个代理（失败自动冷却轮换）", proxyPool.Len())
		}
	}
}

// logScheduleConfig 启动日志：逐项打印排程启用态与小时。纯日志，无副作用。
func logScheduleConfig(cfg *Config) {
	logScheduleTasks(cfg)
	logScheduleLifecycle(cfg)
	logScheduleBalance(cfg)
}

// logScheduleTasks 打印签到 / 旅行 / 活跃上报的启用态。
func logScheduleTasks(cfg *Config) {
	switch {
	case !cfg.Schedule.CheckinEnabled:
		log.Printf("签到已禁用（schedule.checkin_enabled=false）")
	default:
		log.Printf("签到已启用：%v 点（签到 + 余额查询解冻）", cfg.Schedule.CheckinHours)
	}

	switch {
	case !cfg.Schedule.TravelEnabled:
		log.Printf("猫猫旅行已禁用（schedule.travel_enabled=false）")
	default:
		log.Printf("猫猫旅行已启用：%v 点（独立排程：领养 / 派出 / 领奖）", cfg.Schedule.TravelHours)
	}

	switch {
	case !cfg.Schedule.ActivityEnabled:
		log.Printf("活跃上报已禁用（schedule.activity_enabled=false）")
	default:
		log.Printf("活跃上报已启用：%v 点（每日 1 次，点亮连登 + 解锁 first_buddy）", cfg.Schedule.ActivityHours)
	}
}

// logScheduleLifecycle 打印保活 / 夜猫子的启用态。
func logScheduleLifecycle(cfg *Config) {
	if !cfg.Schedule.KeepaliveEnabled {
		log.Printf("token 保活已禁用（schedule.keepalive_enabled=false）")
	} else {
		log.Printf("token 保活已启用：%v 点", cfg.Schedule.KeepaliveHours)
	}

	switch {
	case !cfg.Schedule.BlackcatEnabled:
		log.Printf("夜猫子已禁用（schedule.blackcat_enabled=false）")
	default:
		log.Printf("夜猫子已启用：%v 点（23:00–08:00 窗口 glm-5.2 对话补足）", cfg.Schedule.BlackcatHours)
	}
}

// logScheduleBalance 打印余额后台刷新与保号任务覆盖禁用账号的开关。
func logScheduleBalance(cfg *Config) {
	switch {
	case !cfg.Schedule.BalanceRefreshEnabled:
		log.Printf("余额后台刷新已禁用（schedule.balance_refresh_enabled=false）")
	case cfg.BalanceRefreshInterval > 0:
		log.Printf("余额后台刷新：每 %s（签到时点照常额外刷新）", cfg.BalanceRefreshInterval)
	}

	if cfg.Schedule.IncludeDisabledInTasks {
		log.Printf("保号任务覆盖禁用账号（schedule.include_disabled_in_tasks=true）：禁用号仍签到 / 活跃 / 保活 / 刷新余额，但不参与选号")
	}
}

// panelListenPath 从 listen 地址提取 ":port" 形式，用于启动日志拼面板 URL
// （":7863" 或 "0.0.0.0:7863" → ":7863"；异常输入原样返回）。
func panelListenPath(listen string) string {
	for i := len(listen) - 1; i >= 0; i-- {
		if listen[i] == ':' {
			return listen[i:]
		}
	}

	return listen
}

// saveConfig 面板保存配置：校验 → 落盘 → 热应用 → 返回需重启的字段列表。
//
// 热生效范围（设计取舍）：
//   - api_key / cooldown.soft_rate / features.sanitize_blacklist_fingerprints → livecfg 快照
//   - pool.* → pool.SetBreaker/SetMaxInFlight/SetSoftRateMax/SetWeights/SetCostExploreInterval/SetPreferExpiring/SetCreditFloor
//   - schedule.* → scheduler.Reconfigure/SetBalanceInterval/SetExpiringSoonWindow
//
// 需重启（涉及监听地址、HTTP client 超时、auth_dir 等装配期依赖）：
//   - listen / auth_dir / state_file / upstream.* / upstash.* / session_sticky.*（TTL 类）
//
// 落盘用"先写 tmp 再 rename"原子替换，且优先保留磁盘上的原始 JSON 结构（只改
// 面板表单覆盖到的键），避免把用户手写的注释性字段/未知键洗掉——这里直接整体
// 序列化校验后的配置，未知键在 json.Unmarshal 时已丢失，故先合并原始 map。
func saveConfig(
	raw []byte,
	path string,
	live *livecfg.Holder,
	state6 *pool.Pool,
	client5 *upstream.Client,
	sch *scheduler.Scheduler,
) ([]string, error) {
	configWriteMu.Lock()
	defer configWriteMu.Unlock()

	return saveConfigInternal(raw, path, live, state6, client5, sch, true, true)
}

// configWriteMu 串行化配置写盘。save/restore 都是「读旧文件 → 合并 → 写 tmp →
// rename」的读改写序列；并发请求共用同名 tmp 会交错写入，甚至 rename 出半截 JSON。
var configWriteMu sync.Mutex

// restoreConfig 用 config.json.1 覆盖当前配置。恢复是整体替换（不 merge 当前文件），
// 否则被改坏的未知键会继续残留；同时不再滚动备份，保留同一份 .1 供重复恢复。
func restoreConfig(
	path string,
	live *livecfg.Holder,
	state2 *pool.Pool,
	client3 *upstream.Client,
	sch *scheduler.Scheduler,
) ([]string, error) {
	configWriteMu.Lock()
	defer configWriteMu.Unlock()

	raw, err := os.ReadFile(configBackupPath(path))
	if err != nil {
		return nil, fmt.Errorf("read config backup: %w", err)
	}

	return saveConfigInternal(raw, path, live, state2, client3, sch, false, false)
}

// saveConfigInternal 是保存/恢复的共同管线：校验 → 备份（可选）→ 原子落盘 → 热应用。
// merge=true 时把提交键深合并进当前文件（面板表单只提交它管理的键）；false 时整体替换。
func saveConfigInternal(
	raw []byte, path string, live *livecfg.Holder, state3 *pool.Pool,
	client2 *upstream.Client, sch *scheduler.Scheduler, merge, makeBackup bool,
) ([]string, error) {
	oldRaw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read current config: %w", err)
	}

	merged, oldCfg, newCfg, err := prepareConfigMerge(oldRaw, raw, merge)
	if err != nil {
		return nil, err
	}

	out, err := json.MarshalIndent(merged, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal config: %w", err)
	}

	// 落盘前留一代旧配置（issue #147）。只在校验与序列化成功后备份，避免把无效
	// 提交当作「上一版」；恢复路径跳过备份，保留原 .1。
	if makeBackup {
		if err := backupConfigFile(path, oldRaw); err != nil {
			return nil, err
		}
	}

	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, out, 0o600); err != nil {
		return nil, fmt.Errorf("write config: %w", err)
	}

	if err := replaceConfigFile(tmp, path, out); err != nil {
		return nil, err
	}

	applyRuntimeConfig(newCfg, live, state3, client2, sch)

	return restartRequiredFields(oldCfg, newCfg), nil
}

// prepareConfigMerge 解析旧配置与提交内容，产出待落盘 map、旧配置与新配置。
// 旧配置用于重启字段 diff；文件损坏时回落默认，最多多报重启项。
func prepareConfigMerge(oldRaw, raw []byte, merge bool) (map[string]any, *Config, *Config, error) {
	var incoming map[string]any
	if err := json.Unmarshal(raw, &incoming); err != nil {
		return nil, nil, nil, fmt.Errorf("parse submitted config: %w", err)
	}

	oldCfg, oldErr := parseConfigWithEnv(oldRaw)
	if oldErr != nil {
		oldCfg = Default()
	}

	merged := incoming

	if merge {
		var cur map[string]any
		if err := json.Unmarshal(oldRaw, &cur); err != nil {
			cur = map[string]any{}
		}

		merged = mergeConfigMaps(cur, incoming)
	}

	newCfg, err := parseConfigWithEnv(mergedJSON(merged))
	if err != nil {
		return nil, nil, nil, err
	}

	return merged, oldCfg, newCfg, nil
}

// applyRuntimeConfig 应用可热生效字段（池参数 / 排程 / 密钥 / 脱敏）。
func applyRuntimeConfig(newCfg *Config, live *livecfg.Holder, state9 *pool.Pool, client3 *upstream.Client, sch *scheduler.Scheduler) {
	live.Store(livecfg.Snapshot{
		APIKey:               newCfg.APIKey,
		SoftCooldown:         newCfg.SoftRateDur,
		SanitizeFingerprints: newCfg.Features.SanitizeBlacklistFingerprints,
		RecordClientInfo:     newCfg.Logging.RequestClientInfo,
		MultimodalRouting:    newCfg.Features.MultimodalRouting,
		LocalWebTools:        newCfg.Features.LocalWebTools,
		LocalWebToolRounds:   newCfg.Features.LocalWebToolRounds,
		BackgroundNames:      backgroundNameSet(newCfg.Models.BackgroundNames),
		BackgroundTarget:     newCfg.Models.BackgroundTarget,
	})
	client3.SanitizeFingerprints.Store(newCfg.Features.SanitizeBlacklistFingerprints)
	state9.SetBreaker(newCfg.Pool.BreakerThreshold, newCfg.BreakerCooldownDur, newCfg.BreakerCooldownMaxD)
	state9.SetMaxInFlight(newCfg.Pool.MaxInFlight)
	state9.SetMaxInFlightGlobal(newCfg.Pool.MaxInFlightGlobal)
	state9.SetMaxInFlightTotal(newCfg.Pool.MaxInFlightTotal)
	applyDailyLimits(state9, newCfg)
	state9.SetDegrade(newCfg.Pool.DegradeThreshold, newCfg.DegradeCooldownDur, newCfg.DegradeCooldownMaxD)
	state9.SetSoftRateMax(newCfg.SoftRateMaxDur)
	state9.SetCostExploreInterval(newCfg.CostExploreIntervalDur) // costTier 探索窗口热生效（0 关停）
	state9.SetCreditFloor(newCfg.Pool.CreditFloor)               // 积分保底热生效（0 = 关闭）
	state9.SetWeights(newCfg.Pool.IdleWeightPerHour, newCfg.Pool.IdleWeightMax)
	state9.SetPreferExpiring(newCfg.Pool.PreferExpiring)
	sch.SetExpiringSoonWindow(newCfg.ExpiringSoonDur)
	sch.Reconfigure(
		newCfg.Schedule.CheckinHours, newCfg.Schedule.TravelHours,
		newCfg.Schedule.ActivityHours, newCfg.Schedule.KeepaliveHours, newCfg.Schedule.BlackcatHours,
		newCfg.Schedule.GrowthHours,
		!newCfg.Schedule.CheckinEnabled, !newCfg.Schedule.TravelEnabled,
		!newCfg.Schedule.ActivityEnabled, !newCfg.Schedule.KeepaliveEnabled, !newCfg.Schedule.BlackcatEnabled,
		!newCfg.Schedule.GrowthEnabled,
	)
	sch.SetBalanceInterval(newCfg.BalanceRefreshInterval)
	sch.SetIncludeDisabledInTasks(newCfg.Schedule.IncludeDisabledInTasks)
	sch.SetCheckinAccountDelay(newCfg.CheckinAccountDelayDur)
}

// configBackupPath 单代配置备份路径（只留上一版，KISS）。
func configBackupPath(path string) string { return path + ".1" }

// backupConfigFile 把旧配置写入 config.json.1（0600，含 api_key，不得进版本库）。
func backupConfigFile(path string, raw []byte) error {
	backup := configBackupPath(path)

	tmp := backup + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return fmt.Errorf("write config backup: %w", err)
	}

	if err := atomicwrite.Rename(tmp, backup); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("replace config backup: %w", err)
	}

	return nil
}

// replaceConfigFile 用 tmp 原子替换 path；单文件 Docker bind mount 无法被 rename
// 覆盖（Linux 返回 EBUSY / "device or resource busy"）时回退为就地写入挂载文件。
func replaceConfigFile(tmp, path string, out []byte) error {
	if err := atomicwrite.Rename(tmp, path); err != nil {
		// A single-file Docker bind mount cannot be renamed over its mount
		// target (Linux returns EBUSY / "device or resource busy"). Keep the
		// atomic path for regular files, but update the mounted file in place
		// for this specific deployment shape.
		if !errors.Is(err, syscall.EBUSY) {
			return fmt.Errorf("replace config: %w", err)
		}

		return writeConfigInPlace(tmp, path, out)
	}

	return nil
}

// writeConfigInPlace 就地截断重写挂载的配置文件（bind mount 回退路径）。
// 写失败时保留 tmp（挂载文件已被 O_TRUNC 破坏，tmp 里是完整新内容，可手工恢复）；
// 写成功才清理 tmp。
func writeConfigInPlace(tmp, path string, out []byte) error {
	file, openErr := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0o600)
	if openErr != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("replace config (bind mount fallback): %w", openErr)
	}

	_, writeErr := file.Write(out)
	if writeErr == nil {
		writeErr = file.Sync()
	}

	closeErr := file.Close()

	if writeErr != nil {
		return fmt.Errorf("replace config (bind mount fallback, 完整新内容保留在 %s): %w", tmp, writeErr)
	}

	_ = os.Remove(tmp)

	if closeErr != nil {
		return fmt.Errorf("replace config (bind mount fallback): %w", closeErr)
	}

	return nil
}

// restartFieldPaths 是装配期一次性捕获、修改后必须重启才能生效的字段路径。
// 集中维护：新增装配期字段时只在这里加一行；JSON 路径同时覆盖嵌套结构。
//
// 路径在配置中缺失时新旧取值同为 nil，不会误报；字段存在后自动参与比较。
var restartFieldPaths = []string{
	"listen", "auth_dir", "state_file",
	"server.read_timeout", "server.max_body_bytes",
	"server.request_accept_grace_seconds", "server.shutdown_grace_seconds",
	// CORS 允许列表在 handler 构造期复制，运行期无热更新。
	"server.cors_origins",
	// 可信反代网段在启动期解析为 *net.IPNet，运行期无热更新。
	"server.trusted_proxies",
	// Pricing 目录与刷新协程在启动期创建。
	"pricing.enabled", "pricing.refresh_minutes", "pricing.exchange_rate",
	// HTTP client / 出站身份 / 代理在 configureUpstream 一次性注入。
	"upstream.timeout_seconds", "upstream.header_timeout_seconds", "upstream.idle_timeout_seconds",
	"upstream.keepalive_interval", "upstream.model_timeouts", "upstream.user_agent",
	"upstream.client_version", "upstream.cli_version", "upstream.client_name",
	"upstream.device_token", "upstream.device_token_file", "upstream.passthrough_ip",
	"upstream.rotate_max", "upstream.rotate_budget", "upstream.rotate_status_codes",
	"upstream.proxies",
	// global 路由在 configureUpstream / auth / handler 构造期捕获。
	"global.enabled", "global.chat_base", "global.billing_base",
	// 提示词模式与文件在 handler 构造期解析。
	"prompt.mode", "prompt.file",
	// Redis 客户端在启动期装配。
	"upstash.url", "upstash.token",
	"models.aliases", "notify.webhook_url", "notify.events",
	// 粘性路由器是否创建在装配期决定；TTL 类同样在启动期读取。
	"session_sticky.enabled", "session_sticky.ttl", "session_sticky.gc_interval",
	"logging.request_archive_enabled", "logging.request_retention_days",
	"logging.request_archive_max_mb", "logging.decision_trace", "logging.decision_trace_sample",
}

// restartRequiredFields 返回本次改动中无法热生效、需要重启进程的字段路径。
// 逐字段比较新旧配置，只返回实际变化的项（issue #87），避免面板恒提示全量。
func restartRequiredFields(oldCfg, newCfg *Config) []string {
	oldDoc, newDoc := configDoc(oldCfg), configDoc(newCfg)

	var out []string

	for _, path := range restartFieldPaths {
		if !reflect.DeepEqual(digJSONPath(oldDoc, path), digJSONPath(newDoc, path)) {
			out = append(out, path)
		}
	}

	return out
}

// configDoc 把配置序列化为按 JSON 路径取值的 map（解析后的 time.Duration
// 字段带 json:"-"，不会参与比较）。
func configDoc(c *Config) map[string]any {
	raw, err := json.Marshal(c)
	if err != nil {
		return nil
	}

	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil
	}

	return doc
}

// digJSONPath 按 "a.b.c" 路径取嵌套值；路径不存在返回 nil。
func digJSONPath(doc map[string]any, path string) any {
	var cur any = doc
	for key := range strings.SplitSeq(path, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil
		}

		cur = m[key]
	}

	return cur
}

// mergeConfigMaps 把 incoming 深合并进 cur（原地），返回 cur。
// 对嵌套对象逐键覆盖而不是整体替换：面板表单只提交它管理的键，
// 未提交的兄弟键（含用户手写的未知键）保持原样。
func mergeConfigMaps(cur, incoming map[string]any) map[string]any {
	for strVal, iface := range incoming {
		if inMap, ok := iface.(map[string]any); ok {
			if curMap, ok := cur[strVal].(map[string]any); ok {
				cur[strVal] = mergeConfigMaps(curMap, inMap)
				continue
			}
		}

		cur[strVal] = iface
	}

	return cur
}

// mergedJSON 把合并后的 map 序列化回 JSON（供 ParseConfig 校验）。
func mergedJSON(m map[string]any) []byte {
	b, err := json.Marshal(m)
	if err != nil {
		return []byte("{}")
	}

	return b
}
