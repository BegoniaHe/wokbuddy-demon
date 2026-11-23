// speech.go 官方语音端点：文本转语音与语音转文本（issue #604）。
//
// 上游契约来自官方桌面端包内 `main/server.js`（cn 5.11.3 / global 5.11.4 同形）：
//
//	POST {chatBase}/v2/audio/speech    body {model:"hunyuan-tts-2.5", stream:true,
//	                                  speech:{input, voice, response_format:"pcm", sample_rate:24000}}
//	                                  回 SSE：speech.audio.delta（base64 PCM 分片）/ speech.audio.done
//	POST {chatBase}/agenttool/v1/asr   multipart 字段 audio → {code,text}
//
// 两个端点都不在 /v1/models 目录里（独立端点，不是 chat 模型），故上游模型名固定，
// 网关不转发客户端的占位模型名（OpenAI 客户端填的 tts-1 只用来取 realm 前缀）。
//
// 与 chat 的差异同 images.go / webfetch.go：独立请求/响应，不套 chat 的
// sanitize/thinking/prepareBody 管线；选号/轮转/惩罚在 server 层（upstream 层不感知账号池）。
package upstream

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"strings"
	"time"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
	"github.com/BegoniaHe/wokbuddy-demon/internal/jsonx"
)

const (
	// SpeechPath 文本转语音（SSE + base64 PCM 分片）。
	SpeechPath = "/v2/audio/speech"
	// ASRPath 语音转文本（multipart 字段 audio，非流式；WS 流式不在本仓范围）。
	ASRPath = "/agenttool/v1/asr"

	// TTSModel 官方客户端写死的 TTS 模型名（不在模型目录里）。
	TTSModel = "hunyuan-tts-2.5"
	// TTSDefaultVoice 官方默认音色；客户端不传 voice 时用它。音色表是上游全局配置，
	// 只有已配置的音色可用（未配置的音色上游回 500 + code 10000）。
	TTSDefaultVoice = "f191_female_cn_tts2.5_flash"
	// TTSSampleRate 官方固定 24kHz；回程 PCM 是 s16le 单声道，套 WAV 头必须用它。
	TTSSampleRate = 24000

	// ASRMaxAudioBytes 上游 ASR 的硬上限 3MB（实测 2.9MB 通过、3.1MB 回 400 +
	// code 15052「ensure file size is under 3MB」）。在入站入口按它拦，别把尺寸
	// 问题交给上游判成 multipart 解析失败。
	ASRMaxAudioBytes = 3 << 20

	// speechTimeout / asrTimeout 单次上游调用的整体超时：两者都是同步请求，
	// 上游 hang 时若没有超时会一直占着账号在途租约。
	speechTimeout = 120 * time.Second
	asrTimeout    = 60 * time.Second

	// speechMaxAudioBytes 回程 PCM 上限：24kHz s16le 约 48KB/s，64 MiB 已远超任何
	// 单次合成（防异常上游把内存打满）。
	speechMaxAudioBytes = 64 << 20

	// speechMaxErrorBytes / asrMaxResponseBytes 错误体与 ASR 回执上限。
	speechMaxErrorBytes  = 1 << 20
	asrMaxResponseBytes  = 1 << 20
	asrMultipartOverhead = 1 << 20 // multipart 边界/表单头的余量（不计入 3MB 音频上限）
)

// ErrSpeechEmptyAudio 上游 SSE 正常收尾但一片音频都没给：上游缺陷，换号不会变。
var ErrSpeechEmptyAudio = errors.New("tts stream completed without audio")

// speechRequestBody TTS 出站请求体（字段顺序与官方客户端一致）。
type speechRequestBody struct {
	Model  string       `json:"model"`
	Stream bool         `json:"stream"`
	Speech speechParams `json:"speech"`
}

// speechParams 官方 `speech` 段：input 必填，其余取官方固定值。
type speechParams struct {
	Input          string `json:"input"`
	Voice          string `json:"voice"`
	ResponseFormat string `json:"response_format"`
	SampleRate     int    `json:"sample_rate"`
}

// speechFrame 上游 SSE 的单个 data 帧。speech.audio.* 之外只认 error 帧——上游
// 会在流里回 `{"type":"error","error":{...}}`，不认就会一路掉到
// 「completed without audio」，把真正原因咽掉。
type speechFrame struct {
	Type  string `json:"type"`
	Audio struct {
		Data string `json:"data"`
	} `json:"audio"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// Speech 对指定账号合成一次语音，返回 PCM（s16le 单声道，采样率 TTSSampleRate）。
//
// 只返回 PCM：封装格式（wav / pcm）属于对外契约，由 server 层按客户端要的
// response_format 决定；upstream 层不替客户端选择容器。
//
//nolint:gocritic // 公开 API 值传递与 ChatHeaders 同一口径。
func (c *Client) Speech(
	ctx context.Context, account *auth.Auth, text, voice string, meta ChatMeta,
) ([]byte, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, &Error{Kind: ErrBadParams, Msg: "speech input is required"}
	}

	voice = strings.TrimSpace(voice)
	if voice == "" {
		voice = TTSDefaultVoice
	}

	body, err := jsonx.Marshal(speechRequestBody{
		Model: TTSModel, Stream: true,
		Speech: speechParams{
			Input: text, Voice: voice, ResponseFormat: "pcm", SampleRate: TTSSampleRate,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("speech marshal: %w", err)
	}

	ctx, cancel := context.WithTimeout(ctx, speechTimeout)
	defer cancel()

	req, err := c.newAgentToolRequest(ctx, account, SpeechPath, body, surfaceSpeech, meta)
	if err != nil {
		return nil, fmt.Errorf("speech request: %w", err)
	}

	// 回程是 SSE：头族给的是 application/json（真机 chat 也不改），这里按端点形态
	// 声明 event-stream，让中间层（Caddy / APISIX）不缓冲整段合成结果。
	req.Header.Set("Accept", "text/event-stream")

	resp, err := c.HTTPForAccount(account).Do(req)
	if err != nil {
		return nil, fmt.Errorf("speech do: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= http.StatusBadRequest {
		return nil, speechHTTPError(resp)
	}

	return readSpeechStream(resp.Body)
}

// speechHTTPError 把 ≥400 的 TTS 响应翻成带权威分类的 *Error（与 agenttool 同口径）。
func speechHTTPError(resp *http.Response) error {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, speechMaxErrorBytes))

	return &Error{
		Kind: Classify(resp.StatusCode, string(raw)), Status: resp.StatusCode,
		Msg: truncate(string(raw), 200), Code: businessCode(string(raw)),
	}
}

// readSpeechStream 逐行读 SSE，拼 speech.audio.delta 的 base64 PCM 分片。
func readSpeechStream(body io.Reader) ([]byte, error) {
	reader := bufio.NewReaderSize(body, 64<<10)

	pcm := make([]byte, 0, 64<<10)

	for {
		line, readErr := ReadSSELine(reader)

		next, done, frameErr := appendSpeechFrame(pcm, line)
		if frameErr != nil {
			return nil, frameErr
		}

		pcm = next

		if len(pcm) > speechMaxAudioBytes {
			return nil, fmt.Errorf("%w: > %d bytes", ErrSSEAggregateTooLong, speechMaxAudioBytes)
		}

		if done {
			break
		}

		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				break
			}

			return nil, fmt.Errorf("speech read: %w", readErr)
		}
	}

	if len(pcm) == 0 {
		return nil, ErrSpeechEmptyAudio
	}

	return pcm, nil
}

// appendSpeechFrame 处理一行 SSE：非 data 行原样放过，data 行解出本帧音频与收尾标记。
func appendSpeechFrame(pcm []byte, line string) ([]byte, bool, error) {
	payload, ok := sseDataPayload(line)
	if !ok {
		return pcm, false, nil
	}

	chunk, done, err := speechChunk(strings.TrimSpace(payload))
	if err != nil {
		return nil, false, err
	}

	return append(pcm, chunk...), done, nil
}

// speechChunk 解析一帧：done 报告是否收尾，chunk 是本帧音频字节。
// error 帧与非法 base64 一律作为错误返回（不咽成空音频）。
func speechChunk(payload string) ([]byte, bool, error) {
	if payload == "" {
		return nil, false, nil
	}

	if payload == "[DONE]" {
		return nil, true, nil
	}

	var frame speechFrame
	if err := jsonx.Unmarshal([]byte(payload), &frame); err != nil {
		return nil, false, fmt.Errorf("speech frame is not json: %w", err)
	}

	if frame.Error != nil {
		kind := Classify(http.StatusBadGateway, payload)
		if kind == ErrNone {
			kind = ErrServer
		}

		return nil, false, &Error{
			Kind: kind, Status: http.StatusBadGateway,
			Msg: truncate(payload, 200), Code: businessCode(payload),
		}
	}

	switch frame.Type {
	case "speech.audio.delta":
		chunk, err := base64.StdEncoding.DecodeString(frame.Audio.Data)
		if err != nil {
			return nil, false, fmt.Errorf("speech audio delta is not base64: %w", err)
		}

		return chunk, false, nil
	case "speech.audio.done":
		return nil, true, nil
	default:
		// 未知事件（含上游新增的进度帧）：忽略，不当作音频也不当作错误。
		return nil, false, nil
	}
}

// Transcribe 对指定账号执行一次非流式语音识别，返回上游识别文本。
//
// 上游要 multipart 字段 `audio`（带文件名与 mime），回 {code,text}；code 非 0 即失败。
// 音频字节原样转发，网关不转码——转码只引入失真与依赖，上游按容器嗅探格式。
//
//nolint:gocritic // 公开 API 值传递与 ChatHeaders 同一口径。
func (c *Client) Transcribe(
	ctx context.Context, account *auth.Auth, filename, contentType string, audio []byte, meta ChatMeta,
) (string, error) {
	body, formType, err := asrMultipart(filename, contentType, audio)
	if err != nil {
		return "", err
	}

	ctx, cancel := context.WithTimeout(ctx, asrTimeout)
	defer cancel()

	req, err := c.newAgentToolRequest(ctx, account, ASRPath, body, surfaceASR, meta)
	if err != nil {
		return "", fmt.Errorf("asr request: %w", err)
	}

	// 必须排在头族之后再设一次：CommonHeaders 给所有请求写死
	// `Content-Type: application/json`，multipart 的 boundary 会被它抹掉，上游只能报
	// 15052「failed to parse multipart form」（真号实测）。
	req.Header.Set("Content-Type", formType)

	resp, err := c.HTTPForAccount(account).Do(req)
	if err != nil {
		return "", fmt.Errorf("asr do: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, asrMaxResponseBytes+1))
	if err != nil {
		return "", fmt.Errorf("asr read: %w", err)
	}

	if len(raw) > asrMaxResponseBytes {
		return "", fmt.Errorf("%w: > %d bytes", ErrSSEAggregateTooLong, asrMaxResponseBytes)
	}

	return parseASRResponse(resp.StatusCode, raw)
}

// parseASRResponse 解析 ASR 回执：非 2xx 或 code 非 0 一律按失败返回（code 非 0 时
// 上游可能仍回 200，不能当成功解 text）。
func parseASRResponse(status int, raw []byte) (string, error) {
	if status >= http.StatusBadRequest {
		return "", asrError(status, raw)
	}

	var envelope struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Text string `json:"text"`
	}
	if err := jsonx.Unmarshal(raw, &envelope); err != nil {
		return "", fmt.Errorf("asr response is not json: %w (body: %s)",
			err, RedactForLog(string(raw), 120))
	}

	if envelope.Code != 0 {
		return "", asrError(status, raw)
	}

	return envelope.Text, nil
}

// asrError 把 ASR 失败响应翻成带权威分类的 *Error：Msg 保留上游原文（含业务码），
// 换任何账号都是同一句，所以分类会把 15053 之类归到请求级（见 Classify）。
func asrError(status int, raw []byte) error {
	kind := Classify(status, string(raw))
	if kind == ErrNone {
		kind = ErrClient
	}

	return &Error{
		Kind: kind, Status: status,
		Msg: truncate(string(raw), 200), Code: businessCode(string(raw)),
	}
}

// asrMultipart 构造 ASR 的 multipart body，返回 body 字节与带 boundary 的 Content-Type。
func asrMultipart(filename, contentType string, audio []byte) ([]byte, string, error) {
	if strings.TrimSpace(filename) == "" {
		filename = "audio.wav"
	}

	if strings.TrimSpace(contentType) == "" {
		contentType = mime.TypeByExtension(extWithDot(filename))
		if contentType == "" {
			contentType = "application/octet-stream"
		}
	}

	var buf bytes.Buffer

	writer := multipart.NewWriter(&buf)

	header := textproto.MIMEHeader{}
	header.Set("Content-Disposition", mime.FormatMediaType("form-data", map[string]string{
		"name": "audio", "filename": filename,
	}))
	header.Set("Content-Type", contentType)

	part, err := writer.CreatePart(header)
	if err != nil {
		return nil, "", fmt.Errorf("asr multipart part: %w", err)
	}

	if _, err := part.Write(audio); err != nil {
		return nil, "", fmt.Errorf("asr multipart write: %w", err)
	}

	if err := writer.Close(); err != nil {
		return nil, "", fmt.Errorf("asr multipart close: %w", err)
	}

	return buf.Bytes(), writer.FormDataContentType(), nil
}

// extWithDot 取文件扩展名（含点，小写）；无扩展名返回空串。
func extWithDot(filename string) string {
	if i := strings.LastIndexByte(filename, '.'); i >= 0 {
		return strings.ToLower(filename[i:])
	}

	return ""
}
