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
	"github.com/BegoniaHe/wokbuddy-demon/internal/session"
	"github.com/BegoniaHe/wokbuddy-demon/internal/upstream"
)

// TestResolveRequestModelAliases 别名解析：先剥请求 realm 前缀再查裸名；
// 目标显式带 realm 以目标为准，省略则继承请求侧 realm（裸请求 = cn）。
func TestResolveRequestModelAliases(t *testing.T) {
	t.Parallel()

	aliases := map[string]string{
		"claude-sonnet": "cn:hy4",
		"codex":         "hy4",
		"global-only":   "global:gpt-5",
	}

	cases := []struct {
		in        string
		wantRealm string
		wantBare  string
	}{
		{"claude-sonnet", "cn", "hy4"},
		{"cn:claude-sonnet", "cn", "hy4"},
		{"global:claude-sonnet", "cn", "hy4"},
		{"codex", "cn", "hy4"},
		{"global:codex", "global", "hy4"},
		{"global-only", "global", "gpt-5"},
		{"global:global-only", "global", "gpt-5"},
		{"not-aliased", "cn", "not-aliased"},
		{"global:not-aliased", "global", "not-aliased"},
	}
	for _, tc := range cases {
		realm, bare := resolveRequestModel(tc.in, aliases)
		if realm != tc.wantRealm || bare != tc.wantBare {
			t.Errorf("resolveRequestModel(%q) = (%q,%q), want (%q,%q)",
				tc.in, realm, bare, tc.wantRealm, tc.wantBare)
		}
	}
}

// TestResolveRequestModelNoAliases 未配置别名时与 resolveModel 完全一致。
func TestResolveRequestModelNoAliases(t *testing.T) {
	t.Parallel()

	for _, in := range []string{"glm-5.2", "cn:glm-5.2", "global:glm-5.2", "foo:bar"} {
		wantRealm, wantBare := resolveModel(in)

		realm, bare := resolveRequestModel(in, nil)
		if realm != wantRealm || bare != wantBare {
			t.Fatalf("%q: got (%q,%q), want (%q,%q)", in, realm, bare, wantRealm, wantBare)
		}
	}
}

// TestChatFlowAppliesModelAlias 命中别名后出站 body 的 model 重写为目标裸名，
// 且 flow 使用解析后的 realm/bare 走选号与记账。
func TestChatFlowAppliesModelAlias(t *testing.T) {
	t.Parallel()

	h := &Handler{cfg: Config{ModelAliases: map[string]string{
		"claude-sonnet-4-5": "cn:hy4",
	}}}
	chatFlow2 := h.newChatFlow(&chatInput{
		body:  []byte(`{"model":"claude-sonnet-4-5","messages":[]}`),
		model: "claude-sonnet-4-5",
	}, &fakeResponder{})

	if chatFlow2.realm != "cn" || chatFlow2.bareModel != "hy4" {
		t.Fatalf("resolved realm/bare = (%q,%q), want (cn,hy4)", chatFlow2.realm, chatFlow2.bareModel)
	}

	var body map[string]any
	if err := json.Unmarshal(chatFlow2.body, &body); err != nil {
		t.Fatalf("body not json: %v", err)
	}

	if body["model"] != "hy4" {
		t.Fatalf("outbound model = %v, want hy4", body["model"])
	}
}

// TestModelsEndpointIncludesAliases 别名条目追加到 /v1/models，且与真实模型
// 同 id 时以别名为准（请求解析同样别名优先）。
func TestModelsEndpointIncludesAliases(t *testing.T) {
	t.Parallel()

	cache := &modelsCache{}

	handler := NewHandler(&Config{
		Pool:        testPoolWith(),
		Upstream:    upstream.New(),
		modelsCache: cache,
		ModelAliases: map[string]string{
			"claude-sonnet-4-5": "cn:hy4",
			"gpt-5-codex":       "hy4",
			"claude-global":     "global:hy4",
		},
	})
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}

	var resp struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("bad json: %v", err)
	}

	got := map[string]map[string]any{}

	for _, m := range resp.Data {
		id, _ := m["id"].(string)
		got[id] = m
	}

	want := map[string]string{
		"claude-sonnet-4-5": "cn:hy4",
		"gpt-5-codex":       "cn:hy4",
		"claude-global":     "global:hy4",
	}
	for id, upstreamID := range want {
		m, ok := got[id]
		if !ok {
			t.Fatalf("alias %q missing from /v1/models: %v", id, got)
		}

		if m["upstream_id"] != upstreamID {
			t.Errorf("%s upstream_id=%v, want %s", id, m["upstream_id"], upstreamID)
		}
	}
}

func modelsTestHandler() *Handler {
	return NewHandler(&Config{
		Pool:     testPoolWith(),
		Upstream: upstream.New(),
		// 独立缓存：nil 会落到进程级 dynamicModelsCache，并行用例填充后
		// /v1/models 会混入别的目录，列表断言随执行顺序漂移。
		modelsCache: &modelsCache{},
		ModelAliases: map[string]string{
			"claude-sonnet-4-5": "cn:hy4",
		},
	})
}

func doModelsRequest(handler *Handler, path string, anthropic bool) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if anthropic {
		req.Header.Set("Anthropic-Version", "2023-06-01")
	}

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	return rec
}

// TestModelsRetrieveOpenAI 覆盖 OpenAI SDK 的 models.retrieve。
func TestModelsRetrieveOpenAI(t *testing.T) {
	t.Parallel()

	rec := doModelsRequest(modelsTestHandler(), "/v1/models/claude-sonnet-4-5", false)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}

	var entry map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &entry); err != nil {
		t.Fatal(err)
	}

	if entry["id"] != "claude-sonnet-4-5" || entry["object"] != "model" {
		t.Errorf("entry=%v", entry)
	}
}

// TestModelsRetrieveAnthropic 覆盖 Anthropic SDK 的 models.retrieve 形态。
func TestModelsRetrieveAnthropic(t *testing.T) {
	t.Parallel()

	rec := doModelsRequest(modelsTestHandler(), "/v1/models/claude-sonnet-4-5", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}

	var entry map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &entry); err != nil {
		t.Fatal(err)
	}

	if entry["type"] != "model" || entry["display_name"] != "claude-sonnet-4-5" || entry["lifecycle"] != "active" {
		t.Errorf("entry=%v", entry)
	}
}

// TestModelsListAnthropic Anthropic SDK models.list 的分页字段与条目类型。
func TestModelsListAnthropic(t *testing.T) {
	t.Parallel()

	rec := doModelsRequest(modelsTestHandler(), "/v1/models", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}

	var page struct {
		Data    []map[string]any `json:"data"`
		HasMore bool             `json:"has_more"`
		FirstID string           `json:"first_id"`
		LastID  string           `json:"last_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}

	if len(page.Data) != 1 || page.Data[0]["type"] != "model" || page.HasMore {
		t.Fatalf("page=%v", page)
	}

	if page.FirstID != "claude-sonnet-4-5" || page.LastID != "claude-sonnet-4-5" {
		t.Errorf("pagination first=%q last=%q", page.FirstID, page.LastID)
	}
}

// TestModelsRetrieveMissing 未收录模型返回 OpenAI 错误信封。
func TestModelsRetrieveMissing(t *testing.T) {
	t.Parallel()

	rec := doModelsRequest(modelsTestHandler(), "/v1/models/missing", false)
	if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "model_not_found") {
		t.Errorf("code=%d body=%s", rec.Code, rec.Body)
	}
}

// TestChatFlowAliasStickyKeepsRealm 别名目标省略 realm 时，粘性可用性过滤必须
// 收到继承请求侧后的 "[realm:]model"：global 请求不能被当成 cn 绑定到 cn 账号。
func TestChatFlowAliasStickyKeepsRealm(t *testing.T) {
	t.Parallel()

	var seen []string

	sess := session.New(session.Config{
		TTL:   time.Minute,
		Store: newBindStore(),
		AvailableForModel: func(model string) []string {
			seen = append(seen, model)
			switch model {
			case "global:hy4":
				return []string{"g1"}
			case "cn:hy4":
				return []string{"cn1"}
			}

			return nil
		},
	})
	p := testPoolWith(
		&auth.Auth{UID: "cn1", AccessToken: "at-cn", ExpiresAt: 9999999999},
		&auth.Auth{UID: "g1", Domain: "acct.workbuddy.ai", AccessToken: "at-g", ExpiresAt: 9999999999},
	)
	handler := NewHandler(&Config{
		Pool:         p,
		Upstream:     upstream.New(),
		Session:      sess,
		ModelAliases: map[string]string{"codex": "hy4"},
	})

	cases := []struct {
		model   string
		wantSee string
		wantUID string
	}{
		{"global:codex", "global:hy4", "g1"},
		{"codex", "cn:hy4", "cn1"},
	}
	for _, testCase := range cases {
		seen = seen[:0]

		chatFlow2 := handler.newChatFlow(&chatInput{
			body:  []byte(`{"model":"` + testCase.model + `","messages":[],"metadata":{"conversation_id":"` + testCase.wantUID + `"}}`),
			model: testCase.model,
		}, &fakeResponder{})
		if len(seen) == 0 || seen[0] != testCase.wantSee {
			t.Errorf("%s: AvailableForModel 收到 %v，want 首个 %q", testCase.model, seen, testCase.wantSee)
		}

		if chatFlow2.realm+":"+chatFlow2.bareModel != testCase.wantSee {
			t.Errorf("%s: resolved=%s:%s，want %s", testCase.model, chatFlow2.realm, chatFlow2.bareModel, testCase.wantSee)
		}

		if chatFlow2.stickyUID != testCase.wantUID {
			t.Errorf("%s: stickyUID=%q，want %q", testCase.model, chatFlow2.stickyUID, testCase.wantUID)
		}
	}
}

// hiddenModelsTestHandler 造一个已填充 CN 目录缓存 + 含别名的 handler，
// 用于 models.hidden 判定（独立缓存，避免落到进程级 dynamicModelsCache）。
func hiddenModelsTestHandler(hidden []string) *Handler {
	cache := &modelsCache{}
	cache.ids = []upstream.ModelInfo{{ID: "glm-5.2"}, {ID: "kimi-k3"}}
	cache.fetched = time.Now()

	return NewHandler(&Config{
		Pool:         testPoolWith(),
		Upstream:     upstream.New(),
		modelsCache:  cache,
		HiddenModels: livecfg.NormalizeHiddenModels(hidden),
		ModelAliases: map[string]string{
			"alias-cn":     "cn:kimi-k3",
			"alias-global": "global:kimi-k3",
			"alias-ok":     "cn:glm-5.2",
		},
	})
}

// modelsListIDs 取 /v1/models 的 id 集合。
func modelsListIDs(t *testing.T, handler *Handler) map[string]bool {
	t.Helper()

	rec := doModelsRequest(handler, "/v1/models", false)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}

	var resp struct {
		Data []map[string]any `json:"data"`
	}

	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("bad json: %v", err)
	}

	ids := make(map[string]bool, len(resp.Data))

	for _, m := range resp.Data {
		if id, ok := m["id"].(string); ok {
			ids[id] = true
		}
	}

	return ids
}

// TestModelsHiddenFiltersList 裸名黑名单：/v1/models 不列出该模型，且别名条目
// （别名名或其目标模型命中）一并过滤——否则隐藏模型会经由别名泄露。
func TestModelsHiddenFiltersList(t *testing.T) {
	t.Parallel()

	ids := modelsListIDs(t, hiddenModelsTestHandler([]string{"kimi-k3"}))

	// CN 条目 id 带 "cn:" 前缀（/v1/models 的既有形态）。
	if ids["cn:kimi-k3"] || ids["kimi-k3"] {
		t.Errorf("隐藏模型不应出现在 /v1/models: %v", ids)
	}

	if ids["alias-cn"] || ids["alias-global"] {
		t.Errorf("指向隐藏模型的别名不应列出: %v", ids)
	}

	if !ids["cn:glm-5.2"] || !ids["alias-ok"] {
		t.Errorf("未隐藏的模型与别名必须保留: %v", ids)
	}
}

// TestModelsHiddenRealmScoped cn: 前缀只藏 cn，不误伤 global。
func TestModelsHiddenRealmScoped(t *testing.T) {
	t.Parallel()

	handler := hiddenModelsTestHandler([]string{"cn:kimi-k3"})

	ids := modelsListIDs(t, handler)
	if ids["cn:kimi-k3"] {
		t.Errorf("cn:kimi-k3 应从 CN 目录移除: %v", ids)
	}

	if handler.modelHidden("global", "kimi-k3") {
		t.Error("cn: 前缀不得影响 global 域")
	}

	if !handler.modelHidden("cn", "kimi-k3") {
		t.Error("cn: 前缀应命中 cn 域")
	}
}

// TestChatHiddenModelRejected 点名调用隐藏模型：400 model_unavailable、
// 不打上游、不占选号、不冷却账号。
func TestChatHiddenModelRejected(t *testing.T) {
	t.Parallel()

	var calls int

	client := newFakeUpstream(t, func(string) (int, string, bool) {
		calls++

		return http.StatusOK, sseOK, true
	})
	state := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	handler := NewHandler(&Config{
		Pool:         state,
		Upstream:     client,
		HiddenModels: livecfg.NormalizeHiddenModels([]string{"glm-5.2"}),
	})

	rec := postChat(t, handler, `{"model":"glm-5.2","messages":[]}`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "model_unavailable") {
		t.Fatalf("code=%d body=%s want 400 model_unavailable", rec.Code, rec.Body)
	}

	if calls != 0 {
		t.Errorf("隐藏模型不得打上游，calls=%d", calls)
	}

	if st := statusOf(t, state, "u1"); st.Cooling || st.Disabled || st.ErrTotal != 0 {
		t.Errorf("隐藏模型不应罚账号: %+v", st)
	}
}

// TestCountTokensHiddenModelRejected /v1/messages/count_tokens 同口径：
// 隐藏模型返回 400，不返回 token 估算。
func TestCountTokensHiddenModelRejected(t *testing.T) {
	t.Parallel()

	handler := NewHandler(&Config{
		Pool:         testPoolWith(),
		Upstream:     upstream.New(),
		modelsCache:  &modelsCache{},
		HiddenModels: livecfg.NormalizeHiddenModels([]string{"cn:glm-5.2"}),
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens",
		strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Anthropic-Version", "2023-06-01")

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d body=%s want 400", rec.Code, rec.Body)
	}

	if !strings.Contains(rec.Body.String(), "hidden") {
		t.Errorf("错误应说明模型被隐藏: %s", rec.Body)
	}

	if strings.Contains(rec.Body.String(), "input_tokens") {
		t.Errorf("不得返回 token 估算: %s", rec.Body)
	}
}
