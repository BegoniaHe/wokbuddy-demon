// Package pricing 只读拉取 OpenRouter 公开模型价格并估算等价花费（issue #99）。
//
// 价格仅用于面板展示，不参与选号、限流或计费。拉取失败保留上一份成功快照，
// 未定价模型不猜数字。价格单位：USD / token。
package pricing

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// DefaultURL OpenRouter 模型目录（公开，无需鉴权）。
const DefaultURL = "https://openrouter.ai/api/v1/models"

// Price 单个模型的每 token 价格（USD）。
type Price struct {
	Prompt     float64 `json:"prompt"`
	Completion float64 `json:"completion"`
	CacheRead  float64 `json:"cache_read"`
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

// Refresh 拉取并替换价格快照；失败时保留旧快照并记录原因。
func (c *Catalog) Refresh(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url, http.NoBody)
	if err != nil {
		c.setError(err)
		return err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		c.setError(err)
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		err := fmt.Errorf("openrouter status %d", resp.StatusCode)
		c.setError(err)
		return err
	}
	var body struct {
		Data []struct {
			ID      string            `json:"id"`
			Pricing map[string]string `json:"pricing"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		c.setError(err)
		return err
	}
	prices := make(map[string]Price, len(body.Data))
	for _, model := range body.Data {
		prompt, err1 := strconv.ParseFloat(model.Pricing["prompt"], 64)
		completion, err2 := strconv.ParseFloat(model.Pricing["completion"], 64)
		if model.ID == "" || err1 != nil || err2 != nil {
			continue
		}
		cacheRead, _ := strconv.ParseFloat(model.Pricing["input_cache_read"], 64)
		prices[model.ID] = Price{Prompt: prompt, Completion: completion, CacheRead: cacheRead}
	}
	c.mu.Lock()
	c.prices = prices
	c.updated = time.Now()
	c.lastErr = ""
	c.mu.Unlock()
	return nil
}

func (c *Catalog) setError(err error) {
	c.mu.Lock()
	c.lastErr = err.Error()
	c.mu.Unlock()
}

// Snapshot 返回当前价格的只读副本。
func (c *Catalog) Snapshot() Snapshot {
	if c == nil {
		return Snapshot{}
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make(map[string]Price, len(c.prices))
	for id, p := range c.prices {
		out[id] = p
	}
	return Snapshot{Prices: out, UpdatedAt: c.updated, LastError: c.lastErr}
}

// Estimate 估算一次用量的 USD 花费。缓存命中/未命中分别计价；未定价返回 false。
// exchangeRate 用于把 USD 折算成展示币种（1 = 保持 USD）。
func (c *Catalog) Estimate(model string, prompt, completion, cacheHit, cacheMiss int64, exchangeRate float64) (float64, bool) {
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
		usd = float64(cacheMiss)*price.Prompt + float64(cacheHit)*price.CacheRead
	}
	usd += float64(completion) * price.Completion
	return usd * exchangeRate, true
}
