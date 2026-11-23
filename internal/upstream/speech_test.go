package upstream

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
	"github.com/BegoniaHe/wokbuddy-demon/internal/jsonx"
)

// TestSpeechSSEWireFormat TTS 出站形状与 SSE 拼装：路径 /v2/audio/speech、
// 官方固定模型与默认音色、Accept 为 event-stream，delta 分片按序拼成 PCM。
//
//nolint:cyclop // 出站契约 + SSE 拼装逐字段线性断言。
func TestSpeechSSEWireFormat(t *testing.T) {
	t.Parallel()

	var gotPath, gotAccept, gotContentType string

	var gotBody []byte

	client := testClient(func(r *http.Request) (*http.Response, error) {
		gotPath = r.URL.Path
		gotAccept = r.Header.Get("Accept")
		gotContentType = r.Header.Get("Content-Type")
		gotBody, _ = io.ReadAll(r.Body)

		// 两片 base64（"AAEC" = 0x00 0x01 0x02，"AwQF" = 0x03 0x04 0x05），中间夹注释帧。
		return sseResponse(200,
			": keepalive\n\n"+
				`data: {"type":"speech.audio.delta","audio":{"data":"AAEC"}}`+"\n\n"+
				`data: {"type":"speech.audio.delta","audio":{"data":"AwQF"}}`+"\n\n"+
				`data: {"type":"speech.audio.done"}`+"\n\n"+
				"data: [DONE]\n\n"), nil
	})

	pcm, err := client.Speech(context.Background(),
		&auth.Auth{AccessToken: "at", UID: "u1"}, "你好", "", ChatMeta{})
	if err != nil {
		t.Fatalf("speech: %v", err)
	}

	if string(pcm) != "\x00\x01\x02\x03\x04\x05" {
		t.Fatalf("pcm=%q want 000102030405", pcm)
	}

	if gotPath != SpeechPath {
		t.Errorf("path=%s want %s", gotPath, SpeechPath)
	}

	if gotAccept != "text/event-stream" {
		t.Errorf("accept=%q want text/event-stream", gotAccept)
	}

	if gotContentType != "application/json" {
		t.Errorf("content-type=%q want application/json", gotContentType)
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

	if sent.Model != TTSModel || !sent.Stream ||
		sent.Speech.Input != "你好" || sent.Speech.Voice != TTSDefaultVoice ||
		sent.Speech.ResponseFormat != "pcm" || sent.Speech.SampleRate != TTSSampleRate {
		t.Fatalf("upstream body=%s", gotBody)
	}
}

// TestSpeechErrorFrameSurfaced TTS 的 SSE 里 speech.audio.* 之外的 error 帧必须
// 按错误透出，不能咽成「completed without audio」。
func TestSpeechErrorFrameSurfaced(t *testing.T) {
	t.Parallel()

	client := testClient(func(*http.Request) (*http.Response, error) {
		return sseResponse(200,
			`data: {"type":"error","error":{"code":10000,"message":"get voice config failed: voice x"}}`+
				"\n\n"+"data: [DONE]\n\n"), nil
	})

	_, err := client.Speech(context.Background(), &auth.Auth{AccessToken: "at", UID: "u1"}, "你好", "x", ChatMeta{})
	if err == nil {
		t.Fatal("error frame must not be swallowed")
	}

	upErr, ok := errors.AsType[*Error](err)
	if !ok {
		t.Fatalf("err=%T want *Error", err)
	}

	if upErr.Kind != ErrBadParams {
		t.Errorf("kind=%v want ErrBadParams（音色问题与账号无关，不许轮转全池）", upErr.Kind)
	}

	if !strings.Contains(upErr.Msg, "get voice config failed") {
		t.Errorf("msg=%q must carry the upstream original text", upErr.Msg)
	}
}

// TestTranscribeMultipartBoundary ASR 出站必须是带 boundary 的 multipart：
// 头族（CommonHeaders）会把 Content-Type 写成 application/json，Transcribe 必须在
// 它之后再设一次，否则上游解不出表单（真号实测 400 15052）。
//
//nolint:cyclop // 出站 multipart 契约逐字段线性断言。
func TestTranscribeMultipartBoundary(t *testing.T) {
	t.Parallel()

	var (
		gotPath, gotContentType string
		gotAudio, gotFilename   string
		gotPartType             string
	)

	client := testClient(func(r *http.Request) (*http.Response, error) {
		gotPath = r.URL.Path
		gotContentType = r.Header.Get("Content-Type")

		reader, err := r.MultipartReader()
		if err != nil {
			t.Fatalf("upstream request is not multipart: %v (Content-Type=%q)", err, gotContentType)
		}

		for {
			part, perr := reader.NextPart()
			if perr != nil {
				if !errors.Is(perr, io.EOF) {
					t.Fatalf("multipart read: %v", perr)
				}

				break
			}

			if part.FormName() == "audio" {
				gotFilename = part.FileName()
				gotPartType = part.Header.Get("Content-Type")
				raw, _ := io.ReadAll(part)
				gotAudio = string(raw)
			}
		}

		return jsonResp(200, `{"code":0,"text":"识别结果"}`), nil
	})

	text, err := client.Transcribe(context.Background(),
		&auth.Auth{AccessToken: "at", UID: "u1"}, "a.wav", "audio/wav", []byte("RIFFdata"), ChatMeta{})
	if err != nil {
		t.Fatalf("transcribe: %v", err)
	}

	if text != "识别结果" {
		t.Fatalf("text=%q", text)
	}

	if gotPath != ASRPath {
		t.Errorf("path=%s want %s", gotPath, ASRPath)
	}

	if !strings.HasPrefix(gotContentType, "multipart/form-data; boundary=") {
		t.Fatalf("Content-Type=%q must be multipart with boundary (被头族覆盖成 JSON 了)", gotContentType)
	}

	if gotAudio != "RIFFdata" || gotFilename != "a.wav" || gotPartType != "audio/wav" {
		t.Errorf("audio=%q filename=%q type=%q", gotAudio, gotFilename, gotPartType)
	}
}

// TestTranscribeBusinessCodeIsRequestLevel ASR 的业务码失败（15053 认不出这段音频）
// 归请求级：换任何账号都是同一句，不能罚号/轮转。
func TestTranscribeBusinessCodeIsRequestLevel(t *testing.T) {
	t.Parallel()

	client := testClient(func(*http.Request) (*http.Response, error) {
		return jsonResp(502, `{"code":15053,"msg":"ASR recognition failed","requestId":"x"}`), nil
	})

	_, err := client.Transcribe(context.Background(),
		&auth.Auth{AccessToken: "at", UID: "u1"}, "a.pcm", "application/octet-stream", []byte("raw"), ChatMeta{})
	if err == nil {
		t.Fatal("want error")
	}

	upErr, ok := errors.AsType[*Error](err)
	if !ok {
		t.Fatalf("err=%T want *Error", err)
	}

	if upErr.Kind != ErrBadParams {
		t.Errorf("kind=%v want ErrBadParams", upErr.Kind)
	}

	if upErr.Status != 502 {
		t.Errorf("status=%d want 502（端点需要透传上游状态码）", upErr.Status)
	}

	if !strings.Contains(upErr.Msg, "15053") {
		t.Errorf("msg=%q must carry the upstream original text", upErr.Msg)
	}
}

// sseResponse 构造一个 text/event-stream 响应。
func sseResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

// TestSpeechTruncatedStreamIsError 帧边界断流（收到音频但没有 done 收尾）不能当成功：
// 客户端会把截断音频当完整结果播放。
func TestSpeechTruncatedStreamIsError(t *testing.T) {
	t.Parallel()

	body := `data: {"type":"speech.audio.delta","audio":{"data":"AAEC"}}` + "\n\n"

	if _, err := readSpeechStream(strings.NewReader(body)); !errors.Is(err, ErrSpeechTruncated) {
		t.Fatalf("err=%v want ErrSpeechTruncated", err)
	}
}

// TestSpeechBusinessErrorInOKStatus 2xx + JSON 错误信封按上游报错分类，不能退化成
// 「空音频 → 传输层失败」把码与文案全丢掉。
func TestSpeechBusinessErrorInOKStatus(t *testing.T) {
	t.Parallel()

	body := `{"code":10000,"msg":"get voice config failed"}`

	_, err := readSpeechStream(strings.NewReader(body))
	if err == nil || errors.Is(err, ErrSpeechEmptyAudio) {
		t.Fatalf("err=%v want 上游错误信封", err)
	}

	uerr, ok := errors.AsType[*Error](err)
	if !ok || uerr.Code != "10000" || uerr.Msg == "" {
		t.Fatalf("err=%v want *Error(code=10000)", err)
	}
}
