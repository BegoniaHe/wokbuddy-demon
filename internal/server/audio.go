// audio.go POST /v1/audio/speech 与 POST /v1/audio/transcriptions：官方语音端点
// 的 OpenAI 兼容出口（issue #604）。
//
// 默认关闭（features.tts_enabled / features.asr_enabled，各自独立、热生效）。开启后：
//   - 鉴权/命名密钥限额复用 withClientAuth；
//   - 模型名只用于取 realm 前缀（上游 TTS 模型固定 hunyuan-tts-2.5，ASR 端点没有
//     model 字段），不参与模型目录 / models.hidden / 密钥模型白名单判定——语音不是
//     chat 模型，拿聊天模型门禁卡它是错的；
//   - 单次尝试，不换号（与出图 / 出视频同口径；401 同号 refresh 重放一次）；
//   - TTS 只回 wav / pcm：上游只产 24kHz s16le 裸 PCM，声称 mp3 等于撒谎；
//   - ASR 在入口按 3MB 拦（上游硬上限），超出回 413 且不打上游。
package server

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"time"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
	"github.com/BegoniaHe/wokbuddy-demon/internal/jsonx"
	"github.com/BegoniaHe/wokbuddy-demon/internal/session"
	"github.com/BegoniaHe/wokbuddy-demon/internal/upstream"
)

// 对外的 response_format：上游只产裸 PCM，故只支持这两种（wav = PCM + RIFF 头）。
const (
	audioFormatWAV = "wav"
	audioFormatPCM = "pcm"
)

// audioSpeechHandler TTS 入口（鉴权由 withClientAuth 负责）。
func (h *Handler) audioSpeechHandler(w http.ResponseWriter, r *http.Request) {
	h.serveSpeech(w, r)
}

// audioTranscriptionsHandler ASR 入口（鉴权由 withClientAuth 负责）。
func (h *Handler) audioTranscriptionsHandler(w http.ResponseWriter, r *http.Request) {
	h.serveTranscription(w, r)
}

// audioDisabled 关闭时的 403：端点存在但未开放（不是 404——404 会让客户端以为
// 网关根本不支持语音，转而去直连上游）。
func audioDisabled(w http.ResponseWriter, r *http.Request, feature string) {
	writeOpenAIErrorHint(w, http.StatusForbidden, "endpoint_disabled",
		"audio endpoint is disabled; enable features."+feature+" to use POST "+r.URL.Path,
		"the gateway supports this endpoint but it is not opened on this deployment")
}

// speechRequest 入站 TTS 请求里网关需要的字段（其余 OpenAI 字段上游没有对应参数）。
type speechRequest struct {
	model  string
	input  string
	voice  string
	format string
}

// readSpeechRequest 读取并校验 TTS 请求体；校验顺序即语义，平铺便于核对拒绝路径。
func (h *Handler) readSpeechRequest(w http.ResponseWriter, r *http.Request) (speechRequest, bool) {
	var body struct {
		Model          string `json:"model"`
		Input          string `json:"input"`
		Voice          string `json:"voice"`
		ResponseFormat string `json:"response_format"`
	}

	raw, ok := h.readVideoBody(w, r)
	if !ok {
		return speechRequest{}, false
	}

	// model 必填只为取 realm 前缀：OpenAI 客户端填 tts-1 / gpt-4o-mini-tts 都可以。
	if jsonx.Unmarshal(raw, &body) != nil || strings.TrimSpace(body.Model) == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "model is required")

		return speechRequest{}, false
	}

	input := strings.TrimSpace(body.Input)
	if input == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "input is required")

		return speechRequest{}, false
	}

	format := strings.ToLower(strings.TrimSpace(body.ResponseFormat))
	if format == "" {
		format = audioFormatWAV
	}

	if format != audioFormatWAV && format != audioFormatPCM {
		writeOpenAIErrorHint(w, http.StatusBadRequest, "invalid_request",
			"response_format "+format+" is not supported; upstream only produces 24kHz PCM",
			"use response_format=wav (RIFF) or pcm")

		return speechRequest{}, false
	}

	return speechRequest{
		model: strings.TrimSpace(body.Model), input: input,
		voice: strings.TrimSpace(body.Voice), format: format,
	}, true
}

// audioAttemptStatus 语音上游错误的跳次状态码（透传给 handleMediaError 用）：
// 非 *upstream.Error 的传输层故障没有上游状态码，按 0 走通用 502 渲染。
func audioAttemptStatus(err error) int {
	if uerr, ok := errors.AsType[*upstream.Error](err); ok {
		return uerr.Status
	}

	return 0
}

// serveSpeech TTS 主体：读请求 → 选号 → 租约 → 刷新 → 上游合成 → 封 WAV / PCM。
func (h *Handler) serveSpeech(w http.ResponseWriter, r *http.Request) {
	if !h.loadLive().TTSEnabled {
		audioDisabled(w, r, "tts_enabled")

		return
	}

	req, ok := h.readSpeechRequest(w, r)
	if !ok {
		return
	}

	realm, bareModel, ok := h.resolveAudioRealm(w, r, req.model)
	if !ok {
		return
	}

	acct, ok := h.acquireAudioAccount(w, realm)
	if !ok {
		return
	}
	defer h.cfg.Pool.Release(acct.UID)

	// 上游只回 PCM 字节，raw 恒为 nil：用量记账只记请求数 + 延迟（无 token 维度）。
	var pcm []byte

	call := func(
		ctx context.Context, acct *auth.Auth, _ []byte, _ string, meta upstream.ChatMeta,
	) (int, []byte, error) {
		out, err := h.cfg.Upstream.Speech(ctx, acct, req.input, req.voice, meta)
		if err != nil {
			return audioAttemptStatus(err), nil, fmt.Errorf("speech: %w", err)
		}

		pcm = out

		return http.StatusOK, nil, nil
	}

	if !h.runAudioAttempt(w, r, acct, realm, bareModel, "speech", call) {
		return
	}

	writeSpeechAudio(w, req.format, pcm)
}

// writeSpeechAudio 按客户端要的格式回音频：wav 套 44 字节 RIFF 头，pcm 原样。
func writeSpeechAudio(w http.ResponseWriter, format string, pcm []byte) {
	if format == audioFormatWAV {
		w.Header().Set("Content-Type", "audio/wav")
		w.WriteHeader(http.StatusOK)
		// 头与 PCM 分两次写出：整段 64MiB 的合成结果再拷贝一份会让并发峰值翻倍。
		_, _ = w.Write(wavHeader(len(pcm)))
		_, _ = w.Write(pcm)

		// RIFF chunk 按偶数对齐：奇数长度的 data 后面必须补一个填充字节。
		if len(pcm)%2 == 1 {
			_, _ = w.Write([]byte{0})
		}

		return
	}

	w.Header().Set("Content-Type", "audio/pcm")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(pcm)
}

// serveTranscription ASR 主体：读 multipart → 选号 → 租约 → 刷新 → 上游识别 → {text}。
func (h *Handler) serveTranscription(w http.ResponseWriter, r *http.Request) {
	if !h.loadLive().ASREnabled {
		audioDisabled(w, r, "asr_enabled")

		return
	}

	upload, ok := readTranscriptionUpload(w, r, h.cfg.MaxBodyBytes)
	if !ok {
		return
	}

	realm, bareModel, ok := h.resolveAudioRealm(w, r, upload.model)
	if !ok {
		return
	}

	acct, ok := h.acquireAudioAccount(w, realm)
	if !ok {
		return
	}
	defer h.cfg.Pool.Release(acct.UID)

	text := ""

	call := func(
		ctx context.Context, acct *auth.Auth, _ []byte, _ string, meta upstream.ChatMeta,
	) (int, []byte, error) {
		out, err := h.cfg.Upstream.Transcribe(ctx, acct, upload.filename, upload.contentType, upload.audio, meta)
		if err != nil {
			return audioAttemptStatus(err), nil, fmt.Errorf("transcription: %w", err)
		}

		text = out

		return http.StatusOK, nil, nil
	}

	if !h.runAudioAttempt(w, r, acct, realm, bareModel, "transcription", call) {
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"text": text})
}

// runAudioAttempt 执行一次语音上游调用（含 401 同号 refresh 重放一次）、逐跳记账
// 与错误处置。返回 true 表示调用成功（业务输出由调用方的闭包捕获）。
//
// 失败处置与出图 / 出视频共用 handleMediaFailure，唯一差异是 preserveStatus：
// 语音的请求级失败要透传上游状态码（未配置音色是上游 500 + code 10000，
// issue #604 验收要求状态码与原文一起透传）。
func (h *Handler) runAudioAttempt(
	w http.ResponseWriter, r *http.Request, acct *auth.Auth, realm, bareModel, noun string, call mediaCall,
) bool {
	meta := upstream.ChatMeta{
		// 与 chat / search / 出图同口径：轮级聚合主键用 UUIDv7 32 hex。
		ConversationRequestID: session.NewUUIDv7Hex(),
		ConversationID:        metaConversationID(r),
	}

	firstStart := time.Now()
	first := mediaAttempt{}
	first.status, first.raw, first.err = call(r.Context(), acct, nil, httpauthClientIP(r, h), meta)
	first.latencyMs = attemptLatencyMs(firstStart)

	final, retried, refreshFail := h.retryMediaAfterRefresh(r, acct, nil, meta, call, first)
	if retried {
		h.recordMediaAttempt(acct, realm, bareModel, first, r)
	}

	h.recordMediaAttempt(acct, realm, bareModel, final, r)

	if h.handleMediaFailure(w, acct, bareModel, final, refreshFail, noun, true) {
		return false
	}

	// 上游实测成功即证明账号可用，清熔断与连败计数（与 chat / 出图同口径）。
	h.cfg.Pool.NoteSuccess(acct.UID)

	return true
}

// resolveAudioRealm 语音端点的出口解析：模型名只用于取 realm 前缀（以及命名密钥
// 出口绑定冲突校验）。不做 models.hidden / models.allowed / 密钥模型白名单判定：
// 语音不是目录内的 chat 模型，拿聊天模型门禁卡它只会误伤。
//
// 模型缺省（ASR 上游没有 model 字段）时取密钥绑定域，否则 cn（与裸模型名默认域一致）。
func (h *Handler) resolveAudioRealm(
	w http.ResponseWriter, r *http.Request, model string,
) (string, string, bool) {
	model = strings.TrimSpace(model)
	keyRealm := keyRealmFrom(r.Context())

	if model == "" {
		if keyRealm != "" {
			return keyRealm, "", true
		}

		return realmCN, "", true
	}

	realm, bareModel, err := resolveKeyModel(model, h.requestModelAliases(model, keyRealm), keyRealm)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", err.Error())

		return "", "", false
	}

	return realm, bareModel, true
}

// acquireAudioAccount 选号 + 在途租约 + 临近过期刷新；失败路径已写出响应。
// 调用方负责 Release 返回的账号。
//
// 选号不带 reqModel：语音请求的 model 是客户端占位名（tts-1 之类），把它交给
// 模型级冷却 / 路由熔断判定只会写出一堆无人读取的状态。
func (h *Handler) acquireAudioAccount(w http.ResponseWriter, realm string) (*auth.Auth, bool) {
	// 号池分区（issue #491）：语音与出图 / 出视频同属媒体面；分区关闭时等价普通选号。
	acct := h.cfg.Pool.PickExcludingForRealmMedia(map[string]bool{}, "", realm, true)
	if acct == nil {
		writeOpenAIErrorHint(w, http.StatusServiceUnavailable, "no_healthy_account",
			"all accounts are temporarily unavailable, please retry later", upstream.NoHealthyAccountHint())

		return nil, false
	}

	if !h.cfg.Pool.Acquire(acct.UID) {
		writeOpenAIError(
			w, http.StatusServiceUnavailable, "no_healthy_account", "account is busy, please retry later",
		)

		return nil, false
	}

	if !h.ensureMediaToken(w, acct, "audio") {
		h.cfg.Pool.Release(acct.UID)

		return nil, false
	}

	return acct, true
}

// transcriptionUpload 入站转写请求里网关需要的三样东西（其余表单字段上游不认）。
type transcriptionUpload struct {
	model       string
	filename    string
	contentType string
	audio       []byte
}

// errAudioPartTooLarge 音频分片超过上游 3MB 上限。
var errAudioPartTooLarge = errors.New("audio file exceeds the 3MB upstream limit")

// readTranscriptionUpload 流式读入站 multipart，只取音频分片并**在入口**按上游
// 3MB 上限拦。不先把整个 body 读进内存：3MB 上限之外的字节没有意义，读完只是
// 让超大上传先吃满网关内存。
//
// 字段名同时接受 audio（issue #604 契约）与 file（OpenAI 官方命名）。
func readTranscriptionUpload(
	w http.ResponseWriter, r *http.Request, maxBodyBytes int64,
) (*transcriptionUpload, bool) {
	// 与其它入口同口径：整体请求体先受 server.max_body_bytes 约束，音频分片另有
	// 3MB 上限（upstream.ASRMaxAudioBytes）。缺这一层时，任意非音频分片都能把
	// 请求体推到任意大，文档化的上限形同虚设。
	limitRequestBody(w, r, maxBodyBytes)

	reader, err := r.MultipartReader()
	if err != nil {
		writeOpenAIErrorHint(w, http.StatusBadRequest, "invalid_request",
			"transcriptions requires a multipart/form-data body",
			"send the audio file as a multipart part named `audio`")

		return nil, false
	}

	upload := &transcriptionUpload{}

	for {
		part, partErr := reader.NextPart()
		if partErr != nil {
			if errors.Is(partErr, io.EOF) {
				break
			}
			// 整体请求体超限由 limitRequestBody 在读取途中抛出，按 413 收口，
			// 不要归成「multipart 形态错误」。
			if isBodyTooLarge(partErr) {
				writeOpenAIErrorHint(w, http.StatusRequestEntityTooLarge, "payload_too_large",
					bodyTooLargeMessage(maxBodyBytes), "")

				return nil, false
			}

			writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "malformed multipart body: "+partErr.Error())

			return nil, false
		}

		if takeErr := upload.takePart(part); takeErr != nil {
			if errors.Is(takeErr, errAudioPartTooLarge) {
				writeOpenAIErrorHint(w, http.StatusRequestEntityTooLarge, "payload_too_large",
					"audio file exceeds the 3MB upstream limit",
					"send a shorter recording or a more compressed format")

				return nil, false
			}

			writeOpenAIError(w, http.StatusBadRequest, "invalid_request", takeErr.Error())

			return nil, false
		}
	}

	if len(upload.audio) == 0 {
		writeOpenAIErrorHint(w, http.StatusBadRequest, "invalid_request",
			"audio file is required", "send the audio file as a multipart part named `audio`")

		return nil, false
	}

	return upload, true
}

// takePart 把单个表单分片收进 upload：音频分片按 3MB 上限读入，model 取第一个，
// 其余字段（language / prompt / temperature 上游没有对应参数）读空丢弃。
func (upload *transcriptionUpload) takePart(part *multipart.Part) error {
	switch name := part.FormName(); name {
	case "audio", "file":
		// 两个名字都给时先到先用，避免后一个覆盖已读出的音频；空分片不算占用，
		// 否则 0 字节的 `audio` 会吃掉后面有效的 `file` 分片。
		if len(upload.audio) > 0 {
			_, _ = io.Copy(io.Discard, part)

			return nil
		}

		raw, err := io.ReadAll(io.LimitReader(part, upstream.ASRMaxAudioBytes+1))
		if err != nil {
			return fmt.Errorf("read audio part: %w", err)
		}

		if len(raw) > upstream.ASRMaxAudioBytes {
			return errAudioPartTooLarge
		}

		upload.audio, upload.filename, upload.contentType = raw, part.FileName(), part.Header.Get("Content-Type")
	case "model":
		raw, _ := io.ReadAll(io.LimitReader(part, 256))
		upload.model = strings.TrimSpace(string(raw))
	default:
		// 仍要把分片读空，否则后续 NextPart 会卡在这个分片上。
		_, _ = io.Copy(io.Discard, part)
	}

	return nil
}

// wavHeader 生成 24kHz 单声道 s16le 裸 PCM 的 44 字节 RIFF 头（上游只产裸 PCM，
// 浏览器 / 播放器要容器才认）。字段按标准 WAV 头声明，无附加 chunk。
//
// pcmLen 为奇数时 RIFF 要求 data chunk 后补一个 0 字节：大小字段按「数据 + 填充」
// 记账，否则严格解析器会把末尾读越界。
func wavHeader(pcmLen int) []byte {
	const (
		wavHeaderBytes  = 44
		pcmBits         = 16
		pcmChannels     = 1
		pcmFormatLinear = 1
	)

	blockAlign := pcmChannels * pcmBits / 8
	byteRate := upstream.TTSSampleRate * blockAlign
	dataBytes := pcmLen + pcmLen%2

	// 长度按 int 计算后再落盘：pcm 已由 upstream 侧限到 64MiB，不会溢出 uint32。
	out := make([]byte, wavHeaderBytes)

	copy(out[0:4], "RIFF")
	binary.LittleEndian.PutUint32(out[4:8], uint32(wavHeaderBytes-8+dataBytes)) //nolint:gosec // pcm 由上游限到 64MiB
	copy(out[8:12], "WAVE")
	copy(out[12:16], "fmt ")
	binary.LittleEndian.PutUint32(out[16:20], 16) // fmt chunk 长度
	binary.LittleEndian.PutUint16(out[20:22], pcmFormatLinear)
	binary.LittleEndian.PutUint16(out[22:24], pcmChannels)
	binary.LittleEndian.PutUint32(out[24:28], uint32(upstream.TTSSampleRate))
	binary.LittleEndian.PutUint32(out[28:32], uint32(byteRate))
	binary.LittleEndian.PutUint16(out[32:34], uint16(blockAlign))
	binary.LittleEndian.PutUint16(out[34:36], pcmBits)
	copy(out[36:40], "data")
	binary.LittleEndian.PutUint32(out[40:44], uint32(pcmLen)) //nolint:gosec // pcm 由上游限到 64MiB

	return out
}
