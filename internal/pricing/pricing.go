// Package pricing 只读拉取 OpenRouter 公开模型价格并估算等价花费（issue #99）。
//
// 价格仅用于面板展示，不参与选号、限流或计费。拉取失败保留上一份成功快照，
// 未定价模型不猜数字。价格单位：USD / token。
package pricing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"math"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// DefaultURL OpenRouter 模型目录（公开，无需鉴权）。
const DefaultURL = "https://openrouter.ai/api/v1/models"

var (
	errRefreshFailed = errors.New("Refresh failed")
	// errEmptyCatalog 上游返回了可解析但没有可用条目的目录：与「拉取失败」同口径，
	// 保留上一份成功快照（否则面板估算金额会瞬间归零）。
	errEmptyCatalog = errors.New("catalog has no usable price entries")
)

// Price 单个模型的每 token 价格（USD）。
type Price struct {
	Prompt     float64 `json:"prompt"`
	Completion float64 `json:"completion"`
	CacheRead  float64 `json:"cache_read"`
}

// catalogModel 上游目录里的单条模型条目（只取价格相关字段）。
type catalogModel struct {
	ID      string            `json:"id"`
	Pricing map[string]string `json:"pricing"`
}

// Snapshot 一次只读快照。
type Snapshot struct {
	Prices    map[string]Price
	UpdatedAt time.Time
	LastError string
}

// Catalog 并发安全的价格缓存。
type Catalog struct {
	mu      sync.RWMutex
	url     string
	client  *http.Client
	prices  map[string]Price
	updated time.Time
	lastErr string
}

// New 构造价格缓存；client 为 nil 时用带超时的默认 client。
func New(url string, client *http.Client) *Catalog {
	if url == "" {
		url = DefaultURL
	}

	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}

	return &Catalog{url: url, client: client, prices: map[string]Price{}}
}

// maxCatalogBytes 价格目录响应体上限（正常响应约 1–2 MiB）。
const maxCatalogBytes = 8 << 20

// Refresh 拉取并替换价格快照；失败时保留旧快照并记录原因。
func (c *Catalog) Refresh(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url, http.NoBody)
	if err != nil {
		c.setError(err)
		return fmt.Errorf("Refresh: %w", err)
	}

	resp, err := c.client.Do(req)
	if err != nil {
		c.setError(err)
		return fmt.Errorf("Refresh: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		err := fmt.Errorf("openrouter status %d: %w", resp.StatusCode, errRefreshFailed)
		c.setError(err)

		return err
	}

	var body struct {
		Data []catalogModel `json:"data"`
	}
	// 限制响应体大小：默认 URL 是公网第三方（openrouter），pricing.New 也接受任意
	// URL，上游异常时不应该让整份响应进内存。
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxCatalogBytes)).Decode(&body); err != nil {
		c.setError(err)
		return fmt.Errorf("Refresh: %w", err)
	}

	prices := parsePrices(body.Data)
	// 空目录（200 + data:[] 或全部条目被 validPrice 丢弃）不覆盖旧快照：包注释承诺
	// 「拉取失败保留上一份成功快照」，用空表覆盖会让面板所有估算金额瞬间归零且
	// 没有任何提示。
	if len(prices) == 0 {
		err := errEmptyCatalog
		c.setError(err)

		return fmt.Errorf("Refresh: %w", err)
	}

	c.mu.Lock()
	c.prices = prices
	c.updated = time.Now()
	c.lastErr = ""
	c.mu.Unlock()

	return nil
}

// parsePrices 把上游目录条目转为价格表，跳过 id 缺失或价格非法的条目。
func parsePrices(models []catalogModel) map[string]Price {
	prices := make(map[string]Price, len(models))

	for _, model := range models {
		if model.ID == "" {
			continue
		}

		prompt, err := strconv.ParseFloat(model.Pricing["prompt"], 64)
		if err != nil {
			continue
		}

		completion, err := strconv.ParseFloat(model.Pricing["completion"], 64)
		if err != nil {
			continue
		}

		// ParseFloat 接受 NaN/Inf/负值：OpenRouter 的 openrouter/auto 就返回 -1，
		// 而 NaN 会让面板 /panel/api/pricing 的 json.Marshal 直接 500（unsupported
		// value: NaN），且在下一次成功刷新前一直坏着。这类条目按无效丢弃。
		if !validPrice(prompt) || !validPrice(completion) {
			continue
		}

		cacheRead, _ := strconv.ParseFloat(model.Pricing["input_cache_read"], 64)
		if !validPrice(cacheRead) {
			cacheRead = 0
		}

		prices[model.ID] = Price{Prompt: prompt, Completion: completion, CacheRead: cacheRead}
	}

	return prices
}

// validPrice 判定价格合法：有限且非负（NaN/Inf/负值一律视为无效条目）。
func validPrice(v float64) bool {
	return !math.IsNaN(v) && !math.IsInf(v, 0) && v >= 0
}

func (c *Catalog) setError(err error) {
	c.mu.Lock()
	c.lastErr = err.Error()
	c.mu.Unlock()
}

// Snapshot 返回当前价格的只读副本。
func (c *Catalog) Snapshot() Snapshot {
	if c == nil {
		return Snapshot{Prices: nil, UpdatedAt: time.Time{}, LastError: ""}
	}

	c.mu.RLock()
	defer c.mu.RUnlock()

	out := make(map[string]Price, len(c.prices))
	maps.Copy(out, c.prices)

	return Snapshot{Prices: out, UpdatedAt: c.updated, LastError: c.lastErr}
}

// Estimate 估算一次用量的 USD 花费。缓存命中/未命中分别计价；未定价返回 false。
// exchangeRate 用于把 USD 折算成展示币种（1 = 保持 USD）。
func (c *Catalog) Estimate(
	model string, prompt, completion, cacheHit, cacheMiss int64, exchangeRate float64,
) (float64, bool) {
	if c == nil {
		return 0, false
	}

	c.mu.RLock()
	price, ok := c.prices[model]
	c.mu.RUnlock()

	if !ok {
		return 0, false
	}

	if exchangeRate <= 0 {
		exchangeRate = 1
	}

	usd := float64(prompt) * price.Prompt
	if cacheHit+cacheMiss > 0 {
		// 上游只回报部分缓存计数时，cacheHit+cacheMiss 可能小于 prompt：差额
		// 按普通输入价计入，不能整段丢掉（否则少算钱）。
		uncached := max(0, prompt-cacheHit-cacheMiss)

		usd = float64(cacheMiss+uncached)*price.Prompt + float64(cacheHit)*price.CacheRead
	}

	usd += float64(completion) * price.Completion

	return usd * exchangeRate, true
}
