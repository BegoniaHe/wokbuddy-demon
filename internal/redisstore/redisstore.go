// Package redisstore 封装 Upstash（Redis）持久化，并提供内存降级（Noop）。
//
// 设计约束：Upstash 走公网 TLS，单次 RTT 可能 50~300ms，因此所有写操作都是
// fire-and-forget（交给单写者 goroutine 串行落地 + 失败仅 debug 日志），读操作只
// 发生在启动时（加载粘性会话镜像、恢复冷却/熔断快照）。内存为主、Redis 为辅。
//
// 单写者（issue #417）：早先每次写起独立 goroutine，完成顺序不受提交顺序约束，
// 旧快照 / 已解绑的绑定可能后落地覆盖新值。现在统一入队、由一个 goroutine 按
// 提交顺序串行执行；Close 仍保证已提交的写全部执行完。
//
// 未配置 url / 连接失败时降级为 Noop：一切功能照常工作（纯内存模式），
// 上层只打一条启动警告日志。
package redisstore

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// keyTTL 粘性会话镜像 + 状态快照的默认 TTL（redis 侧兜底，防脏数据长期滞留）。
const keyTTL = 7 * 24 * time.Hour

// Store 只放本期需要的方法。上下文由实现内部构造（读操作配短超时，写操作 fire-and-forget）。
//
//nolint:interfacebloat // Redis 镜像的读写/生命周期方法同属一个实现，拆开反而增加装配噪音。
type Store interface {
	// SetBind 异步镜像粘性会话绑定（key→uid），带 TTL。
	SetBind(key, uid string, ttl time.Duration)
	// DelBind 异步删除粘性会话绑定。
	DelBind(key string)
	// LoadBinds 全量读取粘性会话绑定（key→uid，key 已剥前缀）；仅在启动时调用（同步）。
	// 供冷启动恢复粘性映射（防重启丢粘性）。
	LoadBinds() map[string]string
	// SaveState 异步写池状态 JSON 快照（与本地 state.json 并存，仅作恢复备份）。
	SaveState(data []byte)
	// LoadState 读池状态快照；仅在启动时调用（同步）。
	LoadState() ([]byte, bool)
	// Close 关停 Store：Upstash 等待已提交的异步写全部执行完再关底层连接
	// （停机语义：最后一笔 Redis 镜像必须写完），之后新提交的写直接丢弃；幂等。
	// Noop 为空操作。进程退出前在 pool.Close() 之后调用。
	Close() error
}

const (
	bindPrefix  = "wb2api:bind:"
	stateKey    = "wb2api:state"
	readTimeout = 3 * time.Second
)

// New 根据 url+token 构建 Store。
//   - url 为空 → Noop（纯内存模式）
//   - url 已是完整 rediss:// URL 则直接 ParseURL；否则用 token 组装 rediss://default:token@host:6379
//   - Ping 失败 → Noop + 启动警告（硬性降级要求：不因 Redis 不可用而失败）
func New(url, token string) Store {
	if url == "" {
		log.Printf("[redisstore] upstash 未配置，进入纯内存模式（Noop 降级）")
		return Noop{}
	}

	full := normalizeURL(url, token)

	opt, err := redis.ParseURL(full)
	if err != nil {
		log.Printf("[redisstore] 警告: redis 连接串解析失败 (%v)，降级 Noop", err)
		return Noop{}
	}

	opt.ReadTimeout = readTimeout
	opt.WriteTimeout = readTimeout
	client := redis.NewClient(opt)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	if err := client.Ping(ctx).Err(); err != nil {
		log.Printf("[redisstore] 警告: upstash 连接失败 (%v)，降级 Noop（纯内存模式）", err)

		_ = client.Close()

		return Noop{}
	}

	log.Printf("[redisstore] upstash 已连接 (addr=%s)", opt.Addr)

	u := &Upstash{client: client}
	u.ensureInit()

	return u
}

// normalizeURL 把 url+token 归一化为可直接 ParseURL 的完整 rediss:// URL。
// 若 url 本身已含 scheme（rediss://、redis://、https://...upstash.io 等）：
//   - rediss:// 或 redis:// 原样返回（已是完整连接串）
//   - 其余（如 https://xxx.upstash.io）剥掉 "://" 前缀只取 host，再按
//     "rediss://default:<token>@<host>:6379" 组装
func normalizeURL(url, token string) string {
	if len(url) >= 8 && (url[:8] == "rediss:/" || url[:7] == "redis:/") {
		return url
	}

	host := url
	if i := strings.Index(host, "://"); i >= 0 {
		host = host[i+3:]
	}

	return "rediss://default:" + token + "@" + host + ":6379"
}

// Upstash 真实现：redis.Client 封装。
//
// 写模型（issue #417）：所有异步写进入同一条队列，由 runWriter 单个 goroutine 按
// 提交顺序串行落地。这消除了「后提交的先完成、旧快照覆盖新值」的乱序窗口；
// 写仍是 fire-and-forget（调用方不等待），Close 前提交的写保证执行完，Close 后
// 提交的写直接丢弃。
type Upstash struct {
	client *redis.Client
	// mu 保护 queue / stopped：入队顺序即提交顺序（单写者据此串行执行）。
	mu sync.Mutex
	// queue 未执行的写（FIFO）；notify 有新写时唤醒空闲的写者（cap=1 合并唤醒）。
	queue  []func()
	notify chan struct{}
	// stopped Close 后置位：此后提交的写直接丢弃，写者排空队列后退出。
	stopped bool
	// initOnce 保证 queue/notify 初始化与写者启动只发生一次（并发 goWrite/Close 安全）。
	initOnce sync.Once
	// wg 已提交未完成的写（Close 排空用）。
	wg sync.WaitGroup
}

// goWrite 以 fire-and-forget 方式登记一次写：入队后由单写者串行执行。
// Close 前提交的写必然执行（停机镜像完整性），Close 后提交的写直接丢弃。
func (u *Upstash) goWrite(fn func()) {
	u.ensureInit()
	u.mu.Lock()

	if u.stopped {
		u.mu.Unlock() // 关停后提交的写：直接丢弃

		return
	}

	u.queue = append(u.queue, fn)
	// 与入队同锁 Add：Close 在同一把锁下置 stopped 并 wg.Wait，Wait 与 Add
	// 不并发（否则零计数下并发 Add 属 WaitGroup 误用）。
	u.wg.Add(1)
	u.mu.Unlock()

	// 非阻塞唤醒：缓冲 1 已足够，写者醒来后会把队列取空。
	select {
	case u.notify <- struct{}{}:
	default:
	}
}

// ensureInit 初始化队列与 done 标志并启动写者（幂等）。
// New 与测试零值构造共用同一入口；sync.Once 保证并发首用时不竞争。
func (u *Upstash) ensureInit() {
	u.initOnce.Do(func() {
		u.queue = make([]func(), 0)
		u.notify = make(chan struct{}, 1)

		go u.runWriter()
	})
}

// runWriter 单写者：按入队顺序执行写；队列空且已停止时退出。
func (u *Upstash) runWriter() {
	for {
		u.mu.Lock()

		if len(u.queue) == 0 {
			if u.stopped {
				u.mu.Unlock()

				return
			}

			u.mu.Unlock()
			<-u.notify

			continue
		}

		fn := u.queue[0]
		u.queue[0] = nil // 立即释放闭包引用，避免长队列滞留已执行写
		u.queue = u.queue[1:]
		u.mu.Unlock()

		fn()
		u.wg.Done()
	}
}

// Close 等待已提交的异步写全部执行完毕，再关底层 redis 连接；幂等。
// 之后新提交的写直接丢弃（goWrite 的 stopped 检查）。停机路径在 pool.Close() 后
// 调用：pool 的最后一次 Flush→SaveState 已提交，本方法保证它写完才返回。
func (u *Upstash) Close() error {
	u.ensureInit()
	// 先置 stopped 再等 wg：与 goWrite 的 mu 互斥后，「已提交的写」集合已冻结
	// （此后新提交的直接丢弃），写者会把剩余队列排空，wg.Wait 覆盖全部。
	u.mu.Lock()
	u.stopped = true
	u.mu.Unlock()

	select {
	case u.notify <- struct{}{}:
	default:
	}

	done := make(chan struct{})

	go func() { u.wg.Wait(); close(done) }()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		// 兜底超时（单写上限 5s，10s 富余）：卡死的写不应阻塞进程退出。
		log.Printf("[redisstore] WARN: Close 等待在途写超时，放弃（镜像可能未写完）")
	}

	return u.closeClient()
}

// closeClient 关底层 redis 连接（client 为 nil——测试构造——时跳过）。
func (u *Upstash) closeClient() error {
	if u.client == nil {
		return nil
	}

	if err := u.client.Close(); err != nil {
		return fmt.Errorf("close redis client: %w", err)
	}

	return nil
}

func bindKey(key string) string { return bindPrefix + key }

// SetBind 异步镜像粘性会话绑定。
func (u *Upstash) SetBind(key, uid string, ttl time.Duration) {
	if ttl <= 0 {
		ttl = keyTTL
	}

	u.goWrite(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		if err := u.client.Set(ctx, bindKey(key), uid, ttl).Err(); err != nil {
			log.Printf("[redisstore] debug: SetBind %s: %v", key, err)
		}
	})
}

// DelBind 异步删除粘性会话绑定。
func (u *Upstash) DelBind(key string) {
	u.goWrite(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		if err := u.client.Del(ctx, bindKey(key)).Err(); err != nil {
			log.Printf("[redisstore] debug: DelBind %s: %v", key, err)
		}
	})
}

// SaveState 异步写池状态 JSON 快照。
func (u *Upstash) SaveState(data []byte) {
	u.goWrite(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		if err := u.client.Set(ctx, stateKey, data, keyTTL).Err(); err != nil {
			log.Printf("[redisstore] debug: SaveState: %v", err)
		}
	})
}

// LoadState 同步读池状态快照。
func (u *Upstash) LoadState() ([]byte, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), readTimeout)
	defer cancel()

	v, err := u.client.Get(ctx, stateKey).Bytes()
	if err != nil {
		return nil, false
	}

	return v, true
}

// bindMGetBatchSize 每批 MGET 的 key 数：把 N+1 次 GET 收敛为 SCAN + 常数次往返。
const bindMGetBatchSize = 100

// bindScanPage 单页 SCAN 的可注入形态（测试用 fake 替代真实 Redis）。
type bindScanPage func(ctx context.Context, cursor uint64) (keys []string, next uint64, err error)

// bindMGet 批量取值的可注入形态（测试用 fake 替代真实 Redis）。
type bindMGet func(ctx context.Context, keys ...string) ([]any, error)

// scanBindKeys 用 SCAN 只收集 key（不读值），直到游标归零；失败时返回已发现的部分。
func scanBindKeys(ctx context.Context, scan bindScanPage) ([]string, error) {
	var keys []string

	var cursor uint64

	for {
		batch, next, err := scan(ctx, cursor)
		if err != nil {
			return keys, err
		}

		keys = append(keys, batch...)
		cursor = next

		if cursor == 0 {
			return keys, nil
		}
	}
}

// readBindValues 按批 MGET 已发现的 key：空值跳过，key 剥前缀后写入结果。
// 单批失败记 WARN 并继续后续批次；complete=false 表示至少一批未成功读取。
func readBindValues(ctx context.Context, keys []string, mget bindMGet) (map[string]string, bool) {
	out := make(map[string]string, len(keys))
	complete := true

	for start := 0; start < len(keys); start += bindMGetBatchSize {
		end := min(start+bindMGetBatchSize, len(keys))

		values, err := mget(ctx, keys[start:end]...)
		if err != nil {
			complete = false

			log.Printf("WARN: [redisstore] LoadBinds MGET batch failed discovered=%d restored=%d: %v",
				len(keys), len(out), err)

			continue
		}

		if len(values) != end-start {
			complete = false

			log.Printf("WARN: [redisstore] LoadBinds MGET truncated discovered=%d restored=%d batch=%d got=%d",
				len(keys), len(out), end-start, len(values))
		}

		for i, raw := range values {
			if i >= end-start {
				break
			}

			value, ok := raw.(string)
			if !ok || value == "" {
				continue
			}

			out[strings.TrimPrefix(keys[start+i], bindPrefix)] = value
		}
	}

	return out, complete
}

// loadBinds 是 LoadBinds 的可注入主体：scan/mget 由生产实现或测试 fake 提供。
// 失败/不完整只记 WARN（带 discovered/restored 计数，不打印 key 或值），
// 成功恢复的汇总日志仍由 session.LoadFromStore 统一输出，避免重复刷屏。
func loadBinds(ctx context.Context, scan bindScanPage, mget bindMGet) map[string]string {
	keys, scanErr := scanBindKeys(ctx, scan)

	out, complete := readBindValues(ctx, keys, mget)

	if scanErr != nil {
		log.Printf("WARN: [redisstore] LoadBinds SCAN failed discovered=%d restored=%d: %v",
			len(keys), len(out), scanErr)
	} else if !complete {
		log.Printf("WARN: [redisstore] LoadBinds incomplete discovered=%d restored=%d", len(keys), len(out))
	}

	return out
}

// LoadBinds 全量读取粘性会话绑定（SCAN bind:* 前缀 + 分批 MGET）。
func (u *Upstash) LoadBinds() map[string]string {
	ctx, cancel := context.WithTimeout(context.Background(), readTimeout)
	defer cancel()

	return loadBinds(ctx,
		func(ctx context.Context, cursor uint64) ([]string, uint64, error) {
			return u.client.Scan(ctx, cursor, bindPrefix+"*", 200).Result()
		},
		func(ctx context.Context, keys ...string) ([]any, error) {
			return u.client.MGet(ctx, keys...).Result()
		},
	)
}

// Noop 纯内存降级：所有方法空实现。
type Noop struct{}

func (Noop) SetBind(string, string, time.Duration) {}
func (Noop) DelBind(string)                        {}
func (Noop) LoadBinds() map[string]string          { return nil }
func (Noop) SaveState([]byte)                      {}
func (Noop) LoadState() ([]byte, bool)             { return nil, false }
func (Noop) Close() error                          { return nil }
