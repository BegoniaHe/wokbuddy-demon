package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
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
