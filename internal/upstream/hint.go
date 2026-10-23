// hint.go 网关错误附加说明字段（error.gateway_hint）的单一事实来源。
//
// 纪律（任务书 gateway-hint）：
//   - error.message 永远是上游 body 原文透传（5755fe3 透传原则不动）；
//     gateway_hint 只做与 message **并列**的网关视角补充说明，绝不替换/包装 message。
//   - 文案集中在本文件（一张 Kind 表 + 11133/11135 形态判定），按 ErrKind + 上下文
//     （请求带图/模型目录能力）映射，不散落 handler 的 if-else。
//   - 未覆盖形态返回空串 → 响应不带该字段（不编造）。
//   - hint 措辞是英文（错误响应面向客户端工具链，英文是通用口径）。
package upstream

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
)

// GatewayHint 按错误形态返回网关视角的补充说明（error.gateway_hint 字段值）。
// msg 是上游错误 body 原文（或 SSE error 帧 payload）；kind 是权威分类
// （Classify / *Error 信封）。返回空串 = 未覆盖形态，调用方不带字段。
//
// ctx 携带判定 hint 所需的请求侧上下文（零值合法，信息缺失时相关形态退为中性
// hint 或无 hint）：
//   - HasImage：请求体是否携带 image_url part（11133 的「模型不支持图片」指向前提）；
//   - Model / ModelInCatalog / ModelSupportsImages：模型目录对该模型的
//     supports_images 声明（目录未收录 → 不做「不支持」判定，防查不到误判成不支持）。
//
// 判定次序：11133/11135 上游业务码**先于** Kind 表——实测这两族归 ErrClient/
// ErrBadParams 皆有可能（Classify 词表不含 11133），hint 层自带判定（hint 是补充
// 说明非权威分类，误判代价只是多一条中性补充说明）；其余走 Kind 一对一映射。
func GatewayHint(kind ErrKind, msg string, ctx HintContext) string {
	// 11133 model_param_invalid 家族（图片回归实测：不支持图片的模型传图，或任意
	// 参数被模型供应商拒绝）。只有请求确实带图、且目录能对该模型做出「不支持图片」
	// 的判定时才给「换模型」指向，否则退中性参数形态（可能是任意参数问题，不点名图片）。
	if isModelParamInvalid(msg) {
		if ctx.HasImage && ctx.ModelInCatalog && !ctx.ModelSupportsImages {
			return "model " + ctx.Model + " does not support images; pick one with supports_images=true from /v1/models"
		}

		return "request parameters were rejected by the model provider; check message format and model capabilities"
	}
	// 11135 invalid_image_data 家族（图片数据无效，Discussion #77 实测形态）。
	if isInvalidImageData(msg) {
		return "image data rejected by upstream; use a real/valid image, may need a new conversation"
	}
	// 其余走 Kind 一对一映射；未覆盖形态（ErrNone/ErrNotFound/ErrServer/
	// ErrBadParams/ErrClient 等）无 hint。
	return gatewayHintByKind[kind]
}

// gatewayHintByKind ErrKind → 固定 hint 文案（未覆盖形态无 hint）。
var gatewayHintByKind = map[ErrKind]string{
	ErrPromptTooLong: "request context exceeds the model's limit; reduce history/message size",
	ErrImageInvalid:  "image request was rejected by upstream; check image_url format and image data",
	// 账号级 WAF 403 与 IP 级 fail-fast 同 hint：两者对客户端的动作一致
	// （等待窗口过去再试，换号/立刻重试无意义）。
	ErrWafBlock: "upstream WAF blocked the gateway; retry after the block window",
	ErrSoftRate: "rate limited by upstream; retry after reset",
	// 队列满载是渠道级容量事件：不是账号问题，换号撞同一堵墙，只应退避重试。
	ErrQueueFull: "upstream inference queue is full (channel-level capacity, not an account problem); " +
		"switching accounts will not help, retry with backoff",
	ErrAccountFault: "account-level fault at upstream (auth/quota state); " +
		"the gateway will rotate or disable this account",
	ErrSessionDead: "account session expired at upstream; the account is disabled until re-login",
	ErrHardCredit:  "account credits exhausted at upstream; waiting for daily check-in to restore",
	ErrModelBlocked: "this account lacks permission for the requested model; " +
		"the gateway will retry on another account",
	ErrModelNotFound: "upstream model catalog has no such model; " +
		"check /v1/models and switch to a supported model",
	ErrModelAccess: "this account cannot use the requested model; " +
		"switch model or try another account",
	// 措辞不含 "upstream"：content_blocked 响应有不含上游字样的既有口径
	// （handler_test 的泄漏守卫），hint 遵守同一口径。
	ErrContentBlocked: "request content was rejected by content policy; adjust the prompt and retry",
	// 11-128 通道校验失败：不是账号问题、也不是内容问题——排查客户端/通道形态
	// （UA、必需头、中间代理改写）。换号与改提示词都无效。
	ErrChannelRefused: "request rejected by channel validation (11-128); " +
		"check client user-agent, required headers and any intermediate proxy",
}

// HintContext gateway_hint 判定所需的请求侧上下文（handler 侧组装，见
// Handler.hintContext）。零值合法。
type HintContext struct {
	Model               string // 请求裸模型名（可空）
	HasImage            bool   // 请求体是否携带 image_url part
	ModelSupportsImages bool   // 模型目录 supports_images 声明（仅 ModelInCatalog 时有意义）
	ModelInCatalog      bool   // 模型目录是否收录该模型（「不支持」判定的前提）
}

// noHealthyHint 本地调度类错误（池中无健康号可用/传输层抖动，无上游原文可透传）
// 的固定 hint。不进 GatewayHint：它没有 ErrKind，是网关自己的调度事实。
const noHealthyHint = "no healthy account available in pool; check /status or retry later"

// NoHealthyAccountHint 本地调度错误的 gateway_hint（与 no_healthy_account code 配套）。
func NoHealthyAccountHint() string { return noHealthyHint }

// FrameHintFunc 返回 SSE error 帧的 gateway_hint 判定函数（Stream 的可选参数）。
// ctxFn 惰性求值：仅在实际撞到 error 帧才调用（正常流零开销，模型目录查询
// 不会为每个成功请求触发）。
func FrameHintFunc(ctxFn func() HintContext) func(string) string {
	return func(payload string) string {
		if payload == "" || payload == "[DONE]" {
			return ""
		}

		return GatewayHint(FrameKind(payload), payload, ctxFn())
	}
}

// FrameKind 从 SSE error 帧 payload 判定 ErrKind：6004 模型级限流的流式形态
// （IsModelRateLimit 对帧 JSON 直接命中）优先；其余先取帧内业务码
// （error.code / error.data.code / data.code，字符串或数字），取不到再回落
// error.message 走 Classify（请求级 400 口径）。判不出 → ErrNone（无 hint）。
//
//nolint:cyclop // 帧形态按优先级线性判定。
func FrameKind(payload string) ErrKind {
	if IsModelRateLimit(payload) {
		return ErrSoftRate
	}

	// 顶层 {code,msg} 业务错误帧（issue #244）：与 error 帧同口径分类。
	if code, msg, ok := BusinessFrameError(payload); ok {
		probe, err := json.Marshal(struct {
			Code string `json:"code"`
			Msg  string `json:"msg"`
		}{Code: code, Msg: msg})
		if err == nil {
			if kind := Classify(http.StatusBadRequest, string(probe)); kind != ErrNone && kind != ErrClient {
				return kind
			}
		}

		return Classify(http.StatusBadRequest, msg)
	}

	var frame struct {
		Data struct {
			Code json.RawMessage `json:"code"`
		} `json:"data"`
		Error struct {
			Code    json.RawMessage `json:"code"`
			Message string          `json:"message"`
			Data    struct {
				Code json.RawMessage `json:"code"`
			} `json:"data"`
		} `json:"error"`
	}
	if json.Unmarshal([]byte(payload), &frame) != nil {
		return ErrNone
	}

	message := strings.TrimSpace(frame.Error.Message)
	if code := firstJSONCode(frame.Error.Code, frame.Error.Data.Code, frame.Data.Code); code != "" {
		// 合成信封保留 code 与 message：已知码按码分类，未知码仍可由文案兜底。
		probe, err := json.Marshal(struct {
			Code string `json:"code"`
			Msg  string `json:"msg"`
		}{Code: code, Msg: message})
		if err == nil {
			if kind := Classify(http.StatusBadRequest, string(probe)); kind != ErrNone && kind != ErrClient {
				return kind
			}
		}
	}

	if message == "" {
		return ErrNone
	}

	return Classify(http.StatusBadRequest, message)
}

// firstJSONCode 返回首个非空的 JSON code 值（字符串或数字），空/非法返回空串。
func firstJSONCode(raws ...json.RawMessage) string {
	for _, raw := range raws {
		if len(raw) == 0 {
			continue
		}

		var value any
		if json.Unmarshal(raw, &value) != nil {
			continue
		}

		switch code := value.(type) {
		case string:
			if code = strings.TrimSpace(code); code != "" {
				return code
			}
		case float64:
			return strconv.FormatFloat(code, 'f', -1, 64)
		}
	}

	return ""
}

// isModelParamInvalid 上游 11133 body 判定（code 11133 / extError.code=
// model_param_invalid / msg 文案家族）。子串口径：hint 是补充说明非权威分类，
// 宁宽勿漏。
func isModelParamInvalid(body string) bool {
	lower := strings.ToLower(body)

	return codeMarker(lower, "11133") ||
		strings.Contains(lower, "model_param_invalid") ||
		strings.Contains(lower, "invalid request parameters") ||
		strings.Contains(lower, "request parameters do not meet the current model requirements")
}

// isInvalidImageData 上游 11135 body 判定（code 11135 / invalid_image_data /
// "replace the image" msg 家族）。
func isInvalidImageData(body string) bool {
	lower := strings.ToLower(body)

	return codeMarker(lower, "11135") ||
		strings.Contains(lower, "invalid_image_data") ||
		strings.Contains(lower, "replace the image")
}

// codeMarker JSON code 字段命中（`"code":N` / `"code": N` / `"code":"N"` 形态，
// 与 IsModelBlocked 的 code 判定同容差口径）。lower 须为小写 body。
func codeMarker(lower, code string) bool {
	for _, item := range []string{
		`"code":` + code,
		`"code": ` + code,
		`"code":"` + code + `"`,
		`"code": "` + code + `"`,
		`"code":" ` + code + `"`,
		`"code": '` + code + `'`,
	} {
		if strings.Contains(lower, item) {
			return true
		}
	}

	return false
}

// BusinessFrameError 报告 SSE 数据帧是否是**顶层**业务错误信封
// `{"code":N,"msg":"..."}`（N 非 0）。上游部分故障不走 `{"error":...}`，
// 而是 200 + 顶层 code/msg；不识别会被白名单洗成空 chunk 或空成功（issue #244）。
// 只认同时具备非零 code 与 msg、且没有 choices/delta 的形态，避免误伤正常分片。
func BusinessFrameError(payload string) (code, msg string, ok bool) {
	var frame map[string]any
	if json.Unmarshal([]byte(payload), &frame) != nil {
		return "", "", false
	}

	if _, hasErr := frame["error"]; hasErr {
		return "", "", false
	}

	if _, hasChoices := frame["choices"]; hasChoices {
		return "", "", false
	}

	rawCode, hasCode := frame["code"]
	if !hasCode {
		return "", "", false
	}

	code = firstJSONCode(mustRaw(rawCode))
	if code == "" || code == "0" {
		return "", "", false
	}

	msg = strings.TrimSpace(stringValueAny(frame["msg"]))
	if msg == "" {
		msg = strings.TrimSpace(stringValueAny(frame["message"]))
	}

	if msg == "" {
		return "", "", false
	}

	return code, msg, true
}

// mustRaw 把已解码值重新编码为 json.RawMessage（仅用于复用 firstJSONCode）。
func mustRaw(value any) json.RawMessage {
	//nolint:errchkjson // value 来自已解码 JSON，Marshal 不会失败。
	raw, _ := json.Marshal(value)

	return raw
}

// stringValueAny 读取任意 JSON 标量的字符串形态（数字/字符串）。
func stringValueAny(value any) string {
	switch v := value.(type) {
	case string:
		return v
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	default:
		return ""
	}
}

// BusinessErrorFrame 把顶层业务错误包成 OpenAI error 帧（保留原始 code/msg）。
func BusinessErrorFrame(payload, code, msg string) string {
	out, err := json.Marshal(map[string]any{
		"error": map[string]any{
			"message": msg, "type": "upstream_error", "code": code,
			"upstream_payload": json.RawMessage(payload),
		},
	})
	if err != nil {
		return payload
	}

	return string(out)
}
