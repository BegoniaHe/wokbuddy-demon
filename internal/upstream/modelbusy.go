// modelbusy.go 官方渲染层模型繁忙度查询（/v2/chat/busy，issue #258）。
//
// 契约来自官方 renderer：批量最多 50 个模型，level ∈
// idle|normal|busy|congested|unavailable|unknown。本客户端按 50 一批发送，
// 并兼容 map / 数组两种响应形态；失败只由调用方并入观测错误，不影响模型目录
// 与选号。
package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"strings"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
)

const (
	modelBusyPath    = "/v2/chat/busy"
	modelBusyBatch   = 50
	modelBusyMaxBody = 1 << 20
)

var errModelBusyTooLarge = errors.New("model busy response too large")

// ModelBusy 查询一批模型的繁忙度（内部按 50 分批）。返回 model → level。
func (c *Client) ModelBusy(ctx context.Context, account *auth.Auth, models []string) (map[string]string, error) {
	if len(models) == 0 {
		return map[string]string{}, nil
	}

	out := make(map[string]string, len(models))

	for start := 0; start < len(models); start += modelBusyBatch {
		end := min(start+modelBusyBatch, len(models))
		batch := models[start:end]

		levels, err := c.modelBusyBatch(ctx, account, batch)
		if err != nil {
			return out, err
		}

		maps.Copy(out, levels)
	}

	return out, nil
}

// modelBusyBatch 单批查询。
func (c *Client) modelBusyBatch(ctx context.Context, account *auth.Auth, models []string) (map[string]string, error) {
	raw, err := json.Marshal(map[string]any{"models": models})
	if err != nil {
		return nil, fmt.Errorf("ModelBusy encode: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.chatBase(account)+modelBusyPath, bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("ModelBusy: %w", err)
	}

	// 官方渲染层复用 chat 会话头；只覆盖 Accept 为 JSON（非 SSE）。
	c.ChatHeaders(req, account, "", ChatMeta{})
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("ModelBusy: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, modelBusyMaxBody+1))
	if err != nil {
		return nil, fmt.Errorf("ModelBusy read: %w", err)
	}

	if int64(len(body)) > modelBusyMaxBody {
		return nil, fmt.Errorf("%w: > %d bytes", errModelBusyTooLarge, modelBusyMaxBody)
	}

	if resp.StatusCode >= 400 {
		return nil, &Error{
			Kind: Classify(resp.StatusCode, string(body)), Status: resp.StatusCode,
			Msg: truncate(string(body), 200), Code: businessCode(string(body)),
		}
	}

	return parseModelBusy(body), nil
}

// parseModelBusy 兼容 {code,data} 信封与 map/数组响应。
func parseModelBusy(body []byte) map[string]string {
	var root any
	if json.Unmarshal(body, &root) != nil {
		return map[string]string{}
	}

	if env, ok := root.(map[string]any); ok {
		if data, exists := env["data"]; exists && data != nil {
			root = data
		}
	}

	out := map[string]string{}
	collectModelBusy(root, out, 0)

	return out
}

// collectModelBusy 递归收集 model→level（深度有界）。
//
//nolint:cyclop,gocognit // 兼容三种响应形态，深度有界且分支集中。
func collectModelBusy(node any, out map[string]string, depth int) {
	if depth > 3 {
		return
	}

	switch value := node.(type) {
	case map[string]any:
		for key, item := range value {
			switch typed := item.(type) {
			case string:
				if isModelBusyLevel(typed) {
					out[key] = typed
				}
			case map[string]any:
				if level, _ := typed["level"].(string); isModelBusyLevel(level) {
					out[key] = level
				} else {
					collectModelBusy(typed, out, depth+1)
				}
			default:
				collectModelBusy(typed, out, depth+1)
			}
		}
	case []any:
		for _, item := range value {
			m, _ := item.(map[string]any)
			if m == nil {
				continue
			}

			name, _ := m["model"].(string)
			if name == "" {
				name, _ = m["id"].(string)
			}

			level, _ := m["level"].(string)
			if name != "" && isModelBusyLevel(level) {
				out[name] = level
			} else {
				collectModelBusy(m, out, depth+1)
			}
		}
	}
}

// isModelBusyLevel 官方六个 level。
func isModelBusyLevel(level string) bool {
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "idle", "normal", "busy", "congested", "unavailable", "unknown":
		return true
	}

	return false
}
