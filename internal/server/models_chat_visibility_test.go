package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/BegoniaHe/wokbuddy-demon/internal/pool"
	"github.com/BegoniaHe/wokbuddy-demon/internal/upstream"
)

func chatVisibilityHandler(aliases map[string]string) *Handler {
	cache := modelCatalog([]upstream.ModelInfo{
		{ID: "visible", ChatVisible: new(true)},
		{ID: "undecided"},
		{ID: "hidden", ChatVisible: new(false)},
	})

	return NewHandler(&Config{
		Pool:         pool.New(""),
		Upstream:     &upstream.Client{},
		ModelAliases: aliases,
		modelsCache:  cache,
	})
}

func modelIDsFromList(t *testing.T, body []byte) map[string]bool {
	t.Helper()

	var resp map[string]any
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("unmarshal models: %v", err)
	}

	data, _ := resp["data"].([]any)
	ids := make(map[string]bool, len(data))

	for _, item := range data {
		entry, _ := item.(map[string]any)
		if id, ok := entry["id"].(string); ok {
			ids[id] = true
		}
	}

	return ids
}

// TestModelsListHidesChatInvisible OpenAI / Anthropic 两种列表都跳过
// ChatVisible=false 的模型。
func TestModelsListHidesChatInvisible(t *testing.T) {
	t.Parallel()

	handler := chatVisibilityHandler(nil)

	openaiRec := httptest.NewRecorder()
	handler.ServeHTTP(openaiRec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))

	openaiIDs := modelIDsFromList(t, openaiRec.Body.Bytes())
	if !openaiIDs["cn:visible"] || !openaiIDs["cn:undecided"] {
		t.Errorf("OpenAI list missing visible models: %v", openaiIDs)
	}

	if openaiIDs["cn:hidden"] {
		t.Errorf("OpenAI list leaked hidden model: %v", openaiIDs)
	}

	anthropicReq := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	anthropicReq.Header.Set("Anthropic-Version", "2023-06-01")

	anthropicRec := httptest.NewRecorder()
	handler.ServeHTTP(anthropicRec, anthropicReq)

	anthropicIDs := modelIDsFromList(t, anthropicRec.Body.Bytes())
	if !anthropicIDs["cn:visible"] || anthropicIDs["cn:hidden"] {
		t.Errorf("Anthropic list filter mismatch: %v", anthropicIDs)
	}
}

// TestModelRetrieveHidesChatInvisible 单个模型查询自然 404；未隐藏模型仍可取。
func TestModelRetrieveHidesChatInvisible(t *testing.T) {
	t.Parallel()

	handler := chatVisibilityHandler(nil)

	hiddenRec := httptest.NewRecorder()
	handler.ServeHTTP(hiddenRec, httptest.NewRequest(http.MethodGet, "/v1/models/cn:hidden", nil))

	if hiddenRec.Code != http.StatusNotFound {
		t.Fatalf("hidden model code=%d want 404", hiddenRec.Code)
	}

	visibleRec := httptest.NewRecorder()
	handler.ServeHTTP(visibleRec, httptest.NewRequest(http.MethodGet, "/v1/models/cn:visible", nil))

	if visibleRec.Code != http.StatusOK {
		t.Fatalf("visible model code=%d want 200", visibleRec.Code)
	}
}

// TestAliasEntriesHideChatInvisible 指向隐藏模型的别名、别名名字本身命中隐藏集时
// 都不出现；目标不在目录中的别名保持现状。
func TestAliasEntriesHideChatInvisible(t *testing.T) {
	t.Parallel()

	handler := chatVisibilityHandler(map[string]string{
		"hidden-alias":  "hidden",
		"hidden":        "visible",
		"visible-alias": "visible",
		"missing-alias": "not-in-catalog",
	})

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))

	ids := modelIDsFromList(t, rec.Body.Bytes())

	for _, want := range []string{"visible-alias", "missing-alias", "cn:visible"} {
		if !ids[want] {
			t.Errorf("missing %q in %v", want, ids)
		}
	}

	for _, hidden := range []string{"hidden-alias", "hidden"} {
		if ids[hidden] {
			t.Errorf("hidden alias %q leaked in %v", hidden, ids)
		}
	}
}

// TestChatHiddenDoesNotChangeFetchCatalog 展示层过滤不改内部模型目录/面板数据源。
func TestChatHiddenDoesNotChangeFetchCatalog(t *testing.T) {
	t.Parallel()

	handler := chatVisibilityHandler(nil)

	models := handler.fetchDynamicModels()
	if len(models) != 3 {
		t.Fatalf("fetchDynamicModels=%d want 3（含隐藏模型）", len(models))
	}

	seen := make(map[string]bool, len(models))
	for i := range models {
		seen[models[i].ID] = true
	}

	if !seen["hidden"] {
		t.Fatalf("内部目录不应丢失隐藏模型：%v", seen)
	}
}
