package server

import (
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
	"github.com/BegoniaHe/wokbuddy-demon/internal/jsonx"
	"github.com/BegoniaHe/wokbuddy-demon/internal/livecfg"
)

// TestChatNarrationDisabledStopsContinuation features.narration_enabled=false 时
// 预告句收尾不再发续写请求（issue #606）；首段正文照常下发。
func TestChatNarrationDisabledStopsContinuation(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32

	fake := newFakeUpstream(t, func(string) (int, string, bool) {
		calls.Add(1)

		return http.StatusOK, sseNarrationStop, true
	})

	handler := NewHandler(&Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: fake,
		Live:     livecfg.New(livecfg.Snapshot{NarrationDisabled: true}),
	})

	rec := postChat(t, handler, `{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}

	if got := calls.Load(); got != 1 {
		t.Fatalf("upstream calls=%d want 1（续写开关关闭，不得补发）", got)
	}

	if !strings.Contains(rec.Body.String(), "我先查一下文件：") {
		t.Errorf("首段正文不应丢失: %s", rec.Body.String())
	}
}

// TestChatNarrationEnabledKeepsContinuation 未关闭开关时行为与现状一致（默认 true，
// livecfg 按取反存，零值 = 开启）：预告句仍续写一次（issue #606）。
func TestChatNarrationEnabledKeepsContinuation(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32

	fake := newFakeUpstream(t, func(string) (int, string, bool) {
		if calls.Add(1) == 1 {
			return http.StatusOK, sseNarrationStop, true
		}

		return http.StatusOK, sseTailText, true
	})

	handler := NewHandler(&Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: fake,
		Live:     livecfg.New(livecfg.Snapshot{}),
	})

	rec := postChat(t, handler, `{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}

	if got := calls.Load(); got != 2 {
		t.Fatalf("upstream calls=%d want 2（默认开着，仍续写一次）", got)
	}
}

// TestNonStreamNarrationDisabledStopsContinuation 非流式救援走同一闸门（issue #606）。
func TestNonStreamNarrationDisabledStopsContinuation(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32

	fake := newFakeUpstream(t, func(string) (int, string, bool) {
		calls.Add(1)

		return http.StatusOK, sseNarrationStop, true
	})

	handler := NewHandler(&Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: fake,
		Live:     livecfg.New(livecfg.Snapshot{NarrationDisabled: true}),
	})

	rec := postChat(t, handler, `{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}

	if got := calls.Load(); got != 1 {
		t.Fatalf("upstream calls=%d want 1（非流式救援同样受开关约束）", got)
	}

	var got map[string]any
	if err := jsonx.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if content, _ := firstChoice(got)["message"].(map[string]any)["content"].(string); content != "我先查一下文件：" {
		t.Errorf("content=%q want 首段原文", content)
	}
}
