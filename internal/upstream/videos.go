// videos.go 官方视频端点（/v2/videos/generations 提交任务、/v2/videos/tasks
// 查询任务）的最小透传客户端（issue #467）。
//
// 与 chat 的差异同 images.go：
//   - 独立 JSON 请求/响应，不套 chat 的 sanitize/thinking/prepareBody 管线；
//   - 单次尝试，调用方不做换号轮转（保守策略，避免重复提交计费）；
//   - 官方 CLI 自己「提交后循环 poll 直到 completed」，网关只做无状态透传：
//     提交返回 task id，查询按客户端请求透出上游原文，不实现轮询循环。
package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
)

// 官方视频端点路径（CN / global 共用 chatBase）。
// 官方 CLI `VideoService.submitTask` / `pollTask` 逐字使用这两个路径。
const (
	// VideoGenerationsPath 提交视频生成任务。
	VideoGenerationsPath = "/v2/videos/generations"
	// VideoTasksPath 查询视频任务状态。
	VideoTasksPath = "/v2/videos/tasks"
)

// videoResponseMaxBytes 视频响应上限：提交只有 task id，查询只有状态与直链，
// 8 MiB 已远超任何合法回执（防止异常上游把内存打满）。
const videoResponseMaxBytes = 8 << 20

var (
	errVideoResponseTooLarge = errors.New("video response too large")
	errVideoTaskIDRequired   = errors.New("GetVideoTask: task_id 为空")
)

// GenerateVideo 向账号所属域的视频提交端点透传一次请求（/v2/videos/generations）。
// 返回上游原始 JSON 与状态码；业务错误（HTTP >=400 或 200 + 非零 code）返回 *Error，
// body 保留原文供透传。头集见 `VideosHeaders`（headers.go 的 surface 体系）。
//
// 契约来自官方 CLI `VideoService.submitTask`（CN 5.9.3
// `cli/dist/codebuddy-lite-wb.mjs`）：POST `${endpoint}/v2/videos/generations`，
// body 为 `{prompt, model, watermark:true}` 加可选的 seconds / image_url /
// last_image_url / reference_images / reference_videos / reference_audios /
// extra_parameters；响应信封 `{code, msg, requestId, data:{id}}`，`data.id` 是
// 任务 id。本方法只透传：字段语义由调用方给足。
//
// 与出图同策略：单次尝试、不换号（避免重复计费）。
//
//nolint:dupl,gocritic // 与 GenerateImage 仅路径不同；ChatMeta 值传递与 ChatHeaders 同一 API。
func (c *Client) GenerateVideo(
	ctx context.Context, account *auth.Auth, body []byte, clientIP string, meta ChatMeta,
) (int, []byte, error) {
	return c.postVideo(ctx, account, VideoGenerationsPath, body, clientIP, meta)
}

// GetVideoTask 向账号所属域的视频任务端点查询一次（/v2/videos/tasks）。
//
// 契约来自官方 CLI `VideoService.pollTask`：POST `${endpoint}/v2/videos/tasks`，
// body 只有 `{task_id}`；响应 `{code, msg, requestId, data:{status, data:[{url,...}],
// usage:{credit}}}`，`status` 为 completed / failed / 其他（排队或运行中）。
// 本方法只透传一次请求，终态判定与重试节奏留给调用方（网关不做轮询）。
//
//nolint:gocritic // ChatMeta 值传递与 ChatHeaders 同一 API。
func (c *Client) GetVideoTask(
	ctx context.Context, account *auth.Auth, taskID, clientIP string, meta ChatMeta,
) (int, []byte, error) {
	taskID = strings.TrimSpace(taskID)
	if taskID == "" {
		return 0, nil, errVideoTaskIDRequired
	}

	body, err := json.Marshal(struct {
		TaskID string `json:"task_id"`
	}{TaskID: taskID})
	if err != nil {
		return 0, nil, fmt.Errorf("GetVideoTask: 编码 task_id: %w", err)
	}

	return c.postVideo(ctx, account, VideoTasksPath, body, clientIP, meta)
}

// postVideo 视频端点共用请求装配与响应读取；path 为上游绝对路径，body 为已编码
// JSON（提交为入站原文，查询为 task_id 信封）。
//
//nolint:funlen,gocritic,dupl // 请求装配/解析线性展开，与 postImage 同一可读形态；ChatMeta 值传递同 ChatHeaders。
func (c *Client) postVideo(
	ctx context.Context, account *auth.Auth, path string, body []byte, clientIP string, meta ChatMeta,
) (int, []byte, error) {
	if ctx == nil {
		ctx = context.Background()
	}

	req, err := http.NewRequestWithContext(
		ctx, http.MethodPost, c.chatBase(account)+path, bytes.NewReader(body),
	)
	if err != nil {
		return 0, nil, fmt.Errorf("postVideo %s: %w", path, err)
	}

	c.VideosHeaders(req, account, clientIP, meta)

	resp, err := c.HTTPForAccount(account).Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("postVideo %s: %w", path, err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, videoResponseMaxBytes+1))
	if err != nil {
		return resp.StatusCode, nil, fmt.Errorf("postVideo %s read body: %w", path, err)
	}

	if int64(len(raw)) > videoResponseMaxBytes {
		return resp.StatusCode, nil, fmt.Errorf("%w: > %d bytes", errVideoResponseTooLarge, videoResponseMaxBytes)
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
