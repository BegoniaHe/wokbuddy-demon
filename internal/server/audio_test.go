package server

import (
	"bytes"
	"encoding/binary"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
	"github.com/BegoniaHe/wokbuddy-demon/internal/jsonx"
	"github.com/BegoniaHe/wokbuddy-demon/internal/livecfg"
	"github.com/BegoniaHe/wokbuddy-demon/internal/upstream"
	"github.com/BegoniaHe/wokbuddy-demon/internal/usage"
)

// sseUpstream 构造一个 SSE 响应（TTS 回程形态）。
func sseUpstream(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

// audioHandler 构造语音端点测试用的 handler（单号 + 假上游）。
func audioHandler(t *testing.T, client *upstream.Client, snap livecfg.Snapshot, usageRec *usage.Recorder) *Handler {
	t.Helper()

	return NewHandler(&Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: client,
		Usage:    usageRec,
		Live:     livecfg.New(snap),
	})
}

// audioFakeClient 假上游：behavior 收到请求后返回响应。
func audioFakeClient(behavior func(*http.Request) *http.Response) *upstream.Client {
	return &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			return behavior(r), nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}
}

// TestAudioDisabledReturns403 开关关闭时两条路径都是 403（端点存在但未开放，不是
// 404），且不出网。
func TestAudioDisabledReturns403(t *testing.T) {
	t.Parallel()

	called := false
	client := audioFakeClient(func(*http.Request) *http.Response {
		called = true
		return jsonResponse(http.StatusOK, `{}`)
	})

	handler := audioHandler(t, client, livecfg.Snapshot{}, nil)

	for _, path := range []string{"/v1/audio/speech", "/v1/audio/transcriptions"} {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path,
			strings.NewReader(`{"model":"tts-1","input":"hello"}`)))

		if rec.Code != http.StatusForbidden {
			t.Fatalf("%s code=%d body=%s want 403", path, rec.Code, rec.Body)
		}
	}

	if called {
		t.Fatal("开关关闭时不得出网")
	}

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/audio/speech",
		strings.NewReader(`{"model":"tts-1","input":"hello"}`)))

	if !strings.Contains(rec.Body.String(), "features.tts_enabled") {
		t.Fatalf("body=%s want 提到 features.tts_enabled", rec.Body)
	}
}

// TestAudioSpeechReturnsPlayableWAV 占位模型名（tts-1）即可工作：模型名只用于取
// realm 前缀，上游模型固定 hunyuan-tts-2.5；回程是带正确 RIFF 头的 WAV。
//
//nolint:cyclop // 出站契约 + WAV 头逐字段线性断言，拆开反而丢掉请求与回程的对照关系。
func TestAudioSpeechReturnsPlayableWAV(t *testing.T) {
	t.Parallel()

	var (
		gotPath, gotAccept, gotContentType string
		gotBody                            []byte
	)

	client := audioFakeClient(func(r *http.Request) *http.Response {
		gotPath = r.URL.Path
		gotAccept = r.Header.Get("Accept")
		gotContentType = r.Header.Get("Content-Type")
		gotBody, _ = io.ReadAll(r.Body)

		return sseUpstream(http.StatusOK,
			`data: {"type":"speech.audio.delta","audio":{"data":"AAEC"}}`+"\n\n"+
				`data: {"type":"speech.audio.delta","audio":{"data":"AwQF"}}`+"\n\n"+
				`data: {"type":"speech.audio.done"}`+"\n\n")
	})

	usageRec := usage.New("")
	handler := audioHandler(t, client, livecfg.Snapshot{TTSEnabled: true}, usageRec)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/audio/speech",
		strings.NewReader(`{"model":"tts-1","input":"你好"}`)))

	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}

	if gotPath != "/v2/audio/speech" {
		t.Fatalf("upstream path=%s want /v2/audio/speech", gotPath)
	}

	if gotAccept != "text/event-stream" || gotContentType != "application/json" {
		t.Fatalf("accept=%q content-type=%q", gotAccept, gotContentType)
	}

	var sent struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
		Speech struct {
			Input          string `json:"input"`
			Voice          string `json:"voice"`
			ResponseFormat string `json:"response_format"`
			SampleRate     int    `json:"sample_rate"`
		} `json:"speech"`
	}
	if err := jsonx.Unmarshal(gotBody, &sent); err != nil {
		t.Fatalf("body not json: %v (%s)", err, gotBody)
	}

	if sent.Model != "hunyuan-tts-2.5" || !sent.Stream || sent.Speech.Voice != "f191_female_cn_tts2.5_flash" ||
		sent.Speech.Input != "你好" || sent.Speech.ResponseFormat != "pcm" || sent.Speech.SampleRate != 24000 {
		t.Fatalf("upstream body=%s", gotBody)
	}

	if ct := rec.Header().Get("Content-Type"); ct != "audio/wav" {
		t.Fatalf("content-type=%q want audio/wav", ct)
	}

	// WAV 头必须是 44 字节 RIFF + 24000Hz 单声道 16bit，data 段是拼好的 PCM。
	wav := rec.Body.Bytes()
	if len(wav) != 44+6 || string(wav[0:4]) != "RIFF" || string(wav[8:12]) != "WAVE" ||
		string(wav[12:16]) != "fmt " || string(wav[36:40]) != "data" {
		t.Fatalf("wav header malformed: %q", wav)
	}

	if got := binary.LittleEndian.Uint32(wav[4:8]); got != uint32(len(wav)-8) {
		t.Errorf("riff size=%d want %d", got, len(wav)-8)
	}

	if got := binary.LittleEndian.Uint16(wav[22:24]); got != 1 {
		t.Errorf("channels=%d want 1", got)
	}

	if got := binary.LittleEndian.Uint32(wav[24:28]); got != 24000 {
		t.Errorf("sample rate=%d want 24000", got)
	}

	if got := binary.LittleEndian.Uint16(wav[34:36]); got != 16 {
		t.Errorf("bits per sample=%d want 16", got)
	}

	if got := binary.LittleEndian.Uint32(wav[40:44]); got != 6 {
		t.Errorf("data size=%d want 6", got)
	}

	if !bytes.Equal(wav[44:], []byte{0, 1, 2, 3, 4, 5}) {
		t.Errorf("pcm payload=%v want 00..05", wav[44:])
	}

	// 语音路径必须进用量账本（无 token 维度：只记请求数 + 延迟）。
	if totals := usageRec.Snapshot(24, nil).Totals; totals.Requests != 1 || totals.Errors != 0 {
		t.Errorf("requests=%d errors=%d want 1/0", totals.Requests, totals.Errors)
	}
}

// TestAudioSpeechPCMFormat 要 pcm 时原样回裸 PCM，不套容器。
func TestAudioSpeechPCMFormat(t *testing.T) {
	t.Parallel()

	client := audioFakeClient(func(*http.Request) *http.Response {
		return sseUpstream(http.StatusOK, `data: {"type":"speech.audio.delta","audio":{"data":"AAEC"}}`+"\n\n")
	})

	handler := audioHandler(t, client, livecfg.Snapshot{TTSEnabled: true}, nil)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/audio/speech",
		strings.NewReader(`{"model":"tts-1","input":"hi","response_format":"pcm"}`)))

	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}

	if ct := rec.Header().Get("Content-Type"); ct != "audio/pcm" {
		t.Fatalf("content-type=%q want audio/pcm", ct)
	}

	if !bytes.Equal(rec.Body.Bytes(), []byte{0, 1, 2}) {
		t.Fatalf("raw pcm=%v", rec.Body.Bytes())
	}
}

// TestAudioSpeechRejectsMP3 mp3 必须明确拒绝：上游只产 PCM，回车 mp3 等于撒谎。
func TestAudioSpeechRejectsMP3(t *testing.T) {
	t.Parallel()

	called := false
	client := audioFakeClient(func(*http.Request) *http.Response {
		called = true
		return sseUpstream(http.StatusOK, "")
	})

	handler := audioHandler(t, client, livecfg.Snapshot{TTSEnabled: true}, nil)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/audio/speech",
		strings.NewReader(`{"model":"tts-1","input":"hi","response_format":"mp3"}`)))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}

	if called {
		t.Fatal("拒绝的格式不得打上游")
	}

	if !strings.Contains(rec.Body.String(), "wav") || !strings.Contains(rec.Body.String(), "pcm") {
		t.Fatalf("body=%s want 指出可用格式", rec.Body)
	}
}

// TestAudioSpeechVoiceMissingIsRequestLevel 音色未配置是上游 500 + code 10000：
// 状态码与原文透传、hint 点名默认音色，且不罚号（换任何号都是同一句）。
func TestAudioSpeechVoiceMissingIsRequestLevel(t *testing.T) {
	t.Parallel()

	calls := 0
	client := audioFakeClient(func(*http.Request) *http.Response {
		calls++

		return jsonResponse(http.StatusInternalServerError,
			`{"code":10000,"msg":"get voice config failed: voice x not found in Rainbow"}`)
	})

	state := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	handler := NewHandler(&Config{
		Pool:     state,
		Upstream: client,
		Live:     livecfg.New(livecfg.Snapshot{TTSEnabled: true}),
	})

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/audio/speech",
		strings.NewReader(`{"model":"tts-1","input":"hi","voice":"not-configured"}`)))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("code=%d body=%s want 500（请求级失败透传上游状态码）", rec.Code, rec.Body)
	}

	if calls != 1 {
		t.Fatalf("upstream calls=%d want 1（请求级失败不轮转）", calls)
	}

	var parsed struct {
		Error struct {
			Message string `json:"message"`
			Code    string `json:"code"`
			Hint    string `json:"gateway_hint"`
		} `json:"error"`
	}
	if err := jsonx.Unmarshal(rec.Body.Bytes(), &parsed); err != nil {
		t.Fatalf("body not json: %v (%s)", err, rec.Body)
	}

	if !strings.Contains(parsed.Error.Message, "get voice config failed") {
		t.Errorf("message=%q want 上游原文", parsed.Error.Message)
	}

	if !strings.Contains(parsed.Error.Hint, "f191_female_cn_tts2.5_flash") {
		t.Errorf("hint=%q want 点名默认音色", parsed.Error.Hint)
	}

	status, ok := state.Status("u1")
	if !ok {
		t.Fatal("account missing")
	}

	if status.ConsecutiveFails != 0 || status.BreakerFails != 0 {
		t.Errorf("consecutive_fails=%d breaker_fails=%d want 0/0（请求级失败不罚号）",
			status.ConsecutiveFails, status.BreakerFails)
	}
}

// TestAudioSpeechEmptyStreamIsError 上游正常收尾但一片音频都没有：按错误回，不能
// 回一个空的 200 WAV。
func TestAudioSpeechEmptyStreamIsError(t *testing.T) {
	t.Parallel()

	client := audioFakeClient(func(*http.Request) *http.Response {
		return sseUpstream(http.StatusOK, `data: {"type":"speech.audio.done"}`+"\n\n")
	})

	handler := audioHandler(t, client, livecfg.Snapshot{TTSEnabled: true}, nil)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/audio/speech",
		strings.NewReader(`{"model":"tts-1","input":"hi"}`)))

	if rec.Code < http.StatusBadRequest {
		t.Fatalf("code=%d body=%s want >=400", rec.Code, rec.Body)
	}
}

// transcriptionRequest 构造一个 OpenAI 形态的转写请求（multipart 字段 audio）。
func transcriptionRequest(t *testing.T, filename string, audio []byte) *http.Request {
	t.Helper()

	var buf bytes.Buffer

	writer := multipart.NewWriter(&buf)

	part, err := writer.CreateFormFile("audio", filename)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := part.Write(audio); err != nil {
		t.Fatal(err)
	}

	if err := writer.WriteField("model", "whisper-1"); err != nil {
		t.Fatal(err)
	}

	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions", &buf)
	req.Header.Set("Content-Type", writer.FormDataContentType())

	return req
}

// TestAudioTranscriptionsReturnsText 入站 audio 文件字段 → 出站带 boundary 的
// multipart（同名字段）→ {code:0,text} 翻成 OpenAI 的 {text}。
//
//nolint:cyclop // 出站 multipart 契约 + 入站响应逐项断言。
func TestAudioTranscriptionsReturnsText(t *testing.T) {
	t.Parallel()

	var (
		gotPath, gotContentType, gotField, gotFilename string
		gotAudio                                       string
	)

	client := audioFakeClient(func(r *http.Request) *http.Response {
		gotPath = r.URL.Path
		gotContentType = r.Header.Get("Content-Type")

		reader, err := r.MultipartReader()
		if err != nil {
			t.Errorf("出站不是 multipart：%v（Content-Type=%q）", err, gotContentType)
			return jsonResponse(http.StatusBadRequest, `{"code":15052}`)
		}

		for {
			part, partErr := reader.NextPart()
			if partErr != nil {
				break
			}

			if part.FormName() == "audio" {
				gotField, gotFilename = part.FormName(), part.FileName()
				raw, _ := io.ReadAll(part)
				gotAudio = string(raw)
			}
		}

		return jsonResponse(http.StatusOK, `{"code":0,"text":"识别结果"}`)
	})

	usageRec := usage.New("")
	handler := audioHandler(t, client, livecfg.Snapshot{ASREnabled: true}, usageRec)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, transcriptionRequest(t, "a.wav", []byte("RIFFdata")))

	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}

	if gotPath != "/agenttool/v1/asr" {
		t.Fatalf("upstream path=%s want /agenttool/v1/asr", gotPath)
	}

	if !strings.HasPrefix(gotContentType, "multipart/form-data; boundary=") {
		t.Fatalf("outbound Content-Type=%q 必须带 boundary（被头族抹成 JSON 了）", gotContentType)
	}

	if gotField != "audio" || gotFilename != "a.wav" || gotAudio != "RIFFdata" {
		t.Errorf("field=%q filename=%q audio=%q", gotField, gotFilename, gotAudio)
	}

	var parsed struct {
		Text string `json:"text"`
	}
	if err := jsonx.Unmarshal(rec.Body.Bytes(), &parsed); err != nil {
		t.Fatalf("body not json: %v (%s)", err, rec.Body)
	}

	if parsed.Text != "识别结果" {
		t.Fatalf("text=%q", parsed.Text)
	}

	if totals := usageRec.Snapshot(24, nil).Totals; totals.Requests != 1 {
		t.Errorf("requests=%d want 1", totals.Requests)
	}
}

// TestAudioTranscriptionsOversize413 超过 3MB 在入口回 413，不产生上游请求。
func TestAudioTranscriptionsOversize413(t *testing.T) {
	t.Parallel()

	called := false
	client := audioFakeClient(func(*http.Request) *http.Response {
		called = true
		return jsonResponse(http.StatusOK, `{"code":0,"text":"x"}`)
	})

	handler := audioHandler(t, client, livecfg.Snapshot{ASREnabled: true}, nil)

	oversize := make([]byte, upstream.ASRMaxAudioBytes+1)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, transcriptionRequest(t, "big.wav", oversize))

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("code=%d body=%s want 413", rec.Code, rec.Body)
	}

	if called {
		t.Fatal("超限请求不得打上游")
	}
}

// TestAudioTranscriptionsBadAudioIsRequestLevel 上游 15053（认不出这段音频）
// 请求级：状态码与原文透传、不换号、不喂连败。
func TestAudioTranscriptionsBadAudioIsRequestLevel(t *testing.T) {
	t.Parallel()

	calls := 0
	client := audioFakeClient(func(*http.Request) *http.Response {
		calls++
		return jsonResponse(http.StatusBadRequest, `{"code":15053,"msg":"ASR recognition failed"}`)
	})

	state := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	handler := NewHandler(&Config{
		Pool:     state,
		Upstream: client,
		Live:     livecfg.New(livecfg.Snapshot{ASREnabled: true}),
	})

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, transcriptionRequest(t, "a.pcm", []byte("raw")))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d body=%s want 400（透传上游状态码）", rec.Code, rec.Body)
	}

	if calls != 1 {
		t.Fatalf("upstream calls=%d want 1（请求级失败不轮转）", calls)
	}

	if !strings.Contains(rec.Body.String(), "15053") {
		t.Fatalf("body=%s want 上游原文（含业务码）", rec.Body)
	}

	if !strings.Contains(rec.Body.String(), "not raw PCM") {
		t.Errorf("body=%s want hint 指出裸 PCM", rec.Body)
	}

	status, ok := state.Status("u1")
	if !ok {
		t.Fatal("account missing")
	}

	if status.ConsecutiveFails != 0 || status.BreakerFails != 0 {
		t.Errorf("consecutive_fails=%d breaker_fails=%d want 0/0（不喂连败）",
			status.ConsecutiveFails, status.BreakerFails)
	}
}

// TestAudioTranscriptionsRequiresMultipart 非 multipart 入站明确回 400，不打上游。
func TestAudioTranscriptionsRequiresMultipart(t *testing.T) {
	t.Parallel()

	called := false
	client := audioFakeClient(func(*http.Request) *http.Response {
		called = true
		return jsonResponse(http.StatusOK, `{"code":0,"text":"x"}`)
	})

	handler := audioHandler(t, client, livecfg.Snapshot{ASREnabled: true}, nil)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions",
		strings.NewReader(`{"file":"a.wav"}`)))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d body=%s want 400", rec.Code, rec.Body)
	}

	if called {
		t.Fatal("形态错误不得打上游")
	}
}

// TestAudioPathsAreTraced 两条语音路径都必须进请求台账（tracedPath 集合）。
func TestAudioPathsAreTraced(t *testing.T) {
	t.Parallel()

	handler := NewHandler(&Config{})

	for _, path := range []string{"/v1/audio/speech", "/v1/audio/transcriptions"} {
		if !handler.tracedPath(httptest.NewRequest(http.MethodPost, path, nil)) {
			t.Errorf("%s 未纳入请求台账", path)
		}
	}
}
