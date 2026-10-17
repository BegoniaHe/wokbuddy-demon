package server

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
	"github.com/BegoniaHe/wokbuddy-demon/internal/livecfg"
	"github.com/BegoniaHe/wokbuddy-demon/internal/pool"
	"github.com/BegoniaHe/wokbuddy-demon/internal/upstream"
)

// setModelCatalog 注入 CN 模型目录缓存（测试隔离：cleanup 清空）。
func setModelCatalog(t *testing.T, infos []upstream.ModelInfo) {
	t.Helper()
	dynamicModelsCache.Lock()
	dynamicModelsCache.ids = infos
	dynamicModelsCache.fetched = time.Now()
	dynamicModelsCache.lastFail = time.Time{}
	dynamicModelsCache.Unlock()
	t.Cleanup(func() {
		dynamicModelsCache.Lock()
		dynamicModelsCache.ids = nil
		dynamicModelsCache.fetched = time.Time{}
		dynamicModelsCache.lastFail = time.Time{}
		dynamicModelsCache.Unlock()
	})
}

// captureUpstream 记录每次出站请求体的模型名，返回固定成功 SSE。
func captureUpstream(t *testing.T) (*upstream.Client, *[]string, *int64) {
	t.Helper()
	models := &[]string{}
	var calls int64
	client := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			atomic.AddInt64(&calls, 1)
			raw, _ := io.ReadAll(r.Body)
			var body struct {
				Model string `json:"model"`
			}
			_ = json.Unmarshal(raw, &body)
			*models = append(*models, body.Model)
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader(sseOK)),
			}, nil
		})},
		ChatBaseCN:     "https://fake.example",
		BillingBaseCN:  "https://fake.example",
		ChatBaseGlobal: "https://fake.example",
	}
	return client, models, &calls
}

// modalityCatalog 文本模型 + 视觉模型 + 音频模型。
func modalityCatalog() []upstream.ModelInfo {
	return []upstream.ModelInfo{
		{ID: "text-only", InputModalities: []string{"text"}},
		{ID: "vision", InputModalities: []string{"text", "image"}},
		{ID: "audio", InputModalities: []string{"text", "audio"}},
		{ID: "files", InputModalities: []string{"text", "file"}},
	}
}

// newModalityHandler 构造单账号池 + 目录缓存 + 可选热配置。
func newModalityHandler(t *testing.T, routing bool) (*Handler, *[]string, *int64) {
	t.Helper()
	setModelCatalog(t, modalityCatalog())
	up, models, calls := captureUpstream(t)
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
	cfg := Config{Pool: p, Upstream: up, SoftCooldown: time.Minute, MultimodalRouting: routing}
	if routing {
		cfg.Live = livecfg.New(livecfg.Snapshot{MultimodalRouting: true})
	}
	return NewHandler(cfg), models, calls
}

// imageBody 构造带 image_url part 的请求体。
func imageBody(model string) string {
	return `{"model":"` + model + `","messages":[{"role":"user","content":[` +
		`{"type":"text","text":"what is this"},` +
		`{"type":"image_url","image_url":{"url":"https://example.com/a.png"}}]}]}`
}

// partBody 构造带指定 part type 的请求体。
func partBody(model, partType string) string {
	return `{"model":"` + model + `","messages":[{"role":"user","content":[{"type":"` + partType + `"}]}]}`
}

// TestRequestModalitiesMixedContent 字符串 content 与 part 数组混用：字符串消息
// 视为纯文本，数组消息的 part 仍被识别（OpenAI 规范的常见形态）。
func TestRequestModalitiesMixedContent(t *testing.T) {
	body := `{"messages":[` +
		`{"role":"system","content":"be nice"},` +
		`{"role":"assistant","content":"ok"},` +
		`{"role":"user","content":[{"type":"text","text":"what"},` +
		`{"type":"image_url","image_url":{"url":"https://example.com/a.png"}}]}]}`
	got := requestModalities([]byte(body))
	if strings.Join(got, ",") != "image" {
		t.Fatalf("modalities=%v want [image]", got)
	}
}

// TestChatMixedContentModalityRejected 混用 content 的带图请求仍按模型能力拒绝，
// 不因解析失败而静默送上游（issue #109）。
func TestChatMixedContentModalityRejected(t *testing.T) {
	h, _, calls := newModalityHandler(t, false)
	body := `{"model":"text-only","messages":[` +
		`{"role":"system","content":"be nice"},` +
		`{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://example.com/a.png"}}]}]}`
	rec := postChat(t, h, body)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d want 400 body=%s", rec.Code, rec.Body)
	}
	if n := atomic.LoadInt64(calls); n != 0 {
		t.Errorf("upstream calls=%d want 0", n)
	}
}

// TestChatRejectsUnsupportedModality 模型明确不支持图片：默认 400
// model_capability，不发起任何上游调用，hint 列出可用模型（issue #109）。
func TestChatRejectsUnsupportedModality(t *testing.T) {
	h, _, calls := newModalityHandler(t, false)
	rec := postChat(t, h, imageBody("text-only"))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d want 400 body=%s", rec.Code, rec.Body)
	}
	if n := atomic.LoadInt64(calls); n != 0 {
		t.Errorf("upstream calls=%d want 0（能力判定必须在选号前）", n)
	}
	var resp struct {
		Error struct {
			Code        string `json:"code"`
			Message     string `json:"message"`
			GatewayHint string `json:"gateway_hint"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Error.Code != "model_capability" {
		t.Errorf("code=%q want model_capability", resp.Error.Code)
	}
	if !strings.Contains(resp.Error.Message, "image") {
		t.Errorf("message=%q want image", resp.Error.Message)
	}
	if !strings.Contains(resp.Error.GatewayHint, "vision") {
		t.Errorf("hint=%q want vision candidate", resp.Error.GatewayHint)
	}
}

// TestChatRejectsAudioAndFileParts 音频/文件 part 同样被识别并按能力拒绝。
func TestChatRejectsAudioAndFileParts(t *testing.T) {
	cases := []struct {
		part string
		want string
	}{
		{"input_audio", "audio"},
		{"file", "file"},
	}
	for _, c := range cases {
		t.Run(c.part, func(t *testing.T) {
			h, _, calls := newModalityHandler(t, false)
			rec := postChat(t, h, partBody("text-only", c.part))
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("code=%d want 400 body=%s", rec.Code, rec.Body)
			}
			if n := atomic.LoadInt64(calls); n != 0 {
				t.Errorf("upstream calls=%d want 0", n)
			}
			if !strings.Contains(rec.Body.String(), c.want) {
				t.Errorf("body=%s want modality %q", rec.Body, c.want)
			}
		})
	}
}

// TestChatMultimodalRoutingRewritesModel 开启路由后改选支持该模态的模型，
// 出站 body 的 model 被重写（issue #109）。
func TestChatMultimodalRoutingRewritesModel(t *testing.T) {
	h, models, calls := newModalityHandler(t, true)
	rec := postChat(t, h, imageBody("text-only"))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if n := atomic.LoadInt64(calls); n != 1 {
		t.Fatalf("upstream calls=%d want 1", n)
	}
	if got := (*models)[0]; got != "vision" {
		t.Errorf("outbound model=%q want vision（按模态改选）", got)
	}
}

// TestChatMultimodalRoutingSkipsUnknownCapability 能力未知的模型不得作为改选
// 候选（未知 ≠ 支持），只能选到明确声明支持该模态的模型。
func TestChatMultimodalRoutingSkipsUnknownCapability(t *testing.T) {
	setModelCatalog(t, []upstream.ModelInfo{
		{ID: "unknown-first"}, // 目录第一位，但能力未知
		{ID: "text-only", InputModalities: []string{"text"}},
		{ID: "vision", InputModalities: []string{"text", "image"}},
	})
	up, models, calls := captureUpstream(t)
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
	h := NewHandler(Config{
		Pool: p, Upstream: up, SoftCooldown: time.Minute,
		Live: livecfg.New(livecfg.Snapshot{MultimodalRouting: true}),
	})
	rec := postChat(t, h, imageBody("text-only"))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if n := atomic.LoadInt64(calls); n != 1 {
		t.Fatalf("upstream calls=%d want 1", n)
	}
	if got := (*models)[0]; got != "vision" {
		t.Errorf("outbound model=%q want vision（未知能力模型不得入选）", got)
	}
}

// TestRejectModalityHintSkipsUnknownCapability 400 提示只列明确声明支持的模型，
// 不把能力未知的模型写成候选。
func TestRejectModalityHintSkipsUnknownCapability(t *testing.T) {
	setModelCatalog(t, []upstream.ModelInfo{
		{ID: "unknown-first"},
		{ID: "text-only", InputModalities: []string{"text"}},
	})
	up, _, _ := captureUpstream(t)
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, SoftCooldown: time.Minute})
	rec := postChat(t, h, imageBody("text-only"))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d want 400 body=%s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), "unknown-first") {
		t.Errorf("body=%s 不应把能力未知模型列为候选", rec.Body)
	}
}

// TestChatMultimodalRoutingNoCandidateFallsBackTo400 无候选模型时仍明确 400。
func TestChatMultimodalRoutingNoCandidateFallsBackTo400(t *testing.T) {
	setModelCatalog(t, []upstream.ModelInfo{{ID: "text-only", InputModalities: []string{"text"}}})
	up, _, calls := captureUpstream(t)
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
	h := NewHandler(Config{
		Pool: p, Upstream: up, SoftCooldown: time.Minute,
		Live: livecfg.New(livecfg.Snapshot{MultimodalRouting: true}),
	})
	rec := postChat(t, h, imageBody("text-only"))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d want 400 body=%s", rec.Code, rec.Body)
	}
	if n := atomic.LoadInt64(calls); n != 0 {
		t.Errorf("upstream calls=%d want 0", n)
	}
}

// TestChatModalityUnknownPasses 能力未知（目录无该模型）不拦截：请求照常送上游。
func TestChatModalityUnknownPasses(t *testing.T) {
	h, _, calls := newModalityHandler(t, false)
	rec := postChat(t, h, imageBody("not-in-catalog"))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d want 200 body=%s（能力未知不拦截）", rec.Code, rec.Body)
	}
	if n := atomic.LoadInt64(calls); n != 1 {
		t.Errorf("upstream calls=%d want 1", n)
	}
}

// TestChatTextOnlyRoutingUnchanged 纯文本请求不受模态门控影响。
func TestChatTextOnlyRoutingUnchanged(t *testing.T) {
	h, _, calls := newModalityHandler(t, false)
	rec := postChat(t, h, `{"model":"text-only","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if n := atomic.LoadInt64(calls); n != 1 {
		t.Errorf("upstream calls=%d want 1", n)
	}
}

// TestModelsListingIncludesInputModalities /v1/models 透出 input_modalities，
// 能力未知的模型不宣称（issue #109）。
func TestModelsListingIncludesInputModalities(t *testing.T) {
	setModelCatalog(t, []upstream.ModelInfo{
		{ID: "vision", SupportsImages: true},
		{ID: "text-only", InputModalities: []string{"text"}},
		{ID: "unknown"},
	})
	h := NewHandler(Config{Pool: pool.New(""), Upstream: &upstream.Client{}})
	list := h.modelList()
	byID := map[string]map[string]any{}
	for _, entry := range list {
		byID[entry["id"].(string)] = entry
	}
	vision := byID["cn:vision"]
	mods, ok := vision["input_modalities"].([]string)
	if !ok || strings.Join(mods, ",") != "text,image" {
		t.Errorf("vision input_modalities=%v want text,image", vision["input_modalities"])
	}
	if vision["supports_images"] != true {
		t.Errorf("vision supports_images=%v want true（兼容字段保留）", vision["supports_images"])
	}
	textOnly := byID["cn:text-only"]
	if mods, ok := textOnly["input_modalities"].([]string); !ok || strings.Join(mods, ",") != "text" {
		t.Errorf("text-only input_modalities=%v want text", textOnly["input_modalities"])
	}
	if unknown := byID["cn:unknown"]; unknown["input_modalities"] != nil {
		t.Errorf("unknown model must not claim modalities: %v", unknown["input_modalities"])
	}
}
