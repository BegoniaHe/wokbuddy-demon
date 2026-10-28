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
func (c *Client) ModelBusy(
	ctx context.Context, account *auth.Auth, models []string,
) (map[string]string, error) {
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
func (c *Client) modelBusyBatch(
	ctx context.Context, account *auth.Auth, models []string,
) (map[string]string, error) {
	raw, err := json.Marshal(map[string]any{"models": models})
	if err != nil {
		return nil, fmt.Errorf("ModelBusy encode: %w", err)
	}

	req, err := http.NewRequestWithContext(
		ctx, http.MethodPost, c.chatBase(account)+modelBusyPath, bytes.NewReader(raw),
	)
	if err != nil {
		return nil, fmt.Errorf("ModelBusy: %w", err)
	}

	// 官方渲染层走通用 http 封装，不构造会话头：真机只有 8 个轻头。
	// 头族见 headers.go 的 lightHeaders，取舍见 `internal/upstream/UPSTREAM.md` §六。
	c.applyHeaders(surfaceModelBusy, req, account, "", ChatMeta{})

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

	if resp.StatusCode >= http.StatusBadRequest {
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
// 官方 /v2/chat/busy 的数组项是 {"model","busy_level"}；map 形态是
// {model: level} 或 {model: {busy_level: level}}。level 键只作为旧形态兼容，
// busy_level 优先。字段名本身不能当模型名写入结果（否则模型页出现伪模型）。
//
//nolint:cyclop,gocognit,gocyclo // 兼容三种响应形态，深度有界且分支集中。
func collectModelBusy(node any, out map[string]string, depth int) {
	if depth > 3 {
		return
	}

	switch value := node.(type) {
	case map[string]any:
		for key, item := range value {
			if isModelBusyMetaKey(key) {
				continue
			}

			switch typed := item.(type) {
			case string:
				if level, ok := normalizeModelBusyLevel(typed); ok {
					out[key] = level
				}
			case map[string]any:
				if level, ok := modelBusyLevel(typed); ok {
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
			m, ok := item.(map[string]any)
			if !ok {
				collectModelBusy(item, out, depth+1)
				continue
			}

			name, _ := m["model"].(string)
			if name == "" {
				name, _ = m["id"].(string)
			}

			if level, ok := modelBusyLevel(m); ok && name != "" {
				out[name] = level
			} else {
				collectModelBusy(m, out, depth+1)
			}
		}
	}
}

// modelBusyLevel 读 busy_level，缺失/非法时回落旧 level 键。
func modelBusyLevel(m map[string]any) (string, bool) {
	if level, ok := normalizeModelBusyLevel(stringValue(m["busy_level"])); ok {
		return level, true
	}

	return normalizeModelBusyLevel(stringValue(m["level"]))
}

// normalizeModelBusyLevel 归一官方六个 level；大小写/空白不一致时统一为小写。
func normalizeModelBusyLevel(level string) (string, bool) {
	level = strings.ToLower(strings.TrimSpace(level))
	if isModelBusyLevel(level) {
		return level, true
	}

	return "", false
}

// isModelBusyMetaKey 跳过响应字段名，避免把 busy_level / level 当成模型名。
func isModelBusyMetaKey(key string) bool {
	switch strings.ToLower(strings.TrimSpace(key)) {
	case "busy_level", "level", "model", "id":
		return true
	}

	return false
}

// stringValue 读取 JSON 字符串字段（非字符串返回空串）。
func stringValue(v any) string {
	s, _ := v.(string)
	return s
}

// isModelBusyLevel 官方六个 level。
func isModelBusyLevel(level string) bool {
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "idle", "normal", "busy", "congested", "unavailable", "unknown":
		return true
	}

	return false
}
