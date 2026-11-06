package upstream

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
)

// TestGenerateVideoPostsToGenerationsPath 提交走 /v2/videos/generations，body 原样
// 透传，头族与图像面同源（Content-Type / Accept 都是 JSON）。
func TestGenerateVideoPostsToGenerationsPath(t *testing.T) {
	t.Parallel()

	var (
		gotPath, gotContentType, gotAccept, gotCodebuddyRequest, gotConvReqID string
		gotBody                                                               []byte
	)

	client := testClient(func(r *http.Request) (*http.Response, error) {
		gotPath = r.URL.Path
		gotContentType = r.Header.Get("Content-Type")
		gotAccept = r.Header.Get("Accept")
		gotCodebuddyRequest = r.Header.Get("X-Codebuddy-Request")
		gotConvReqID = r.Header.Get("X-Conversation-Request-Id")
		gotBody, _ = io.ReadAll(r.Body)

		return jsonResp(200, `{"code":0,"msg":"ok","requestId":"r1","data":{"id":"task-1"}}`), nil
	})

	body := []byte(`{"prompt":"a cat","model":"seedance-2.5","watermark":true,"seconds":5,` +
		`"reference_images":[{"image_url":"https://img.example/a.png"}]}`)

	status, raw, err := client.GenerateVideo(
		context.Background(), &auth.Auth{AccessToken: "at", UID: "u1"}, body, "", ChatMeta{
			ConversationRequestID: "01a17a9c4d2f7b3e8c5a6d4e2f1b0a93",
		},
	)
	if err != nil {
		t.Fatalf("generate video: %v", err)
	}

	if status != 200 || !strings.Contains(string(raw), `"id":"task-1"`) {
		t.Fatalf("status=%d raw=%s", status, raw)
	}

	if gotPath != VideoGenerationsPath {
		t.Errorf("path=%s want %s", gotPath, VideoGenerationsPath)
	}

	if gotContentType != "application/json" || gotAccept != "application/json" {
		t.Errorf("content-type=%q accept=%q want application/json", gotContentType, gotAccept)
	}

	// 视频面复用 chat 头族（官方 VideoService 与图像面同源）。
	if gotCodebuddyRequest != "1" {
		t.Errorf("X-Codebuddy-Request=%q want 1", gotCodebuddyRequest)
	}

	if gotConvReqID != "01a17a9c4d2f7b3e8c5a6d4e2f1b0a93" {
		t.Errorf("X-Conversation-Request-ID=%q want 透传值", gotConvReqID)
	}

	if string(gotBody) != string(body) {
		t.Errorf("body=%s want passthrough %s", gotBody, body)
	}
}

// TestGetVideoTaskPostsTaskIDEnvelope 查询走 /v2/videos/tasks，body 是 JSON 编码的
// {"task_id":...}（不是字符串拼接）。
func TestGetVideoTaskPostsTaskIDEnvelope(t *testing.T) {
	t.Parallel()

	var gotPath string

	var gotBody []byte

	client := testClient(func(r *http.Request) (*http.Response, error) {
		gotPath = r.URL.Path
		gotBody, _ = io.ReadAll(r.Body)

		return jsonResp(200, `{"code":0,"msg":"ok","requestId":"r2","data":{"status":"completed",`+
			`"data":[{"url":"https://cdn.example/v.mp4"}],"usage":{"credit":12.5}}}`), nil
	})

	status, raw, err := client.GetVideoTask(context.Background(),
		&auth.Auth{AccessToken: "at", UID: "u1"}, "task-1", "", ChatMeta{})
	if err != nil {
		t.Fatalf("get video task: %v", err)
	}

	if status != 200 || !strings.Contains(string(raw), `"status":"completed"`) {
		t.Fatalf("status=%d raw=%s", status, raw)
	}

	if gotPath != VideoTasksPath {
		t.Errorf("path=%s want %s", gotPath, VideoTasksPath)
	}

	if string(gotBody) != `{"task_id":"task-1"}` {
		t.Errorf("body=%s want {\"task_id\":\"task-1\"}", gotBody)
	}
}

// TestGetVideoTaskRequiresTaskID 空 task_id 在本地拒绝，不发上游。
func TestGetVideoTaskRequiresTaskID(t *testing.T) {
	t.Parallel()

	called := false

	client := testClient(func(*http.Request) (*http.Response, error) {
		called = true

		return jsonResp(200, `{"code":0}`), nil
	})

	if _, _, err := client.GetVideoTask(context.Background(),
		&auth.Auth{AccessToken: "at", UID: "u1"}, "   ", "", ChatMeta{}); err == nil {
		t.Fatal("want error for empty task_id")
	}

	if called {
		t.Fatal("空 task_id 不应发上游")
	}
}

// TestVideoUpstreamErrorCarriesStatus 上游业务错误归一为 *Error，body 保留供透传。
func TestVideoUpstreamErrorCarriesStatus(t *testing.T) {
	t.Parallel()

	client := testClient(func(*http.Request) (*http.Response, error) {
		return jsonResp(200, `{"code":11102,"msg":"model not allowed"}`), nil
	})

	status, raw, err := client.GenerateVideo(context.Background(),
		&auth.Auth{AccessToken: "at", UID: "u1"}, []byte(`{"model":"m"}`), "", ChatMeta{})
	if err == nil {
		t.Fatal("want error for non-zero code")
	}

	var upstreamErr *Error
	if !errors.As(err, &upstreamErr) {
		t.Fatalf("err=%T want *Error", err)
	}

	if status != 200 || upstreamErr.Status != 200 || len(raw) == 0 {
		t.Errorf("status=%d upstreamErr.Status=%d raw=%s", status, upstreamErr.Status, raw)
	}
}

// TestVideoModelCatalogCapability 视频生成模型保留在目录里并标记能力，同时钉住
// ChatVisible=false（不进入 chat 可见集合）；图片生成 / tiny 等仍整条丢弃。
func TestVideoModelCatalogCapability(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		tags     []string
		wantCap  string
		wantDrop bool
	}{
		{"文生视频", []string{"text-to-video"}, "text-to-video", false},
		{"图生视频", []string{"image-to-video"}, "image-to-video", false},
		{"两者都有取文生视频", []string{"image-to-video", "text-to-video"}, "text-to-video", false},
		{"出图仍整条丢弃", []string{"text-to-image"}, "", true},
		{"对话模型", []string{"craft"}, "", false},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			entry := dynModelEntry{ID: "m", Tags: append([]string{"balanced"}, testCase.tags...)}
			info := entry.modelInfo()

			if info.Capability != testCase.wantCap {
				t.Errorf("Capability=%q want %q", info.Capability, testCase.wantCap)
			}

			if testCase.wantCap != "" {
				if info.ChatVisible == nil || *info.ChatVisible {
					t.Errorf("视频模型 ChatVisible=%v want false", info.ChatVisible)
				}
			} else if info.ChatVisible != nil {
				t.Errorf("非视频模型不应产生 ChatVisible 判定：%v", *info.ChatVisible)
			}

			if got := dropFromCatalog(entry.ID, entry.MaxOutputTokens, entry.Tags); got != testCase.wantDrop {
				t.Errorf("dropFromCatalog=%v want %v", got, testCase.wantDrop)
			}
		})
	}
}

// TestModelInfoCapabilityNotSerialized Capability 是目录内部字段，不得随
// ModelInfo 的 JSON 序列化泄漏（与 ChatVisible 同口径）。
func TestModelInfoCapabilityNotSerialized(t *testing.T) {
	t.Parallel()

	raw, err := json.Marshal(ModelInfo{ID: "m", Capability: "text-to-video"}) //nolint:musttag // 内部结构无 JSON 契约，此处只验证字段被排除
	if err != nil {
		t.Fatal(err)
	}

	if strings.Contains(string(raw), "Capability") || strings.Contains(string(raw), "text-to-video") {
		t.Fatalf("Capability 不应进入 JSON: %s", raw)
	}
}
