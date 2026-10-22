package server

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
)

// TestStreamFirstFrameErrorVeto 首帧可换号失败不写 200：直接换号后成功（issue #244）。
func TestStreamFirstFrameErrorVeto(t *testing.T) {
	t.Parallel()

	errFrame := "data: {\"error\":{\"code\":6004,\"message\":\"model busy\"}}\n\ndata: [DONE]\n\n"
	okFrame := "data: {\"id\":\"1\",\"model\":\"glm-5.2\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"}}]}\n\n" +
		"data: [DONE]\n\n"

	upstreamClient := newFakeUpstream(t, func(authz string) (int, string, bool) {
		if strings.Contains(authz, "at1") {
			return 200, errFrame, true
		}

		return 200, okFrame, true
	})
	state := testPoolWith(
		&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "u2", AccessToken: "at2", ExpiresAt: 9999999999},
	)
	handler := NewHandler(&Config{Pool: state, Upstream: upstreamClient, MaxRotate: 3})

	rec := postChat(t, handler, `{"model":"glm-5.2","stream":true,"messages":[]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}

	if !strings.Contains(rec.Body.String(), `"content":"ok"`) {
		t.Fatalf("body=%s", rec.Body)
	}

	if strings.Contains(rec.Body.String(), `"error"`) {
		t.Fatalf("client must not see first-frame error after successful rotation: %s", rec.Body)
	}
}

// TestStreamZeroOutputLengthRetriesThinkingDisabled 零输出 length 关闭思考重发一次（issue #244）。
func TestStreamZeroOutputLengthRetriesThinkingDisabled(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32

	var secondBody atomic.Value

	lengthFrame := "data: {\"id\":\"1\",\"model\":\"glm-5.2\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"length\"}]}\n\ndata: [DONE]\n\n"
	okFrame := "data: {\"id\":\"1\",\"model\":\"glm-5.2\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"

	upstreamClient := newFakeUpstream(t, func(authz string) (int, string, bool) {
		if calls.Add(1) == 1 {
			return 200, lengthFrame, true
		}

		return 200, okFrame, true
	})
	// 包一层 Transport，记录第二次请求体。
	base := upstreamClient.HTTP.Transport
	upstreamClient.HTTP.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if calls.Load() >= 1 {
			raw, _ := io.ReadAll(r.Body)
			r.Body = io.NopCloser(strings.NewReader(string(raw)))
			secondBody.Store(string(raw))
		}

		return base.RoundTrip(r)
	})

	state := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	handler := NewHandler(&Config{Pool: state, Upstream: upstreamClient})

	rec := postChat(t, handler, `{"model":"glm-5.2","stream":true,"messages":[],"thinking":{"type":"enabled"}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}

	if calls.Load() != 2 {
		t.Fatalf("calls=%d want 2", calls.Load())
	}

	body, _ := secondBody.Load().(string)
	if !strings.Contains(body, `"thinking":{"type":"disabled"}`) {
		t.Fatalf("second request must disable thinking: %s", body)
	}

	if !strings.Contains(rec.Body.String(), `"content":"ok"`) {
		t.Fatalf("body=%s", rec.Body)
	}
}

// TestNonStreamContinuation 非流式 length 截断自动续写并拼接正文（issue #247）。
func TestNonStreamContinuation(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32

	client := newFakeUpstream(t, func(string) (int, string, bool) {
		if calls.Add(1) == 1 {
			return 200, "data: {\"id\":\"c1\",\"model\":\"glm-5.2\",\"choices\":[{\"index\":0,\"message\":{\"role\":\"assistant\",\"content\":\"hello \"},\"finish_reason\":\"length\"}]}\n\ndata: [DONE]\n\n", true
		}

		return 200, "data: {\"id\":\"c1\",\"model\":\"glm-5.2\",\"choices\":[{\"index\":0,\"message\":{\"role\":\"assistant\",\"content\":\"world\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n", true
	})

	state := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	handler := NewHandler(&Config{Pool: state, Upstream: client})

	rec := postChat(t, handler, `{"model":"glm-5.2","messages":[]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}

	var parsed struct {
		Choices []struct {
			Message      map[string]any `json:"message"`
			FinishReason string         `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &parsed); err != nil {
		t.Fatal(err)
	}

	if len(parsed.Choices) != 1 || parsed.Choices[0].FinishReason != "stop" {
		t.Fatalf("choices=%+v", parsed.Choices)
	}

	if parsed.Choices[0].Message["content"] != "hello world" {
		t.Fatalf("content=%v", parsed.Choices[0].Message["content"])
	}

	if calls.Load() != 2 {
		t.Fatalf("calls=%d want 2", calls.Load())
	}
}
