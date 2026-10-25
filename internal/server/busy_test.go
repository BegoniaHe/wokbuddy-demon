package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
	"github.com/BegoniaHe/wokbuddy-demon/internal/livecfg"
	"github.com/BegoniaHe/wokbuddy-demon/internal/upstream"
)

type busyTestResponse struct {
	Code int    `json:"code"`
	Msg  string `json:"msg"`
	Data []struct {
		Model     string `json:"model"`
		BusyLevel string `json:"busy_level"`
	} `json:"data"`
}

func newBusyTestHandler(t *testing.T) (*Handler, *upstream.Client) {
	t.Helper()

	account := &auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}
	client := &upstream.Client{ChatBaseCN: "https://fake.example"}
	handler := NewHandler(&Config{
		Pool:        testPoolWith(account),
		Upstream:    client,
		Live:        livecfg.New(livecfg.Snapshot{}),
		modelsCache: &modelsCache{},
	})
	handler.modelsCache.ids = []upstream.ModelInfo{{ID: "glm-5.2"}, {ID: "kimi-k2"}}
	handler.modelsCache.fetched = time.Now()

	return handler, client
}

func postBusy(t *testing.T, handler *Handler, body string) *httptest.ResponseRecorder {
	t.Helper()

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v2/chat/busy", strings.NewReader(body)))

	return rec
}

func decodeBusy(t *testing.T, rec *httptest.ResponseRecorder) busyTestResponse {
	t.Helper()

	var out busyTestResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("parse %s: %v", rec.Body, err)
	}

	return out
}

// TestBusyEmptyAndMalformed 空列表返回 []，坏 JSON 400。
func TestBusyEmptyAndMalformed(t *testing.T) {
	t.Parallel()

	handler, _ := newBusyTestHandler(t)

	rec := postBusy(t, handler, `{"models":[]}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}

	if !strings.Contains(rec.Body.String(), `"data":[]`) {
		t.Fatalf("empty data must be []: %s", rec.Body)
	}

	rec = postBusy(t, handler, `{broken`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
}

// TestBusyModelLimit 单次预检模型数超过官方批量上限时整体 400：
// 逐个模型都要解析别名并扫描目录，无上限会被大列表放大成 CPU/内存消耗。
func TestBusyModelLimit(t *testing.T) {
	t.Parallel()

	handler, _ := newBusyTestHandler(t)

	models := make([]string, busyMaxModels)
	for i := range models {
		models[i] = "glm-5.2"
	}

	body, err := json.Marshal(map[string]any{"models": models})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	rec := postBusy(t, handler, string(body))
	if rec.Code != http.StatusOK {
		t.Fatalf("at limit: code=%d body=%s want 200", rec.Code, rec.Body)
	}

	if out := decodeBusy(t, rec); len(out.Data) != busyMaxModels {
		t.Fatalf("data len=%d want %d", len(out.Data), busyMaxModels)
	}

	over := make([]string, 0, busyMaxModels+1)
	over = append(over, models...)
	over = append(over, "glm-5.2")

	body, err = json.Marshal(map[string]any{"models": over})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	rec = postBusy(t, handler, string(body))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("over limit: code=%d body=%s want 400", rec.Code, rec.Body)
	}
}

// TestBusyUsesPoolState idle / busy / congested / unavailable / unknown 映射。
func TestBusyUsesPoolState(t *testing.T) {
	t.Parallel()

	handler, _ := newBusyTestHandler(t)
	handler.cfg.Live = livecfg.New(livecfg.Snapshot{HiddenModels: livecfg.NormalizeHiddenModels([]string{"hidden-model"})})
	handler.modelsCache.ids = append(handler.modelsCache.ids, upstream.ModelInfo{ID: "hidden-model"})

	handler.cfg.Pool.CooldownModelBusy("u1", "busy-model", "14003 model busy")

	reset := time.Now().Add(30 * time.Minute)
	handler.cfg.Pool.CooldownSoftForModel("u1", time.Minute, reset, "limited-model", "6004 model rate limit")
	// CooldownSoftForModel 只对已存在的模型建冷却；补进目录让预检能看到。
	handler.modelsCache.ids = append(handler.modelsCache.ids,
		upstream.ModelInfo{ID: "busy-model"}, upstream.ModelInfo{ID: "limited-model"})

	rec := postBusy(t, handler, `{"models":["glm-5.2","busy-model","limited-model","hidden-model","not-a-model"]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}

	out := decodeBusy(t, rec)

	want := map[string]string{
		"glm-5.2":       "idle",
		"busy-model":    "busy",
		"limited-model": "congested",
		"hidden-model":  "unavailable",
		"not-a-model":   "unknown",
	}
	for _, item := range out.Data {
		if item.BusyLevel != want[item.Model] {
			t.Errorf("%s busy_level=%q want %q", item.Model, item.BusyLevel, want[item.Model])
		}
	}

	if len(out.Data) != len(want) {
		t.Fatalf("data=%+v", out.Data)
	}
}

// TestBusyStaleCatalogStillKnown 目录缓存超过 TTL 仍应认得已知模型（只读预检
// 不应因为客户端长时间没刷新 /v1/models 就把全部模型报成 unknown）。
func TestBusyStaleCatalogStillKnown(t *testing.T) {
	t.Parallel()

	handler, _ := newBusyTestHandler(t)
	handler.modelsCache.fetched = time.Now().Add(-2 * time.Hour)

	rec := postBusy(t, handler, `{"models":["glm-5.2","not-a-model"]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}

	out := decodeBusy(t, rec)

	want := map[string]string{"glm-5.2": "idle", "not-a-model": "unknown"}
	for _, item := range out.Data {
		if item.BusyLevel != want[item.Model] {
			t.Errorf("%s busy_level=%q want %q", item.Model, item.BusyLevel, want[item.Model])
		}
	}
}

// TestBusyGlobalDisabledUnknown global 逃生门关闭时不误报 idle。
func TestBusyGlobalDisabledUnknown(t *testing.T) {
	t.Parallel()

	handler, _ := newBusyTestHandler(t)
	handler.modelsCache.ids = append(handler.modelsCache.ids, upstream.ModelInfo{ID: "glm-global"})

	rec := postBusy(t, handler, `{"models":["global:glm-global"]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}

	if out := decodeBusy(t, rec); out.Data[0].BusyLevel != "unknown" {
		t.Fatalf("global disabled must be unknown: %+v", out.Data)
	}
}

// TestBusyKeyRealmMismatchUnknown 命名 key 绑定与模型显式 realm 冲突 → unknown。
func TestBusyKeyRealmMismatchUnknown(t *testing.T) {
	t.Parallel()

	handler, _ := newBusyTestHandler(t)

	level := handler.modelBusyLevel("cn:glm-5.2", nil, "global")
	if level != "unknown" {
		t.Fatalf("level=%q want unknown", level)
	}
}

// TestBusyAliasResolution 配置别名指向的模型按别名目标查冷却。
func TestBusyAliasResolution(t *testing.T) {
	t.Parallel()

	handler, _ := newBusyTestHandler(t)
	handler.cfg.ModelAliases = map[string]string{"fast": "cn:busy-model"}
	handler.modelsCache.ids = append(handler.modelsCache.ids, upstream.ModelInfo{ID: "busy-model"})
	handler.cfg.Pool.CooldownModelBusy("u1", "busy-model", "14003 model busy")

	rec := postBusy(t, handler, `{"models":["fast"]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}

	out := decodeBusy(t, rec)
	if len(out.Data) != 1 || out.Data[0].Model != "fast" || out.Data[0].BusyLevel != "busy" {
		t.Fatalf("data=%+v want original model + busy", out.Data)
	}
}
