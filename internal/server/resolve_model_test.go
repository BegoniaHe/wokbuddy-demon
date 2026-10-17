package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/BegoniaHe/wokbuddy-demon/internal/upstream"
)

// TestResolveRequestModelAliases 别名解析：先剥请求 realm 前缀再查裸名；
// 目标显式带 realm 以目标为准，省略则继承请求侧 realm（裸请求 = cn）。
func TestResolveRequestModelAliases(t *testing.T) {
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
	h := &Handler{cfg: Config{ModelAliases: map[string]string{
		"claude-sonnet-4-5": "cn:hy4",
	}}}
	f := h.newChatFlow(chatInput{
		body:  []byte(`{"model":"claude-sonnet-4-5","messages":[]}`),
		model: "claude-sonnet-4-5",
	}, &fakeResponder{})

	if f.realm != "cn" || f.bareModel != "hy4" {
		t.Fatalf("resolved realm/bare = (%q,%q), want (cn,hy4)", f.realm, f.bareModel)
	}
	var body map[string]any
	if err := json.Unmarshal(f.body, &body); err != nil {
		t.Fatalf("body not json: %v", err)
	}
	if body["model"] != "hy4" {
		t.Fatalf("outbound model = %v, want hy4", body["model"])
	}
}

// TestModelsEndpointIncludesAliases 别名条目追加到 /v1/models，且与真实模型
// 同 id 时以别名为准（请求解析同样别名优先）。
func TestModelsEndpointIncludesAliases(t *testing.T) {
	dynamicModelsCache.Lock()
	dynamicModelsCache.ids = nil
	dynamicModelsCache.fetched = time.Time{}
	dynamicModelsCache.lastFail = time.Time{}
	dynamicModelsCache.Unlock()

	h := NewHandler(Config{
		Pool:     testPoolWith(),
		Upstream: upstream.New(),
		ModelAliases: map[string]string{
			"claude-sonnet-4-5": "cn:hy4",
			"gpt-5-codex":       "hy4",
			"claude-global":     "global:hy4",
		},
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
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
