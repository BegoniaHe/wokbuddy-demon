// images.go 官方图像生成端点（/v2/images/generations）的最小透传客户端。
//
// 与 chat 的差异（issue #253）：
//   - 独立 JSON 请求/响应，不套 chat 的 sanitize/thinking/prepareBody 管线；
//   - 单次尝试，调用方不做换号轮转（保守策略，避免重复出图计费）；
//   - 响应 {created,data:[...],usage} 原样返回，不转存 COS 直链。
package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
)

// imageGenerationsPath 官方出图路径（CN / global 共用 chatBase）。
const imageGenerationsPath = "/v2/images/generations"

// imageResponseMaxBytes 图像响应上限：b64 图片可能较大，但 64 MiB 已远超单图。
const imageResponseMaxBytes = 64 << 20

var errImageResponseTooLarge = errors.New("image response too large")

// GenerateImage 向账号所属域的发图端点透传一次请求。返回上游原始 JSON 与状态码；
// 业务错误（HTTP >=400 或 200 + 非零 code）返回 *Error，body 保留原文供透传。
//
//nolint:funlen,gocritic // ChatMeta 值传递与 ChatHeaders 保持同一公开 API；请求装配/解析线性展开。
func (c *Client) GenerateImage(
	ctx context.Context, account *auth.Auth, body []byte, clientIP string, meta ChatMeta,
) (int, []byte, error) {
	if ctx == nil {
		ctx = context.Background()
	}

	req, err := http.NewRequestWithContext(
		ctx, http.MethodPost, c.chatBase(account)+imageGenerationsPath, bytes.NewReader(body),
	)
	if err != nil {
		return 0, nil, fmt.Errorf("GenerateImage: %w", err)
	}

	c.ImagesHeaders(req, account, clientIP, meta)

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("GenerateImage: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, imageResponseMaxBytes+1))
	if err != nil {
		return resp.StatusCode, nil, fmt.Errorf("GenerateImage read body: %w", err)
	}

	if int64(len(raw)) > imageResponseMaxBytes {
		return resp.StatusCode, nil, fmt.Errorf("%w: > %d bytes", errImageResponseTooLarge, imageResponseMaxBytes)
	}

	if resp.StatusCode >= http.StatusBadRequest {
		return resp.StatusCode, raw, &Error{
			Kind: Classify(resp.StatusCode, string(raw)), Status: resp.StatusCode,
			Msg: truncate(string(raw), 200), Code: businessCode(string(raw)),
		}
	}

	// 200 + 业务错误信封（部分上游故障形态）。
	var env apiEnvelope
	if json.Unmarshal(raw, &env) == nil && env.Code != 0 {
		kind := Classify(resp.StatusCode, string(raw))
		if kind == ErrNone {
			kind = ErrClient
		}

		return resp.StatusCode, raw, &Error{
			Kind: kind, Status: resp.StatusCode,
			Msg: truncate(string(raw), 200), Code: businessCode(string(raw)),
		}
	}

	return resp.StatusCode, raw, nil
}
