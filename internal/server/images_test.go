package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/BegoniaHe/wokbuddy-demon/internal/apikey"
	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
	"github.com/BegoniaHe/wokbuddy-demon/internal/livecfg"
	"github.com/BegoniaHe/wokbuddy-demon/internal/pool"
	"github.com/BegoniaHe/wokbuddy-demon/internal/upstream"
)

func jsonResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func TestImagesDisabledReturns404(t *testing.T) {
	t.Parallel()

	handler := NewHandler(&Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: newFakeUpstream(t, func(string) (int, string, bool) { return 200, `{}`, false }),
		Live:     livecfg.New(livecfg.Snapshot{ImagesEndpoint: false}),
	})

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/images/generations",
		strings.NewReader(`{"model":"text-to-image","prompt":"cat"}`)))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
}

func TestImagesPassthroughAndUsage(t *testing.T) {
	t.Parallel()

	var gotPath, gotAccept, gotConvReqID string

	var gotBody []byte

	client := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			gotPath = r.URL.Path
			gotAccept = r.Header.Get("Accept")
			gotConvReqID = r.Header.Get("X-Conversation-Request-Id")
			gotBody, _ = io.ReadAll(r.Body)

			return jsonResponse(http.StatusOK,
				`{"created":1,"data":[{"url":"https://img.example/a.png"}],"usage":{"prompt_tokens":3,"completion_tokens":4,"total_tokens":7}}`), nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}

	handler := NewHandler(&Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: client,
		Live:     livecfg.New(livecfg.Snapshot{ImagesEndpoint: true}),
	})

	body := `{"model":"text-to-image","prompt":"cat","n":1}`
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(body)))

	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}

	if gotPath != "/v2/images/generations" {
		t.Fatalf("path=%s", gotPath)
	}

	if gotAccept != "application/json" {
		t.Fatalf("accept=%q", gotAccept)
	}

	// 与 chat/search 同口径：轮级聚合主键必须是 UUIDv7 32 hex，不能是裸随机 hex。
	if !uuidV7SearchPattern.MatchString(gotConvReqID) {
		t.Fatalf("X-Conversation-Request-Id=%q want UUIDv7 32 hex", gotConvReqID)
	}

	if string(gotBody) != body {
		t.Fatalf("body must be passed through untouched: %s", gotBody)
	}

	var parsed map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &parsed); err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(rec.Body.String(), `"url":"https://img.example/a.png"`) {
		t.Fatalf("response not passed through: %s", rec.Body)
	}
}

func TestImagesRealmDirectNoFallback(t *testing.T) {
	t.Parallel()

	client := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return jsonResponse(http.StatusOK, `{}`), nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}

	// 只有 global 号：裸模型默认 cn，必须 fail-fast 而不是回落到 global。
	state := pool.New("")

	global := &auth.Auth{UID: "g1", AccessToken: "at-global", ExpiresAt: 9999999999}
	if _, err := auth.BackfillRealmFor(global, "global"); err != nil {
		t.Fatal(err)
	}

	state.Add(global)

	handler := NewHandler(&Config{Pool: state, Upstream: client, Live: livecfg.New(livecfg.Snapshot{ImagesEndpoint: true})})

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/images/generations",
		strings.NewReader(`{"model":"text-to-image","prompt":"cat"}`)))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
}

// TestImagesRealmPrefixStripped 带 realm 前缀的模型只改 body.model，按前缀直路由。
func TestImagesRealmPrefixStripped(t *testing.T) {
	t.Parallel()

	var gotBody []byte

	client := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			gotBody, _ = io.ReadAll(r.Body)

			return jsonResponse(http.StatusOK, `{"created":1,"data":[]}`), nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}

	handler := NewHandler(&Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: client,
		Live:     livecfg.New(livecfg.Snapshot{ImagesEndpoint: true}),
	})

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/images/generations",
		strings.NewReader(`{"model":"cn:text-to-image","prompt":"cat"}`)))

	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}

	if !strings.Contains(string(gotBody), `"model":"text-to-image"`) || strings.Contains(string(gotBody), "cn:") {
		t.Fatalf("body=%s", gotBody)
	}
}

// TestImagesHiddenModelRejected models.hidden 在出图端点同样生效（不能绕过模型限制）。
func TestImagesHiddenModelRejected(t *testing.T) {
	t.Parallel()

	called := false
	client := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			called = true
			return jsonResponse(http.StatusOK, `{}`), nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}

	handler := NewHandler(&Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: client,
		Live: livecfg.New(livecfg.Snapshot{
			ImagesEndpoint: true,
			HiddenModels:   map[string]struct{}{"cn:text-to-image": {}},
		}),
	})

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/images/generations",
		strings.NewReader(`{"model":"text-to-image","prompt":"cat"}`)))

	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "model_unavailable") {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}

	if called {
		t.Fatal("hidden model must not reach upstream")
	}
}

// TestImagesAliasRewrittenToUpstreamModel 别名解析后按最终裸模型出站。
func TestImagesAliasRewrittenToUpstreamModel(t *testing.T) {
	t.Parallel()

	var gotBody []byte

	client := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			gotBody, _ = io.ReadAll(r.Body)

			return jsonResponse(http.StatusOK, `{"created":1,"data":[]}`), nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}

	handler := NewHandler(&Config{
		Pool:         testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream:     client,
		Live:         livecfg.New(livecfg.Snapshot{ImagesEndpoint: true}),
		ModelAliases: map[string]string{"img-alias": "cn:text-to-image"},
	})

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/images/generations",
		strings.NewReader(`{"model":"img-alias","prompt":"cat"}`)))

	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}

	if !strings.Contains(string(gotBody), `"model":"text-to-image"`) {
		t.Fatalf("body=%s", gotBody)
	}
}

// TestImagesUpstreamErrorTruncated 上游错误正文只透出截断后的 Msg，避免把大段
// HTML/调试信息原样回给客户端。
func TestImagesUpstreamErrorTruncated(t *testing.T) {
	t.Parallel()

	bigBody := strings.Repeat("x", 4096)
	client := newFakeUpstream(t, func(string) (int, string, bool) {
		return http.StatusInternalServerError, bigBody, false
	})

	handler := NewHandler(&Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: client,
		Live:     livecfg.New(livecfg.Snapshot{ImagesEndpoint: true}),
	})

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/images/generations",
		strings.NewReader(`{"model":"text-to-image","prompt":"cat"}`)))

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}

	var parsed struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &parsed); err != nil {
		t.Fatal(err)
	}

	if len(parsed.Error.Message) > 200 {
		t.Fatalf("error message len=%d want <=200", len(parsed.Error.Message))
	}
}

// TestImagesRefreshFailureDoesNotMarkSessionDead 出图 401 后 refresh 遇到瞬时失败
// （网络/5xx）时不能把原始 401 记成会话失效，否则健康号会被连续三次刷新失败误禁用。
func TestImagesRefreshFailureDoesNotMarkSessionDead(t *testing.T) {
	t.Parallel()

	var imageCalls, refreshCalls int

	client := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if strings.Contains(r.URL.Path, "/images/") {
				imageCalls++

				return jsonResponse(http.StatusUnauthorized, `{"code":12153,"msg":"offline user session"}`), nil
			}

			refreshCalls++

			return jsonResponse(http.StatusInternalServerError, `{"code":10000,"msg":"temporary"}`), nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}

	state := testPoolWith(&auth.Auth{
		UID: "u1", AccessToken: "at1", RefreshToken: "rt1", ExpiresAt: 9999999999,
	})
	handler := NewHandler(&Config{
		Pool:     state,
		Upstream: client,
		Live:     livecfg.New(livecfg.Snapshot{ImagesEndpoint: true}),
	})

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/images/generations",
		strings.NewReader(`{"model":"text-to-image","prompt":"cat"}`)))

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}

	if imageCalls != 1 || refreshCalls == 0 {
		t.Fatalf("imageCalls=%d refreshCalls=%d", imageCalls, refreshCalls)
	}

	status, ok := state.Status("u1")
	if !ok {
		t.Fatal("account missing")
	}

	if status.SessionDeadFails != 0 || status.Disabled {
		t.Fatalf("transient refresh failure must not mark session dead: %+v", status)
	}
}

// TestImagesBusinessErrorNot200 上游 HTTP 200 + 非零业务码必须映射成错误状态，
// 不能把 error 信封挂在 200 上让 OpenAI 客户端当成功解析。
func TestImagesBusinessErrorNot200(t *testing.T) {
	t.Parallel()

	client := newFakeUpstream(t, func(string) (int, string, bool) {
		return http.StatusOK, `{"code":9999,"msg":"image backend failed"}`, false
	})

	handler := NewHandler(&Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: client,
		Live:     livecfg.New(livecfg.Snapshot{ImagesEndpoint: true}),
	})

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/images/generations",
		strings.NewReader(`{"model":"text-to-image","prompt":"cat"}`)))

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}

	if !strings.Contains(rec.Body.String(), `"code":"upstream_error"`) {
		t.Fatalf("body=%s", rec.Body)
	}
}

// TestImagesRefreshDeadCredentialCountsSession 出图前的刷新若确认凭证失效，
// 必须计入 session dead 连续计数（与 chat/search 同口径），不能静默 502。
func TestImagesRefreshDeadCredentialCountsSession(t *testing.T) {
	t.Parallel()

	imageCalls := 0

	client := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if strings.Contains(r.URL.Path, "/images/") {
				imageCalls++
			}

			return jsonResponse(http.StatusUnauthorized, `{"code":12153,"msg":"offline user session"}`), nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}

	state := testPoolWith(&auth.Auth{
		UID: "u1", AccessToken: "at1", RefreshToken: "rt1", ExpiresAt: 1,
	})
	handler := NewHandler(&Config{
		Pool:     state,
		Upstream: client,
		Live:     livecfg.New(livecfg.Snapshot{ImagesEndpoint: true}),
	})

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/images/generations",
		strings.NewReader(`{"model":"text-to-image","prompt":"cat"}`)))

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}

	if imageCalls != 0 {
		t.Fatalf("凭证失效时不应继续出图: imageCalls=%d", imageCalls)
	}

	status, ok := state.Status("u1")
	if !ok {
		t.Fatal("account missing")
	}

	if status.SessionDeadFails != 1 || status.Disabled {
		t.Fatalf("session dead=%+v want 1 次且未达阈值不禁用", status)
	}
}

// TestNoteRefreshFailureNoRefreshTokenCountsSession 无 RT 的 credential-dead
// 错误在非 chat 入口也必须计入 session dead 阈值，而不是只喂熔断。
func TestNoteRefreshFailureNoRefreshTokenCountsSession(t *testing.T) {
	t.Parallel()

	account := &auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 1}
	state := testPoolWith(account)
	client := upstream.New()
	client.BillingBaseCN = "https://fake.example"

	handler := NewHandler(&Config{Pool: state, Upstream: client})

	err := client.RefreshToken(account)
	if err == nil {
		t.Fatal("无 refreshToken 应返回错误")
	}

	handler.noteRefreshFailure(account, err, "search_refresh")

	status, ok := state.Status("u1")
	if !ok {
		t.Fatal("account missing")
	}

	if status.SessionDeadFails != 1 || status.Disabled {
		t.Fatalf("session dead=%+v want 1 次且未达阈值不禁用", status)
	}
}

// TestImagesNamedKeyUsageRecorded 出图路径也要把 token 用量回填到命名密钥：
// 只记请求数而漏 token 会让密钥用量面板少记（与 chat / anthropic 同口径）。
func TestImagesNamedKeyUsageRecorded(t *testing.T) {
	t.Parallel()

	store := apikey.New("")

	created, raw, err := store.Create("img")
	if err != nil {
		t.Fatal(err)
	}

	client := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return jsonResponse(http.StatusOK,
				`{"created":1,"data":[],"usage":{"prompt_tokens":3,"completion_tokens":4,"total_tokens":7}}`), nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}

	handler := NewHandler(&Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: client,
		Keys:     store,
		Live:     livecfg.New(livecfg.Snapshot{APIKey: "secret", ImagesEndpoint: true}),
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations",
		strings.NewReader(`{"model":"text-to-image","prompt":"cat"}`))
	req.Header.Set("Authorization", "Bearer "+raw)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}

	var got *apikey.Record

	for _, item := range store.List() {
		if item.ID == created.ID {
			got = &item

			break
		}
	}

	if got == nil {
		t.Fatal("named key missing")
	}

	if got.Requests != 1 || got.PromptTokens != 3 || got.CompletionTokens != 4 || got.TotalTokens != 7 {
		t.Fatalf("key usage=%+v want requests=1 prompt=3 completion=4 total=7", got)
	}
}

// TestImagesAllowedModelRejected models.allowed 在出图端点同样生效（不能绕过白名单）。
func TestImagesAllowedModelRejected(t *testing.T) {
	t.Parallel()

	called := false
	client := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			called = true
			return jsonResponse(http.StatusOK, `{}`), nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}

	handler := NewHandler(&Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: client,
		Live: livecfg.New(livecfg.Snapshot{
			ImagesEndpoint: true,
			AllowedModels:  livecfg.NormalizeAllowedModels([]string{"cn:glm-5.2"}),
		}),
	})

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/images/generations",
		strings.NewReader(`{"model":"text-to-image","prompt":"cat"}`)))

	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "model_unavailable") {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}

	if called {
		t.Fatal("白名单外模型不得打上游")
	}
}

// TestImagesEditsForwardedToEditsPath 开启开关后 /v1/images/edits 被转发到
// /v2/images/edits（JSON 形态原样透传，不拆 image 字段）。
func TestImagesEditsForwardedToEditsPath(t *testing.T) {
	t.Parallel()

	var gotPath, gotAccept string

	var gotBody []byte

	client := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			gotPath = r.URL.Path
			gotAccept = r.Header.Get("Accept")
			gotBody, _ = io.ReadAll(r.Body)

			return jsonResponse(http.StatusOK,
				`{"created":1,"data":[{"url":"https://img.example/edited.png"}],"usage":{"total_tokens":9}}`), nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}

	handler := NewHandler(&Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: client,
		Live:     livecfg.New(livecfg.Snapshot{ImagesEndpoint: true}),
	})

	body := `{"model":"image-to-image","prompt":"make it blue","image":["data:image/png;base64,AAA"],"input_fidelity":"high"}`
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/images/edits", strings.NewReader(body)))

	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}

	if gotPath != "/v2/images/edits" {
		t.Fatalf("path=%s want /v2/images/edits", gotPath)
	}

	if gotAccept != "application/json" {
		t.Fatalf("accept=%q", gotAccept)
	}

	if string(gotBody) != body {
		t.Fatalf("body must be passed through untouched: %s", gotBody)
	}

	if !strings.Contains(rec.Body.String(), "edited.png") {
		t.Fatalf("response not passed through: %s", rec.Body)
	}
}

// TestImagesEditsDisabledMentionsEditsPath 关闭时的文案按端点区分：
// edits 请求必须提到 /v1/images/edits，generations 文案保持原样。
func TestImagesEditsDisabledMentionsEditsPath(t *testing.T) {
	t.Parallel()

	handler := NewHandler(&Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: newFakeUpstream(t, func(string) (int, string, bool) { return 200, `{}`, false }),
		Live:     livecfg.New(livecfg.Snapshot{ImagesEndpoint: false}),
	})

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/images/edits",
		strings.NewReader(`{"model":"image-to-image","prompt":"cat"}`)))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}

	if !strings.Contains(rec.Body.String(), "POST /v1/images/edits") {
		t.Fatalf("body=%s want mention POST /v1/images/edits", rec.Body)
	}

	if strings.Contains(rec.Body.String(), "/v1/images/generations") {
		t.Fatalf("body=%s must not mention generations", rec.Body)
	}

	// generations 的文案保持原样。
	recGen := httptest.NewRecorder()
	handler.ServeHTTP(recGen, httptest.NewRequest(http.MethodPost, "/v1/images/generations",
		strings.NewReader(`{"model":"text-to-image","prompt":"cat"}`)))

	if !strings.Contains(recGen.Body.String(), "enable features.images_endpoint to use POST /v1/images/generations") {
		t.Fatalf("generations body=%s want unchanged copy", recGen.Body)
	}
}

// TestImagesEditsRealmPrefixStripped edits 与 generations 共用 rewriteImageModel：
// 带域前缀的 model 只改 body.model，image / input_fidelity 不动。
func TestImagesEditsRealmPrefixStripped(t *testing.T) {
	t.Parallel()

	var gotBody []byte

	client := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			gotBody, _ = io.ReadAll(r.Body)

			return jsonResponse(http.StatusOK, `{"created":1,"data":[]}`), nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}

	handler := NewHandler(&Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: client,
		Live:     livecfg.New(livecfg.Snapshot{ImagesEndpoint: true}),
	})

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/images/edits",
		strings.NewReader(`{"model":"cn:image-to-image","prompt":"cat","image":"data:image/png;base64,AAA"}`)))

	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}

	if !strings.Contains(string(gotBody), `"model":"image-to-image"`) || strings.Contains(string(gotBody), "cn:") {
		t.Fatalf("body=%s", gotBody)
	}

	if !strings.Contains(string(gotBody), `"image":"data:image/png;base64,AAA"`) {
		t.Fatalf("image field lost: %s", gotBody)
	}
}

// TestImagesEditsRejectsMultipartBody edits 只收 JSON（上游形态）：multipart 形态必须
// 明确报「形态不支持并给出 JSON 写法」，不能因为解析失败落成误导性的 "model is required"，
// 且不得打上游。
func TestImagesEditsRejectsMultipartBody(t *testing.T) {
	t.Parallel()

	upstreamCalled := false

	client := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			upstreamCalled = true

			return jsonResponse(http.StatusOK, `{"created":1,"data":[]}`), nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}

	handler := NewHandler(&Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: client,
		Live:     livecfg.New(livecfg.Snapshot{ImagesEndpoint: true}),
	})

	const boundary = "----wbBoundary"

	body := "--" + boundary + "\r\n" +
		`Content-Disposition: form-data; name="model"` + "\r\n\r\n" +
		"image-to-image\r\n" +
		"--" + boundary + "\r\n" +
		`Content-Disposition: form-data; name="image"; filename="a.png"` + "\r\n" +
		"Content-Type: image/png\r\n\r\n" + "PNGDATA\r\n" +
		"--" + boundary + "--\r\n"

	req := httptest.NewRequest(http.MethodPost, "/v1/images/edits", strings.NewReader(body))
	req.Header.Set("Content-Type", "multipart/form-data; boundary="+boundary)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}

	if upstreamCalled {
		t.Fatal("multipart 请求不得打上游")
	}

	got := rec.Body.String()

	for _, want := range []string{"multipart/form-data is not supported", "data URL"} {
		if !strings.Contains(got, want) {
			t.Errorf("body=%s want mention %q", got, want)
		}
	}

	if strings.Contains(got, "model is required") {
		t.Errorf("body=%s 不能把形态问题误报为缺 model", got)
	}
}

// TestImagesEditsMultipartRejectedBeforeBodyLimit multipart 判定先于读 body：
// 超过 server.max_body_bytes 的上传也必须回 400 形态提示，而不是 413 体积超限
// （后者会引导客户端去调大上限而不是换形态）。
func TestImagesEditsMultipartRejectedBeforeBodyLimit(t *testing.T) {
	t.Parallel()

	upstreamCalled := false

	client := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			upstreamCalled = true

			return jsonResponse(http.StatusOK, `{"created":1,"data":[]}`), nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}

	handler := NewHandler(&Config{
		Pool:         testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream:     client,
		Live:         livecfg.New(livecfg.Snapshot{ImagesEndpoint: true}),
		MaxBodyBytes: 8,
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/images/edits",
		strings.NewReader("--b\r\n"+strings.Repeat("x", 4096)+"\r\n--b--\r\n"))
	req.Header.Set("Content-Type", "multipart/form-data; boundary=b")

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d body=%s want 400（形态优先于体积）", rec.Code, rec.Body)
	}

	if upstreamCalled {
		t.Fatal("multipart 请求不得打上游")
	}

	if !strings.Contains(rec.Body.String(), "multipart/form-data is not supported") {
		t.Errorf("body=%s want multipart 提示", rec.Body)
	}
}
