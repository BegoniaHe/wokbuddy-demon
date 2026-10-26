package upstream

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
)

// TestSelectChatAgentPriority 官方顺序：tags 含 default → name=="cli" →
// 第一个 models 非空的 agent。
func TestSelectChatAgentPriority(t *testing.T) {
	t.Parallel()

	agents := []modelAgent{
		{Name: "first", Models: []string{"first-model"}},
		{Name: "cli", Models: []string{"cli-model"}},
		{Name: "picked", Models: []string{"default-model"}, Tags: []string{"default"}},
	}

	agent, ok := selectChatAgent(agents)
	if !ok || agent.Name != "picked" {
		t.Fatalf("default agent=%+v ok=%v want picked", agent, ok)
	}

	agents = agents[:2]

	agent, ok = selectChatAgent(agents)
	if !ok || agent.Name != "cli" {
		t.Fatalf("cli agent=%+v ok=%v want cli", agent, ok)
	}

	agents = []modelAgent{{Name: "empty"}, {Name: "first", Models: []string{"x"}}}

	agent, ok = selectChatAgent(agents)
	if !ok || agent.Name != "first" {
		t.Fatalf("first non-empty agent=%+v ok=%v want first", agent, ok)
	}

	if _, ok = selectChatAgent([]modelAgent{{Name: "empty"}}); ok {
		t.Fatal("没有可用 agent 时不应选出默认 agent")
	}
}

// TestChatVisibilityRules 复刻 selectConversationModels 的最终集合：
// H ∪ custom ∪（仅 H 为空时）tags 含 chat。
//
//nolint:cyclop // 逐档校验可见性规则，线性展开更直观。
func TestChatVisibilityRules(t *testing.T) {
	t.Parallel()

	infos := []ModelInfo{
		{ID: "hit", Name: "Hit Display"},
		{ID: "chat", Tags: []string{"chat"}},
		{ID: "custom", Tags: []string{"custom"}},
		{ID: "custom-local:local", Name: "local"},
		{ID: "other"},
	}

	hitAgent := modelAgent{Name: "default", Models: []string{"hit"}, Tags: []string{"default"}}
	whitelist := newModelChatWhitelist([]modelAgent{hitAgent}, infos)
	whitelist.apply(infos)

	want := map[string]bool{
		"hit": true, "chat": false, "custom": true, "custom-local:local": true, "other": false,
	}
	for i := range infos {
		if infos[i].ChatVisible == nil || *infos[i].ChatVisible != want[infos[i].ID] {
			t.Errorf("%s ChatVisible=%v want %v", infos[i].ID, infos[i].ChatVisible, want[infos[i].ID])
		}
	}

	// H 为空：保留 chat 与 custom，其余隐藏。
	missingAgent := modelAgent{Name: "cli", Models: []string{"absent"}}
	emptyHit := []ModelInfo{
		{ID: "chat", Tags: []string{"chat"}},
		{ID: "custom", Tags: []string{"custom"}},
		{ID: "other"},
	}
	whitelist = newModelChatWhitelist([]modelAgent{missingAgent}, emptyHit)
	whitelist.apply(emptyHit)

	if emptyHit[0].ChatVisible == nil || !*emptyHit[0].ChatVisible {
		t.Error("H 为空时 chat 模型应可见")
	}

	if emptyHit[1].ChatVisible == nil || !*emptyHit[1].ChatVisible {
		t.Error("H 为空时 custom 模型应可见")
	}

	if emptyHit[2].ChatVisible == nil || *emptyHit[2].ChatVisible {
		t.Error("H 为空时普通模型应隐藏")
	}

	// 没有 agent：不产生任何判定。
	noAgent := []ModelInfo{{ID: "anything"}}
	whitelist = newModelChatWhitelist(nil, noAgent)
	whitelist.apply(noAgent)

	if noAgent[0].ChatVisible != nil {
		t.Error("无 agent 时 ChatVisible 应保持 nil")
	}
}

// TestChatVisibilityNameMatch 白名单条目可匹配 model.name，而不只是 id。
func TestChatVisibilityNameMatch(t *testing.T) {
	t.Parallel()

	infos := []ModelInfo{{ID: "internal-id", Name: "Display Name"}}
	agent := modelAgent{Name: "cli", Models: []string{"Display Name"}}

	whitelist := newModelChatWhitelist([]modelAgent{agent}, infos)
	whitelist.apply(infos)

	if infos[0].ChatVisible == nil || !*infos[0].ChatVisible {
		t.Fatalf("name 命中应可见：%+v", infos[0])
	}
}

// TestMergeModelInfosKeepsChatVisibility 同 id 合并时主来源显式判定优先；
// 主来源 nil 时采用次来源判定。
//
//nolint:cyclop // 覆盖主来源 nil/显式与 globalCatalog 合并，线性展开更直观。
func TestMergeModelInfosKeepsChatVisibility(t *testing.T) {
	t.Parallel()

	out := mergeModelInfos(
		[]ModelInfo{{ID: "shared"}},
		[]ModelInfo{{ID: "shared", ChatVisible: new(true)}},
	)
	if out[0].ChatVisible == nil || !*out[0].ChatVisible {
		t.Fatalf("主来源 nil 时应采用次来源判定：%+v", out[0])
	}

	out = mergeModelInfos(
		[]ModelInfo{{ID: "shared", ChatVisible: new(false)}},
		[]ModelInfo{{ID: "shared", ChatVisible: new(true)}},
	)
	if out[0].ChatVisible == nil || *out[0].ChatVisible {
		t.Fatalf("主来源显式判定不得被次来源覆盖：%+v", out[0])
	}

	names, infos := mergeGlobalCatalog(
		[]string{"shared"}, []ModelInfo{{ID: "shared"}},
		[]string{"shared", "secondary-only"},
		[]ModelInfo{{ID: "shared", ChatVisible: new(false)}, {ID: "secondary-only", ChatVisible: new(true)}},
	)
	if len(names) != 2 || len(infos) != 2 {
		t.Fatalf("merged names=%v infos=%+v", names, infos)
	}

	if infos[0].ChatVisible == nil || *infos[0].ChatVisible {
		t.Fatalf("shared 应采用次来源 false：%+v", infos[0])
	}

	if infos[1].ChatVisible == nil || !*infos[1].ChatVisible {
		t.Fatalf("secondary-only 应保留自身判定：%+v", infos[1])
	}
}

// TestFetchModelsV3DefaultAgentWhitelist v3 自带 agents 时按默认 agent 裁剪标记；
// H 非空时未命中的 chat 模型也隐藏，custom 始终可见。
func TestFetchModelsV3DefaultAgentWhitelist(t *testing.T) {
	t.Parallel()

	client := testClient(func(r *http.Request) (*http.Response, error) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/v3/config"):
			return jsonResp(200, `{"code":0,"data":{"models":[
				{"id":"hit","name":"Hit","maxOutputTokens":8192},
				{"id":"chat-only","maxOutputTokens":8192,"tags":["chat"]},
				{"id":"custom","maxOutputTokens":8192,"tags":["custom"]},
				{"id":"other","maxOutputTokens":8192}
			],"agents":[
				{"name":"default","models":["hit"],"tags":["default"]},
				{"name":"cli","models":["other"]}
			]}}`), nil
		default:
			return jsonResp(500, `{}`), nil
		}
	})

	infos, err := client.FetchModels(&auth.Auth{AccessToken: "at", UID: "u1"})
	if err != nil {
		t.Fatalf("FetchModels: %v", err)
	}

	assertChatVisibility(t, infos, map[string]bool{
		"hit": true, "chat-only": false, "custom": true, "other": false,
	})
}

// TestFetchModelsEnterpriseWhitelistFallback v3 没有 agents 时，用企业端点默认
// agent 白名单给 v3 独有模型补判定。
func TestFetchModelsEnterpriseWhitelistFallback(t *testing.T) {
	t.Parallel()

	client := testClient(func(r *http.Request) (*http.Response, error) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/v3/config"):
			return jsonResp(200, `{"code":0,"data":{"models":[
				{"id":"v3-only","maxOutputTokens":8192},
				{"id":"shared","maxOutputTokens":8192},
				{"id":"v3-chat","maxOutputTokens":8192,"tags":["chat"]}
			]}}`), nil
		case strings.HasSuffix(r.URL.Path, "/console/enterprises/personal/models"):
			return jsonResp(200, `{"code":0,"data":{"models":[
				{"id":"ent-only","maxOutputTokens":8192},
				{"id":"shared","maxOutputTokens":8192}
			],"agents":[
				{"name":"picked","models":["ent-only","shared"],"tags":["default"]},
				{"name":"cli","models":["ent-only"]}
			]}}`), nil
		default:
			return jsonResp(500, `{}`), nil
		}
	})

	infos, err := client.FetchModels(&auth.Auth{AccessToken: "at", UID: "u1"})
	if err != nil {
		t.Fatalf("FetchModels: %v", err)
	}

	assertChatVisibility(t, infos, map[string]bool{
		"v3-only": false, "shared": true, "v3-chat": false, "ent-only": true,
	})
}

// TestFetchModelsNoAgentsKeepsUndecided 两路都没有 agents（企业 allowFull）时，
// 不产生任何 ChatVisible 判定，/v1/models 输出与旧行为一致。
func TestFetchModelsNoAgentsKeepsUndecided(t *testing.T) {
	t.Parallel()

	client := testClient(func(r *http.Request) (*http.Response, error) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/v3/config"):
			return jsonResp(200, `{"code":0,"data":{"models":[
				{"id":"v3-model","maxOutputTokens":8192}
			]}}`), nil
		case strings.HasSuffix(r.URL.Path, "/v2/enterprises/personal/models"):
			return jsonResp(200, `{"code":0,"data":{"models":[
				{"id":"v3-model","maxOutputTokens":8192},
				{"id":"ent-model","maxOutputTokens":8192}
			]}}`), nil
		default:
			return jsonResp(200, `{"code":1,"msg":"no agent"}`), nil
		}
	})

	infos, err := client.FetchModels(&auth.Auth{AccessToken: "at", UID: "u1"})
	if err != nil {
		t.Fatalf("FetchModels: %v", err)
	}

	for _, info := range infos {
		if info.ChatVisible != nil {
			t.Errorf("%s 不应产生判定：%v", info.ID, *info.ChatVisible)
		}
	}
}

// TestFetchGlobalEnterpriseWhitelistFallback global 企业端点 agents 同样给
// v3 独有模型补判定。
func TestFetchGlobalEnterpriseWhitelistFallback(t *testing.T) {
	t.Parallel()

	client := testClient(func(r *http.Request) (*http.Response, error) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/v3/config"):
			return jsonResp(200, `{"code":0,"data":{"models":[
				{"id":"v3-only","maxOutputTokens":8192},
				{"id":"shared","maxOutputTokens":8192}
			]}}`), nil
		case strings.HasSuffix(r.URL.Path, "/v2/enterprises/personal/models"):
			return jsonResp(200, `{"code":0,"data":{"models":[
				{"id":"ent-only","maxOutputTokens":8192},
				{"id":"shared","maxOutputTokens":8192}
			],"agents":[{"name":"cli","models":["ent-only","shared"]}]}}`), nil
		default:
			return jsonResp(500, `{}`), nil
		}
	})
	client.GlobalEnabled = true
	client.ChatBaseGlobal = "https://global.example"

	account := &auth.Auth{AccessToken: "at", UID: "u-gl", Domain: "www.workbuddy.ai"}

	infos := client.FetchGlobalModelInfos(account)

	assertChatVisibility(t, infos, map[string]bool{
		"v3-only": false, "shared": true, "ent-only": true,
	})
}

// TestFetchGlobalV3AgentWhitelist global v3 自带 agents 时同样生效。
func TestFetchGlobalV3AgentWhitelist(t *testing.T) {
	t.Parallel()

	client := testClient(func(r *http.Request) (*http.Response, error) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/v3/config"):
			return jsonResp(200, `{"code":0,"data":{"models":[
				{"id":"hit","maxOutputTokens":8192},
				{"id":"chat","maxOutputTokens":8192,"tags":["chat"]},
				{"id":"custom","maxOutputTokens":8192,"tags":["custom"]}
			],"agents":[{"name":"default","models":["hit"],"tags":["default"]}]}}`), nil
		default:
			return jsonResp(500, `{}`), nil
		}
	})
	client.GlobalEnabled = true
	client.ChatBaseGlobal = "https://global.example"

	account := &auth.Auth{AccessToken: "at", UID: "u-gl-v3", Domain: "www.workbuddy.ai"}

	infos := client.FetchGlobalModelInfos(account)

	assertChatVisibility(t, infos, map[string]bool{
		"hit": true, "chat": false, "custom": true,
	})
}

// assertChatVisibility 断言目录内每个 id 的 ChatVisible 判定。
func assertChatVisibility(t *testing.T, infos []ModelInfo, want map[string]bool) {
	t.Helper()

	if len(infos) != len(want) {
		t.Fatalf("infos=%+v want ids=%v", infos, want)
	}

	for i := range infos {
		wantVisible, ok := want[infos[i].ID]
		if !ok {
			t.Errorf("unexpected model %q", infos[i].ID)
			continue
		}

		if infos[i].ChatVisible == nil {
			t.Errorf("%s ChatVisible=nil want %v", infos[i].ID, wantVisible)
			continue
		}

		if *infos[i].ChatVisible != wantVisible {
			t.Errorf("%s ChatVisible=%v want %v", infos[i].ID, *infos[i].ChatVisible, wantVisible)
		}
	}
}

// TestModelInfoChatVisibleNotSerialized ChatVisible 是目录内部判定，不得随
// ModelInfo 序列化泄露到任何 JSON 输出。
func TestModelInfoChatVisibleNotSerialized(t *testing.T) {
	t.Parallel()

	raw, err := json.Marshal(ModelInfo{ID: "m", ChatVisible: new(false)}) //nolint:musttag // 内部结构无 JSON 契约，此处只验证字段被排除
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	if strings.Contains(string(raw), "ChatVisible") {
		t.Fatalf("ChatVisible 不应进入 JSON: %s", raw)
	}
}
