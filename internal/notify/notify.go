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
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
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

// DefaultEvents 返回默认订阅的事件集合。
func DefaultEvents() []string { return append([]string(nil), defaultEvents...) }

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
	u, err := url.Parse(raw)
	if err != nil {
		// 不回显原始 URL：webhook 地址本身通常就是 bearer token。
		var uerr *url.Error
		if errors.As(err, &uerr) {
			err = uerr.Err
		}
		return fmt.Errorf("webhook_url 必须是 http(s) 绝对地址: %w", err)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return errors.New("webhook_url 必须是 http(s) 绝对地址")
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
	Event    string         `json:"event"`
	Time     string         `json:"time"`
	UID      string         `json:"uid,omitempty"`
	Nickname string         `json:"nickname,omitempty"`
	Realm    string         `json:"realm,omitempty"`
	Summary  string         `json:"summary,omitempty"`
	Detail   map[string]any `json:"detail,omitempty"`
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
)

// Sender 告警投递接口（测试可注入 fake）。
type Sender interface {
	Notify(event Event)
}

// Notifier 异步 webhook 发送器；URL 为空时所有方法为空操作（nil 也可安全调用）。
type Notifier struct {
	url     string
	events  map[string]bool
	client  *http.Client
	timeout time.Duration
	now     func() time.Time
	window  time.Duration

	queue chan payload
	done  chan struct{}
	wg    sync.WaitGroup
	once  sync.Once

	mu   sync.Mutex
	last map[string]time.Time
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
		n.client = &http.Client{Timeout: n.timeout}
	}
}

// Notify 投递一条事件：未订阅/去重窗口内重复时丢弃；队列满时丢新事件并计数。
func (n *Notifier) Notify(e Event) {
	if n == nil || n.url == "" || !n.events[e.Event] {
		return
	}
	if !n.allow(e) {
		return
	}
	p := payload{
		Event:    e.Event,
		Time:     n.now().UTC().Format(time.RFC3339),
		Nickname: e.Nickname,
		Realm:    e.Realm,
		Summary:  e.Summary,
		Detail:   e.Detail,
	}
	if e.UID != "" {
		p.UID = logfmt.UID8(e.UID)
	}
	select {
	case n.queue <- p:
	default:
		log.Printf("WARN: [notify] event=%s dropped: queue full", p.Event)
	}
}

// allow 做同事件 + 账号的窗口去重。
func (n *Notifier) allow(e Event) bool {
	key := e.Event + "\x00" + e.UID
	now := n.now()
	n.mu.Lock()
	defer n.mu.Unlock()
	if last, ok := n.last[key]; ok && now.Sub(last) < n.window {
		return false
	}
	n.last[key] = now
	return true
}

// run 串行发送（单 worker，天然限频；失败不影响下一事件）。
func (n *Notifier) run() {
	defer n.wg.Done()
	for {
		select {
		case p := <-n.queue:
			n.send(p)
		case <-n.done:
			return
		}
	}
}

// send POST JSON；失败只记事件名，不带 URL（URL 可能内嵌 token）。
func (n *Notifier) send(p payload) {
	raw, err := json.Marshal(p)
	if err != nil {
		log.Printf("WARN: [notify] event=%s encode failed", p.Event)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), n.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.url, bytes.NewReader(raw))
	if err != nil {
		log.Printf("WARN: [notify] event=%s request build failed", p.Event)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := n.client.Do(req)
	if err != nil {
		log.Printf("WARN: [notify] event=%s send failed", p.Event)
		return
	}
	_ = resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		log.Printf("WARN: [notify] event=%s send failed: status=%d", p.Event, resp.StatusCode)
	}
}

// Close 停止 worker（幂等）；未发送的队列事件被丢弃。
func (n *Notifier) Close() {
	if n == nil || n.done == nil {
		return
	}
	n.once.Do(func() {
		close(n.done)
		n.wg.Wait()
	})
}
