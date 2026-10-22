package server

import (
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

	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		if strings.Contains(authz, "at1") {
			return 200, errFrame, true
		}

		return 200, okFrame, true
	})
	state := testPoolWith(
		&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "u2", AccessToken: "at2", ExpiresAt: 9999999999},
	)
	h := NewHandler(&Config{Pool: state, Upstream: up, MaxRotate: 3})

	rec := postChat(t, h, `{"model":"glm-5.2","stream":true,"messages":[]}`)
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

	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		if calls.Add(1) == 1 {
			return 200, lengthFrame, true
		}

		return 200, okFrame, true
	})
	// 包一层 Transport，记录第二次请求体。
	base := up.HTTP.Transport
	up.HTTP.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if calls.Load() >= 1 {
			raw, _ := io.ReadAll(r.Body)
			r.Body = io.NopCloser(strings.NewReader(string(raw)))
			secondBody.Store(string(raw))
		}

		return base.RoundTrip(r)
	})

	state := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(&Config{Pool: state, Upstream: up})

	rec := postChat(t, h, `{"model":"glm-5.2","stream":true,"messages":[],"thinking":{"type":"enabled"}}`)
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
