// images.go 官方图像端点（/v2/images/generations 出图、/v2/images/edits 图生图）
// 的最小透传客户端。
//
// 与 chat 的差异（issue #253 / #357）：
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

// 官方图像端点路径（CN / global 共用 chatBase）。
const (
	// ImageGenerationsPath 文生图。
	ImageGenerationsPath = "/v2/images/generations"
	// ImageEditsPath 图生图。官方 CLI `ImageService.editImage` 用 JSON 而非 multipart。
	ImageEditsPath = "/v2/images/edits"
)

// imageResponseMaxBytes 图像响应上限：b64 图片可能较大，但 64 MiB 已远超单图。
const imageResponseMaxBytes = 64 << 20

var errImageResponseTooLarge = errors.New("image response too large")

// ImagesHeaders 出图请求头：复用 chat 头族，但 Accept 改回 JSON（非 SSE）。
// 统一指纹是刻意取舍，理由与边界见 `internal/upstream/UPSTREAM.md` §六。
//
//nolint:gocritic // ChatMeta 值传递与 ChatHeaders 保持同一公开 API。
func (c *Client) ImagesHeaders(req *http.Request, account *auth.Auth, clientIP string, meta ChatMeta) {
	c.ChatHeaders(req, account, clientIP, meta)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
}

// GenerateImage 向账号所属域的发图端点透传一次请求（/v2/images/generations）。
// 返回上游原始 JSON 与状态码；业务错误（HTTP >=400 或 200 + 非零 code）返回 *Error，
// body 保留原文供透传。
//
//nolint:dupl,gocritic // 与 EditImage 仅路径不同，两个导出入口并排便于对照；ChatMeta 值传递与 ChatHeaders 同一 API。
func (c *Client) GenerateImage(
	ctx context.Context, account *auth.Auth, body []byte, clientIP string, meta ChatMeta,
) (int, []byte, error) {
	return c.postImage(ctx, account, ImageGenerationsPath, body, clientIP, meta)
}

// EditImage 向账号所属域的图生图端点透传一次请求（/v2/images/edits）。
//
// 契约来自官方 CLI `ImageService.editImage`（CN 5.9.3
// `cli/dist/codebuddy-lite-wb.mjs`）：POST `${endpoint}/v2/images/edits`，
// Content-Type 为 application/json（**不是 multipart**），请求头是
// Authorization + conversation 头族 + identity 头族（与 generations 同族，
// 即 ImagesHeaders），body 由 buildBaseRequestBody 构造：
//
//   - model / prompt / size（默认 1024x1024）必填或可选，与 generations 同形；
//   - image：单个字符串或字符串数组，元素为 data URL / http URL / 裸 base64；
//     空数组或缺失时官方不带该字段；
//   - input_fidelity：存在时原样透传。
//
// 本方法只做透传：字段语义由调用方给足，不在此校验 image 形态。
// 与 generations 同策略：单次尝试、不换号（避免重复计费）。
//
//nolint:dupl,gocritic // 与 GenerateImage 仅路径不同；ChatMeta 值传递与 ChatHeaders 同一 API。
func (c *Client) EditImage(
	ctx context.Context, account *auth.Auth, body []byte, clientIP string, meta ChatMeta,
) (int, []byte, error) {
	return c.postImage(ctx, account, ImageEditsPath, body, clientIP, meta)
}

// postImage 图像端点共用请求装配与响应读取；path 为上游绝对路径，body 原样透传。
//
//nolint:funlen,gocritic // 请求装配/解析线性展开，与 chat 的 doJSON 保持同一可读风格；ChatMeta 值传递同 ChatHeaders。
func (c *Client) postImage(
	ctx context.Context, account *auth.Auth, path string, body []byte, clientIP string, meta ChatMeta,
) (int, []byte, error) {
	if ctx == nil {
		ctx = context.Background()
	}

	req, err := http.NewRequestWithContext(
		ctx, http.MethodPost, c.chatBase(account)+path, bytes.NewReader(body),
	)
	if err != nil {
		return 0, nil, fmt.Errorf("postImage %s: %w", path, err)
	}

	c.ImagesHeaders(req, account, clientIP, meta)

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("postImage %s: %w", path, err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, imageResponseMaxBytes+1))
	if err != nil {
		return resp.StatusCode, nil, fmt.Errorf("postImage %s read body: %w", path, err)
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
