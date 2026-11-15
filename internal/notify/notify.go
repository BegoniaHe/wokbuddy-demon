// Package notify 最小 webhook 告警出口（issue #55）：账号/池级关键事件异步 POST
// JSON，同事件同账号在窗口内去重；发送失败只记日志，不影响主请求与定时任务。
// 首版只支持单一 HTTP(S) webhook，不做告警平台、静默规则或第三方凭证存储。
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"maps"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/BegoniaHe/wokbuddy-demon/internal/logfmt"
)

// 事件名（稳定契约；配置 notify.events 使用这些值）。
const (
	EventSessionDead     = "session_dead"
	EventPoolUnavailable = "pool_unavailable"
	EventCreditLow       = "credit_low"
	EventWAFIPBlocked    = "waf_ip_blocked"
	EventTaskFailed      = "task_failed"
)

var defaultEvents = []string{
	EventSessionDead,
	EventPoolUnavailable,
	EventCreditLow,
	EventWAFIPBlocked,
	EventTaskFailed,
}

var errWebhookURLHTTPS = errors.New("webhook_url 必须是 http(s) 绝对地址")

// ValidEvent 报告事件名是否受支持（配置校验用）。
func ValidEvent(name string) bool {
	switch name {
	case EventSessionDead, EventPoolUnavailable, EventCreditLow, EventWAFIPBlocked, EventTaskFailed:
		return true
	default:
		return false
	}
}

// ValidateURL 校验 webhook 地址：空 = 关闭；非空必须是 http/https 绝对 URL。
func ValidateURL(raw string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}

	url2, err := url.Parse(raw)
	if err != nil {
		// 不回显原始 URL：webhook 地址本身通常就是 bearer token。
		if uerr, ok := errors.AsType[*url.Error](err); ok {
			err = uerr.Err
		}

		return fmt.Errorf("webhook_url 必须是 http(s) 绝对地址: %w", err)
	}

	if (url2.Scheme != "http" && url2.Scheme != "https") || url2.Host == "" {
		return errWebhookURLHTTPS
	}

	return nil
}

// Event 一条告警；UID 传完整值，序列化时截为 uid8（不发送完整 UID）。
type Event struct {
	Event    string         `json:"event"`
	Time     time.Time      `json:"time"`
	UID      string         `json:"uid,omitempty"`
	Nickname string         `json:"nickname,omitempty"`
	Realm    string         `json:"realm,omitempty"`
	Summary  string         `json:"summary,omitempty"`
	Detail   map[string]any `json:"detail,omitempty"`
}

// payload 线上 JSON 形态（uid 截断、时间 RFC3339）。
type payload struct {
	// dedupeKey 去重键（event+uid）；不序列化，仅用于发送失败时释放去重窗口。
	dedupeKey string
	Event     string         `json:"event"`
	Time      string         `json:"time"`
	UID       string         `json:"uid,omitempty"`
	Nickname  string         `json:"nickname,omitempty"`
	Realm     string         `json:"realm,omitempty"`
	Summary   string         `json:"summary,omitempty"`
	Detail    map[string]any `json:"detail,omitempty"`
}

// Config 构造参数；零值 = 关闭。
type Config struct {
	URL    string
	Events []string // 空 = 默认关键事件
	// Timeout / DedupeWindow / Now / HTTPClient 仅供测试注入，生产留零值。
	Timeout      time.Duration
	DedupeWindow time.Duration
	Now          func() time.Time
	HTTPClient   *http.Client
}

const (
	defaultTimeout      = 5 * time.Second
	defaultDedupeWindow = 10 * time.Minute
	queueSize           = 64
	// closeDrainBudget 关停时排空队列的总预算：worker 串行发送、每条自带 timeout，
	// 队列满时最坏 64×5s，不能让它阻塞进程退出。
	closeDrainBudget = 2 * time.Second
)

// Sender 告警投递接口（测试可注入 fake）。
type Sender interface {
	Notify(event *Event)
}

// Notifier 异步 webhook 发送器；URL 为空时所有方法为空操作（nil 也可安全调用）。
type Notifier struct {
	url     string
	events  map[string]bool
	client  *http.Client
	timeout time.Duration
	now     func() time.Time
	window  time.Duration

	queue  chan payload
	done   chan struct{}
	wg     sync.WaitGroup
	once   sync.Once
	closed atomic.Bool // Close 后置位，拒绝新事件入队

	mu   sync.Mutex
	last map[string]time.Time

	// dropped 队列满被丢弃的事件数；lastDropLog 上次打 WARN 的时刻（每分钟一次）。
	// webhook 变慢时丢弃是必然事件，按事件数刷屏没有信息量，运维要的是「丢了多少」。
	dropped     int64
	lastDropLog time.Time
}

// New 构造并启动发送 worker；URL 为空返回禁用实例（不启动 goroutine）。
func New(cfg Config) *Notifier {
	n := &Notifier{
		url:    strings.TrimSpace(cfg.URL),
		events: eventSet(cfg.Events),
		client: cfg.HTTPClient,
		last:   map[string]time.Time{},
	}
	n.applyTimeConfig(cfg)
	n.applyClientConfig(cfg)

	if n.url == "" {
		return n
	}

	n.queue = make(chan payload, queueSize)
	n.done = make(chan struct{})

	n.wg.Add(1)
	go n.run()

	return n
}

// eventSet 构建订阅集合；空 = 默认关键事件。
func eventSet(events []string) map[string]bool {
	out := make(map[string]bool, len(events))
	for _, e := range events {
		out[e] = true
	}

	if len(out) == 0 {
		for _, e := range defaultEvents {
			out[e] = true
		}
	}

	return out
}

// applyTimeConfig 补齐时钟与去重窗口。
func (n *Notifier) applyTimeConfig(cfg Config) {
	n.now = cfg.Now
	if n.now == nil {
		n.now = time.Now
	}

	n.window = cfg.DedupeWindow
	if n.window <= 0 {
		n.window = defaultDedupeWindow
	}
}

// applyClientConfig 补齐发送超时与 HTTP client。
func (n *Notifier) applyClientConfig(cfg Config) {
	n.timeout = cfg.Timeout
	if n.timeout <= 0 && n.client != nil {
		n.timeout = n.client.Timeout
	}

	if n.timeout <= 0 {
		n.timeout = defaultTimeout
	}

	if n.client == nil {
		// 不跟随重定向：webhook 载荷（uid8/nickname/realm）不应被 307/308 转发到
		// 第三方，也不应因重定向降到 http。
		n.client = &http.Client{
			Timeout: n.timeout,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
	}
}

// Notify 投递一条事件：未订阅/去重窗口内重复时丢弃；队列满时丢新事件并计数。
func (n *Notifier) Notify(event *Event) {
	if n == nil || event == nil || n.url == "" || n.closed.Load() || !n.events[event.Event] {
		return
	}

	key := event.Event + "\x00" + event.UID
	if !n.allow(key) {
		return
	}

	payload2 := payload{
		dedupeKey: key,
		Event:     event.Event,
		Time:      n.now().UTC().Format(time.RFC3339),
		Nickname:  event.Nickname,
		Realm:     event.Realm,
		Summary:   event.Summary,
		// 深拷贝：序列化发生在 worker 协程，调用方（请求/调度协程）只要复用或事后
		// 修改同一个 map 就是跨协程读写。三个调用点当前都传字面量，属潜伏缺陷。
		Detail: maps.Clone(event.Detail),
	}
	if event.UID != "" {
		payload2.UID = logfmt.UID8(event.UID)
	}

	select {
	case n.queue <- payload2:
	default:
		// 被丢弃的事件不能占住去重窗口：否则同一事件 + 账号在 window 内再也不重试，
		// 真实告警被静默压制（队列满在 webhook 变慢时必然发生）。
		n.forget(key)
		n.noteDrop(payload2.Event, payload2.UID)
	}
}

// allow 做同事件 + 账号的窗口去重。
func (n *Notifier) allow(key string) bool {
	now := n.now()
	n.mu.Lock()
	defer n.mu.Unlock()

	if last, ok := n.last[key]; ok && now.Sub(last) < n.window {
		return false
	}

	n.last[key] = now

	return true
}

// forget 撤销一次去重登记（入队失败时调用）。
func (n *Notifier) forget(key string) {
	n.mu.Lock()
	delete(n.last, key)
	n.mu.Unlock()
}

// run 串行发送（单 worker，天然限频；失败不影响下一事件）。
func (n *Notifier) run() {
	defer n.wg.Done()

	for {
		select {
		case p := <-n.queue:
			n.deliver(&p)
		case <-n.done:
			// 关停时排空已入队事件再退出：select 同时就绪会随机选，直接 return
			// 会静默丢掉队列里尚未发送的通知（Close 与 Notify 并发下的竞态）。
			for {
				select {
				case p := <-n.queue:
					n.deliver(&p)
				default:
					return
				}
			}
		}
	}
}

// noteDrop 记录一次队列满丢弃：每分钟最多一条日志 + 累计数 + 账号标识。
func (n *Notifier) noteDrop(event, uid string) {
	n.mu.Lock()
	n.dropped++

	emit := time.Since(n.lastDropLog) >= time.Minute
	if emit {
		n.lastDropLog = time.Now()
	}

	dropped := n.dropped
	n.mu.Unlock()

	if emit {
		log.Printf("WARN: [notify] event=%s uid=%s dropped: queue full（累计 %d 条）",
			event, logfmt.UID8(uid), dropped)
	}
}

// send POST JSON；失败只记事件名，不带 URL（URL 可能内嵌 token）。
// 返回是否投递成功：失败时调用方要释放去重窗口，否则这条告警在窗口内既不会
// 重试也不会再产生新事件（webhook 抖动会吃掉唯一一次机会）。
func (n *Notifier) send(payload2 *payload) bool {
	raw, err := json.Marshal(payload2)
	if err != nil {
		log.Printf("WARN: [notify] event=%s encode failed", payload2.Event)

		return false
	}

	ctx, cancel := context.WithTimeout(context.Background(), n.timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.url, bytes.NewReader(raw))
	if err != nil {
		log.Printf("WARN: [notify] event=%s request build failed", payload2.Event)

		return false
	}

	req.Header.Set("Content-Type", "application/json")

	resp, err := n.client.Do(req)
	if err != nil {
		log.Printf("WARN: [notify] event=%s send failed", payload2.Event)

		return false
	}

	// 读掉少量 body 再关：不排空会让 Transport 无法复用连接，每条告警都要重做
	// TCP + TLS 握手。
	_, _ = io.CopyN(io.Discard, resp.Body, 4<<10)
	_ = resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		log.Printf("WARN: [notify] event=%s send failed: status=%d", payload2.Event, resp.StatusCode)

		return false
	}

	return true
}

// deliver 发送并在失败时释放去重窗口：下一条同事件/同账号的事件可以立刻重发，
// 而不是被窗口静默吃掉（webhook 抖动时 session_dead 这类终态告警只有一次机会）。
func (n *Notifier) deliver(payload2 *payload) {
	if !n.send(payload2) && payload2.dedupeKey != "" {
		n.forget(payload2.dedupeKey)
	}
}

// Close 停止 worker（幂等）：先拒绝新事件，再让 worker 排空已入队事件后退出。
func (n *Notifier) Close() {
	if n == nil || n.done == nil {
		return
	}

	n.once.Do(func() {
		n.closed.Store(true)
		close(n.done)

		// 排空要有总预算：worker 串行发送、每条自带 timeout（默认 5s），队列满时
		// 最坏 64×5s = 320s，而 Close 在进程退出路径上。
		drained := make(chan struct{})
		go func() {
			defer close(drained)

			n.wg.Wait()
		}()

		select {
		case <-drained:
		case <-time.After(closeDrainBudget):
			log.Printf("WARN: [notify] 关停排空超时（%s），放弃剩余通知", closeDrainBudget)
		}
	})
}
