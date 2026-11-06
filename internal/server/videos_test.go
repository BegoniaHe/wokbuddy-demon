package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
	"github.com/BegoniaHe/wokbuddy-demon/internal/livecfg"
	"github.com/BegoniaHe/wokbuddy-demon/internal/pool"
	"github.com/BegoniaHe/wokbuddy-demon/internal/upstream"
	"github.com/BegoniaHe/wokbuddy-demon/internal/usage"
)

// TestVideosDisabledReturns404 开关关闭时两个入口都 404，文案点明
// features.videos_endpoint。
func TestVideosDisabledReturns404(t *testing.T) {
	t.Parallel()

	handler := NewHandler(&Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: newFakeUpstream(t, func(string) (int, string, bool) { return 200, `{}`, false }),
		Live:     livecfg.New(livecfg.Snapshot{VideosEndpoint: false}),
	})

	post := httptest.NewRecorder()
	handler.ServeHTTP(post, httptest.NewRequest(http.MethodPost, "/v1/videos/generations",
		strings.NewReader(`{"model":"text-to-video","prompt":"cat"}`)))

	if post.Code != http.StatusNotFound {
		t.Fatalf("post code=%d body=%s", post.Code, post.Body)
	}

	if !strings.Contains(post.Body.String(), "features.videos_endpoint") {
		t.Fatalf("post body=%s want mention features.videos_endpoint", post.Body)
	}

	get := httptest.NewRecorder()
	handler.ServeHTTP(get, httptest.NewRequest(http.MethodGet, "/v1/videos/task-1", nil))

	if get.Code != http.StatusNotFound {
		t.Fatalf("get code=%d body=%s", get.Code, get.Body)
	}

	if !strings.Contains(get.Body.String(), "features.videos_endpoint") {
		t.Fatalf("get body=%s want mention features.videos_endpoint", get.Body)
	}
}

// TestVideoGeneratePassthroughAndUsage 提交端点：打 /v2/videos/generations、body
// 原样透传、响应（含 data.id = task id）原样透出，并按一次上游尝试记账。
func TestVideoGeneratePassthroughAndUsage(t *testing.T) {
	t.Parallel()

	var (
		gotPath, gotAccept, gotConvReqID string
		gotBody                          []byte
	)

	client := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			gotPath = r.URL.Path
			gotAccept = r.Header.Get("Accept")
			gotConvReqID = r.Header.Get("X-Conversation-Request-Id")
			gotBody, _ = io.ReadAll(r.Body)

			return jsonResponse(http.StatusOK,
				`{"code":0,"msg":"ok","requestId":"r1","data":{"id":"task-1"}}`), nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}

	usageRec := usage.New("")
	handler := NewHandler(&Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: client,
		Usage:    usageRec,
		Live:     livecfg.New(livecfg.Snapshot{VideosEndpoint: true}),
	})

	body := `{"prompt":"a cat","model":"seedance-2.5","watermark":true,"seconds":5}`
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/videos/generations",
		strings.NewReader(body)))

	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}

	if gotPath != upstream.VideoGenerationsPath {
		t.Fatalf("path=%s want %s", gotPath, upstream.VideoGenerationsPath)
	}

	if gotAccept != "application/json" {
		t.Fatalf("accept=%q", gotAccept)
	}

	// 与 chat/search/出图同口径：轮级聚合主键必须是 UUIDv7 32 hex。
	if !uuidV7SearchPattern.MatchString(gotConvReqID) {
		t.Fatalf("X-Conversation-Request-Id=%q want UUIDv7 32 hex", gotConvReqID)
	}

	if string(gotBody) != body {
		t.Fatalf("body must be passed through untouched: %s", gotBody)
	}

	if !strings.Contains(rec.Body.String(), `"id":"task-1"`) {
		t.Fatalf("response not passed through: %s", rec.Body)
	}

	if totals := usageRec.Snapshot(24, nil).Totals; totals.Requests != 1 || totals.Errors != 0 {
		t.Errorf("usage requests=%d errors=%d want 1/0", totals.Requests, totals.Errors)
	}
}

// TestVideoGenerateRealmPrefixStripped 带 realm 前缀的模型只改 body.model。
func TestVideoGenerateRealmPrefixStripped(t *testing.T) {
	t.Parallel()

	var gotBody []byte

	client := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			gotBody, _ = io.ReadAll(r.Body)

			return jsonResponse(http.StatusOK, `{"code":0,"data":{"id":"task-1"}}`), nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}

	handler := NewHandler(&Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: client,
		Live:     livecfg.New(livecfg.Snapshot{VideosEndpoint: true}),
	})

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/videos/generations",
		strings.NewReader(`{"model":"cn:seedance-2.5","prompt":"cat"}`)))

	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}

	if !strings.Contains(string(gotBody), `"model":"seedance-2.5"`) || strings.Contains(string(gotBody), "cn:") {
		t.Fatalf("body=%s", gotBody)
	}
}

// TestVideoGenerateRequiresModel 缺 model 必须在选号前 400。
func TestVideoGenerateRequiresModel(t *testing.T) {
	t.Parallel()

	handler := NewHandler(&Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: newFakeUpstream(t, func(string) (int, string, bool) { return 200, `{}`, false }),
		Live:     livecfg.New(livecfg.Snapshot{VideosEndpoint: true}),
	})

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/videos/generations",
		strings.NewReader(`{"prompt":"cat"}`)))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
}

// TestVideoTaskCompletedPassthrough 查询端点：body 是 {"task_id":...}，completed
// 时透出上游原文（含 data[].url 与 data.usage.credit），credit 照常记账。
func TestVideoTaskCompletedPassthrough(t *testing.T) {
	t.Parallel()

	var (
		gotPath string
		gotBody []byte
	)

	client := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			gotPath = r.URL.Path
			gotBody, _ = io.ReadAll(r.Body)

			return jsonResponse(http.StatusOK,
				`{"code":0,"msg":"ok","requestId":"r2","data":{"status":"completed",`+
					`"data":[{"url":"https://cdn.example/v.mp4"}],"usage":{"credit":12.5}}}`), nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}

	usageRec := usage.New("")
	handler := NewHandler(&Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: client,
		Usage:    usageRec,
		Live:     livecfg.New(livecfg.Snapshot{VideosEndpoint: true}),
	})

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/videos/task-1", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}

	if gotPath != upstream.VideoTasksPath {
		t.Fatalf("path=%s want %s", gotPath, upstream.VideoTasksPath)
	}

	if string(gotBody) != `{"task_id":"task-1"}` {
		t.Fatalf("body=%s want {\"task_id\":\"task-1\"}", gotBody)
	}

	for _, want := range []string{`"status":"completed"`, `"url":"https://cdn.example/v.mp4"`, `"credit":12.5`} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Fatalf("response missing %s: %s", want, rec.Body)
		}
	}

	if totals := usageRec.Snapshot(24, nil).Totals; totals.Requests != 1 || totals.Credits != 12.5 {
		t.Errorf("usage requests=%d credits=%v want 1/12.5", totals.Requests, totals.Credits)
	}
}

// TestVideoTaskFailedReturns502 failed 终态归一为 502 通用文案，不透出上游正文
// （可能含直链 / 调试信息 / 凭证片段）。
func TestVideoTaskFailedReturns502(t *testing.T) {
	t.Parallel()

	client := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return jsonResponse(http.StatusOK,
				`{"code":0,"msg":"ok","data":{"status":"failed","data":[{"url":"https://secret.example/v.mp4"}]}}`), nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}

	handler := NewHandler(&Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: client,
		Live:     livecfg.New(livecfg.Snapshot{VideosEndpoint: true}),
	})

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/videos/task-1", nil))

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}

	if strings.Contains(rec.Body.String(), "secret.example") {
		t.Fatalf("failed task must not leak upstream body: %s", rec.Body)
	}

	var parsed struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &parsed); err != nil {
		t.Fatal(err)
	}

	if parsed.Error.Message != "video generation task failed upstream" {
		t.Fatalf("message=%q", parsed.Error.Message)
	}
}

// TestVideoTaskModelQuerySelectsRealm 查询 body 没有模型：?model= 决定选号域，
// 缺省按 cn 选号（本域无号 → 503，不跨域回落）。
func TestVideoTaskModelQuerySelectsRealm(t *testing.T) {
	t.Parallel()

	client := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return jsonResponse(http.StatusOK, `{"code":0,"data":{"status":"running"}}`), nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}

	// 只有 global 号。
	state := pool.New("")

	global := &auth.Auth{UID: "g1", AccessToken: "at-global", ExpiresAt: 9999999999}
	if _, err := auth.BackfillRealmFor(global, "global"); err != nil {
		t.Fatal(err)
	}

	state.Add(global)

	handler := NewHandler(&Config{Pool: state, Upstream: client, Live: livecfg.New(livecfg.Snapshot{VideosEndpoint: true})})

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
		"/v1/videos/task-1?model=global:seedance-2.5", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("global code=%d body=%s", rec.Code, rec.Body)
	}

	bare := httptest.NewRecorder()
	handler.ServeHTTP(bare, httptest.NewRequest(http.MethodGet, "/v1/videos/task-1", nil))

	if bare.Code != http.StatusServiceUnavailable {
		t.Fatalf("bare code=%d body=%s want 503（默认 cn 不回落 global）", bare.Code, bare.Body)
	}
}

// TestModelsListShowsVideoCapability 视频生成模型在 /v1/models 以 capabilities
// 标记可见（issue #467），同时不进入 chat 可见集合：别名仍按隐藏处理。
func TestModelsListShowsVideoCapability(t *testing.T) {
	t.Parallel()

	cache := modelCatalog([]upstream.ModelInfo{
		{ID: "chat-model"},
		{ID: "seedance-2.5", Capability: "text-to-video", ChatVisible: new(false)},
	})

	handler := NewHandler(&Config{
		Pool:         pool.New(""),
		Upstream:     &upstream.Client{},
		ModelAliases: map[string]string{"video-alias": "seedance-2.5"},
		modelsCache:  cache,
	})

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}

	byID := modelEntriesByID(t, rec.Body.Bytes())

	video, ok := byID["cn:seedance-2.5"]
	if !ok {
		t.Fatalf("video model missing from /v1/models: %v", byID)
	}

	caps, _ := video["capabilities"].([]any)
	if len(caps) != 1 || caps[0] != "text-to-video" {
		t.Fatalf("capabilities=%v want [text-to-video]", video["capabilities"])
	}

	if _, leaked := byID["video-alias"]; leaked {
		t.Fatalf("video model alias must stay hidden from chat discovery: %v", byID)
	}

	// 单条查询同样可取到（modelRetrieve 与列表同口径）。
	retrieve := httptest.NewRecorder()
	handler.ServeHTTP(retrieve, httptest.NewRequest(http.MethodGet, "/v1/models/cn:seedance-2.5", nil))

	if retrieve.Code != http.StatusOK {
		t.Fatalf("retrieve code=%d body=%s", retrieve.Code, retrieve.Body)
	}
}

// modelEntriesByID 把 /v1/models 响应解成 id → 条目，供能力字段断言。
func modelEntriesByID(t *testing.T, body []byte) map[string]map[string]any {
	t.Helper()

	var resp struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatal(err)
	}

	byID := make(map[string]map[string]any, len(resp.Data))

	for _, entry := range resp.Data {
		if id, ok := entry["id"].(string); ok {
			byID[id] = entry
		}
	}

	return byID
}
