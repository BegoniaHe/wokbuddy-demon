package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/BegoniaHe/wokbuddy-demon/internal/apikey"
	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
	"github.com/BegoniaHe/wokbuddy-demon/internal/upstream"
)

func containsString(s, sub string) bool { return strings.Contains(s, sub) }

func modelIDs(entries []map[string]any) []string {
	out := make([]string, 0, len(entries))
	for _, entry := range entries {
		out = append(out, stringValue(entry["id"]))
	}

	return out
}

func hasID(entries []map[string]any, id string) bool {
	for _, entry := range entries {
		if stringValue(entry["id"]) == id {
			return true
		}
	}

	return false
}

// TestFilterModelsForKeyScope realm 绑定、白名单、别名最终目标与冲突排除。
//
//nolint:cyclop // 表驱动用例，分支多但都是独立断言。
func TestFilterModelsForKeyScope(t *testing.T) {
	t.Parallel()

	aliases := map[string]string{
		"fast":    "global:gpt-5",
		"cnfast":  "glm-5.2",
		"cnfixed": "cn:glm-5.2",
	}
	entries := []map[string]any{
		{"id": "cn:glm-5.2"},
		{"id": "global:gpt-5"},
		{"id": "fast"},
		{"id": "cnfast"},
		{"id": "cnfixed"},
	}

	store := apikey.New("")

	unbounded, _, err := store.Create("unbounded")
	if err != nil {
		t.Fatal(err)
	}

	global, _, err := store.Create("global")
	if err != nil {
		t.Fatal(err)
	}

	if ok, setErr := store.SetRealm(global.ID, "global"); !ok || setErr != nil {
		t.Fatalf("set realm: %v %v", ok, setErr)
	}

	whitelisted, _, err := store.Create("whitelisted")
	if err != nil {
		t.Fatal(err)
	}

	if ok, setErr := store.UpdateLimits(whitelisted.ID, apikey.Limits{RPM: 0, TPM: 0, MaxConcurrent: 0, Models: []string{"global:gpt-5"}}); !ok || setErr != nil {
		t.Fatalf("update limits: %v %v", ok, setErr)
	}

	aliasList, _, err := store.Create("alias-list")
	if err != nil {
		t.Fatal(err)
	}

	if ok, setErr := store.UpdateLimits(aliasList.ID, apikey.Limits{RPM: 0, TPM: 0, MaxConcurrent: 0, Models: []string{"glm-*"}}); !ok || setErr != nil {
		t.Fatalf("update limits: %v %v", ok, setErr)
	}

	handler := NewHandler(&Config{Keys: store, ModelAliases: aliases})

	cases := []struct {
		name    string
		ku      *keyUsage
		want    []string
		notWant []string
	}{
		{
			name: "未绑定与无白名单保留全部",
			ku:   &keyUsage{id: unbounded.ID},
			want: []string{"cn:glm-5.2", "global:gpt-5", "fast", "cnfast", "cnfixed"},
		},
		{
			name:    "绑定 global 排除显式 cn，裸别名跟随绑定",
			ku:      &keyUsage{id: global.ID, realm: "global"},
			want:    []string{"global:gpt-5", "fast", "cnfast"},
			notWant: []string{"cn:glm-5.2", "cnfixed"},
		},
		{
			name:    "白名单校验别名最终目标",
			ku:      &keyUsage{id: whitelisted.ID},
			want:    []string{"global:gpt-5", "fast"},
			notWant: []string{"cn:glm-5.2", "cnfast", "cnfixed"},
		},
		{
			name:    "白名单 glob 匹配裸名目标",
			ku:      &keyUsage{id: aliasList.ID},
			want:    []string{"cn:glm-5.2", "cnfast", "cnfixed"},
			notWant: []string{"global:gpt-5", "fast"},
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			got := handler.filterModelsForKey(entries, testCase.ku)
			for _, id := range testCase.want {
				if !hasID(got, id) {
					t.Fatalf("缺少 %q: %v", id, modelIDs(got))
				}
			}

			for _, id := range testCase.notWant {
				if hasID(got, id) {
					t.Fatalf("不应包含 %q: %v", id, modelIDs(got))
				}
			}
		})
	}
}

func newModelsByKeyHandler(t *testing.T) (*Handler, *apikey.Store, *atomic.Int64) {
	t.Helper()

	store := apikey.New("")

	var calls atomic.Int64

	upstreamClient := newFakeUpstream(t, func(string) (int, string, bool) {
		calls.Add(1)
		return 200, sseOK, true
	})

	cache := &modelsCache{
		ids:     []upstream.ModelInfo{{ID: "glm-5.2"}, {ID: "glm-other"}},
		fetched: time.Now(),
	}

	handler := NewHandler(&Config{
		Pool:          testPoolWith(&auth.Auth{UID: "cn1", AccessToken: "cn-token", ExpiresAt: 9999999999}),
		Upstream:      upstreamClient,
		APIKey:        "secret",
		Keys:          store,
		modelsCache:   cache,
		GlobalEnabled: false,
	})

	return handler, store, &calls
}

func getModelsAs(t *testing.T, handler *Handler, key, path string, anthropic bool) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequest(http.MethodGet, path, nil)
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}

	if anthropic {
		req.Header.Set("Anthropic-Version", "2023-06-01")
	}

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	return rec
}

func decodeOpenAIModelIDs(t *testing.T, rec *httptest.ResponseRecorder) []string {
	t.Helper()

	var body struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v body=%s", err, rec.Body)
	}

	return modelIDs(body.Data)
}

// TestModelsByKeyDefaultAndUnauth 默认 api_key / 未鉴权保持全量。
func TestModelsByKeyDefaultAndUnauth(t *testing.T) {
	t.Parallel()

	handler, _, _ := newModelsByKeyHandler(t)

	rec := getModelsAs(t, handler, "secret", "/v1/models", false)
	if rec.Code != http.StatusOK {
		t.Fatalf("默认密钥 code=%d", rec.Code)
	}

	ids := decodeOpenAIModelIDs(t, rec)
	if len(ids) != 2 || ids[0] != "cn:glm-5.2" || ids[1] != "cn:glm-other" {
		t.Fatalf("默认密钥 ids=%v", ids)
	}

	// api_key 为空且无命名密钥 = 未鉴权，仍返回全量。
	unauth := NewHandler(&Config{
		Pool:          testPoolWith(&auth.Auth{UID: "cn1", AccessToken: "cn-token", ExpiresAt: 9999999999}),
		Upstream:      newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true }),
		modelsCache:   &modelsCache{ids: []upstream.ModelInfo{{ID: "glm-5.2"}, {ID: "glm-other"}}, fetched: time.Now()},
		GlobalEnabled: false,
	})

	rec = getModelsAs(t, unauth, "", "/v1/models", false)
	if rec.Code != http.StatusOK {
		t.Fatalf("未鉴权 code=%d", rec.Code)
	}

	ids = decodeOpenAIModelIDs(t, rec)
	if len(ids) != 2 || ids[0] != "cn:glm-5.2" || ids[1] != "cn:glm-other" {
		t.Fatalf("未鉴权 ids=%v", ids)
	}
}

// TestModelsByKeyNamedRealmAndWhitelist 命名密钥按 realm/白名单裁剪，且不触发
// 额外上游请求。
//
//nolint:cyclop // 表驱动用例，分支多但都是独立断言。
func TestModelsByKeyNamedRealmAndWhitelist(t *testing.T) {
	t.Parallel()

	handler, store, calls := newModelsByKeyHandler(t)

	global, rawGlobal, err := store.Create("global")
	if err != nil {
		t.Fatal(err)
	}

	if ok, setErr := store.SetRealm(global.ID, "global"); !ok || setErr != nil {
		t.Fatalf("set realm: %v %v", ok, setErr)
	}

	limited, rawLimited, err := store.Create("limited")
	if err != nil {
		t.Fatal(err)
	}

	if ok, setErr := store.UpdateLimits(limited.ID, apikey.Limits{RPM: 0, TPM: 0, MaxConcurrent: 0, Models: []string{"cn:glm-5.2"}}); !ok || setErr != nil {
		t.Fatalf("update limits: %v %v", ok, setErr)
	}

	rec := getModelsAs(t, handler, rawGlobal, "/v1/models", false)
	if ids := decodeOpenAIModelIDs(t, rec); len(ids) != 0 {
		t.Fatalf("绑定 global 不应看到 cn 模型: %v", ids)
	}

	rec = getModelsAs(t, handler, rawLimited, "/v1/models", false)

	ids := decodeOpenAIModelIDs(t, rec)
	if len(ids) != 1 || ids[0] != "cn:glm-5.2" {
		t.Fatalf("白名单裁剪错误: %v", ids)
	}

	if calls.Load() != 0 {
		t.Fatalf("模型列表不应触发额外上游请求: calls=%d", calls.Load())
	}
}

// TestModelsByKeyRetrieveNotFound 未授权模型返回 404，且不泄露存在性。
func TestModelsByKeyRetrieveNotFound(t *testing.T) {
	t.Parallel()

	handler, store, _ := newModelsByKeyHandler(t)

	keyRec, raw, err := store.Create("limited")
	if err != nil {
		t.Fatal(err)
	}

	if ok, setErr := store.UpdateLimits(keyRec.ID, apikey.Limits{RPM: 0, TPM: 0, MaxConcurrent: 0, Models: []string{"cn:glm-5.2"}}); !ok || setErr != nil {
		t.Fatalf("update limits: %v %v", ok, setErr)
	}

	ok := getModelsAs(t, handler, raw, "/v1/models/cn:glm-5.2", false)
	if ok.Code != http.StatusOK {
		t.Fatalf("授权模型 code=%d body=%s", ok.Code, ok.Body)
	}

	denied := getModelsAs(t, handler, raw, "/v1/models/cn:glm-other", false)
	if denied.Code != http.StatusNotFound {
		t.Fatalf("未授权模型 code=%d body=%s", denied.Code, denied.Body)
	}

	if body := denied.Body.String(); !containsString(body, "model_not_found") ||
		containsString(body, "cn:glm-other") {
		t.Fatalf("404 不应泄露模型存在性: %s", body)
	}
}

// TestModelsByKeyAnthropicPagination Anthropic 分页字段在过滤后计算。
//
//nolint:cyclop // 表驱动用例，分支多但都是独立断言。
func TestModelsByKeyAnthropicPagination(t *testing.T) {
	t.Parallel()

	handler, store, _ := newModelsByKeyHandler(t)

	keyRec, raw, err := store.Create("limited")
	if err != nil {
		t.Fatal(err)
	}

	if ok, setErr := store.UpdateLimits(keyRec.ID, apikey.Limits{RPM: 0, TPM: 0, MaxConcurrent: 0, Models: []string{"cn:glm-other"}}); !ok || setErr != nil {
		t.Fatalf("update limits: %v %v", ok, setErr)
	}

	rec := getModelsAs(t, handler, raw, "/v1/models", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}

	var body struct {
		Data    []map[string]any `json:"data"`
		FirstID string           `json:"first_id"`
		LastID  string           `json:"last_id"`
		HasMore bool             `json:"has_more"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if len(body.Data) != 1 || stringValue(body.Data[0]["id"]) != "cn:glm-other" {
		t.Fatalf("data=%v", body.Data)
	}

	if body.FirstID != "cn:glm-other" || body.LastID != "cn:glm-other" || body.HasMore {
		t.Fatalf("分页字段错误: first=%q last=%q more=%v", body.FirstID, body.LastID, body.HasMore)
	}
}

// TestModelsByKeyHiddenPriority models.hidden 仍优先排除，命名密钥无法看回。
func TestModelsByKeyHiddenPriority(t *testing.T) {
	t.Parallel()

	handler, store, _ := newModelsByKeyHandler(t)
	handler.cfg.HiddenModels = map[string]struct{}{"cn:glm-other": {}}

	_, raw, err := store.Create("client")
	if err != nil {
		t.Fatal(err)
	}

	rec := getModelsAs(t, handler, raw, "/v1/models", false)

	ids := decodeOpenAIModelIDs(t, rec)
	if len(ids) != 1 || ids[0] != "cn:glm-5.2" {
		t.Fatalf("hidden 未被排除: %v", ids)
	}
}

// TestModelsByKeyRetrieveAnthropicNotFound Anthropic 形态的 404 错误体。
func TestModelsByKeyRetrieveAnthropicNotFound(t *testing.T) {
	t.Parallel()

	handler, store, _ := newModelsByKeyHandler(t)

	keyRec, raw, err := store.Create("limited")
	if err != nil {
		t.Fatal(err)
	}

	if ok, setErr := store.UpdateLimits(keyRec.ID, apikey.Limits{RPM: 0, TPM: 0, MaxConcurrent: 0, Models: []string{"cn:glm-5.2"}}); !ok || setErr != nil {
		t.Fatalf("update limits: %v %v", ok, setErr)
	}

	rec := getModelsAs(t, handler, raw, "/v1/models/cn:glm-other", true)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}

	body := rec.Body.String()
	if !containsString(body, `"type":"error"`) || !containsString(body, "not_found_error") {
		t.Fatalf("Anthropic 404 错误体不正确: %s", body)
	}

	if containsString(body, "cn:glm-other") {
		t.Fatalf("404 不应泄露模型存在性: %s", body)
	}
}
