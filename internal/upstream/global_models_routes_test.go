package upstream

import (
	"errors"
	"testing"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
)

var errV3ConfigStatus500 = errors.New("v3/config status 500")

// TestMergeV3RoutesPriority 主路字段权威：同 id 在三路都出现时，字段取自主路
// （桌面端 UA），后续路不得覆盖其窗口/effort/credits。
func TestMergeV3RoutesPriority(t *testing.T) {
	t.Parallel()

	callback := func(id string, ctx, out int64, credits string, efforts []string) ModelInfo {
		return ModelInfo{ID: id, ContextWindow: ctx, MaxTokens: out, Credits: credits, Efforts: efforts}
	}
	desktop := probeResult{
		names: []string{"shared", "only-desktop"},
		infos: []ModelInfo{
			callback("shared", 1000000, 128000, "x1.33", []string{"low", "high"}),
			callback("only-desktop", 500000, 64000, "x0.10", []string{"medium"}),
		},
	}
	ide := probeResult{
		names: []string{"shared", "only-ide"},
		infos: []ModelInfo{
			// 同 id 但字段不同：不得覆盖主路。
			callback("shared", 176000, 24000, "x9.99", []string{"max"}),
			callback("only-ide", 200000, 32000, "x0.20", nil),
		},
	}
	cli := probeResult{
		names: []string{"shared", "only-cli"},
		infos: []ModelInfo{
			callback("shared", 272000, 72000, "x8.88", []string{"xhigh"}),
			callback("only-cli", 256000, 32000, "x0.30", nil),
		},
	}

	got := mergeV3Routes([]struct {
		label string
		res   probeResult
	}{
		{"desktop-UA", desktop},
		{"IDE-UA", ide},
		{"CLI-UA", cli},
	})
	if got.err != nil {
		t.Fatalf("unexpected err: %v", got.err)
	}

	assertV3Names(t, got.names, []string{"shared", "only-desktop", "only-ide", "only-cli"})
	// 主路字段权威：shared 的窗口必须是主路值。
	assertSharedFromPrimary(t, got.infos)
}

// assertV3Names 断言合并后的 names 顺序与期望一致。
func assertV3Names(t *testing.T, got, want []string) {
	t.Helper()

	if len(got) != len(want) {
		t.Fatalf("names = %v, want %v", got, want)
	}

	for i, id := range want {
		if got[i] != id {
			t.Errorf("names[%d] = %q, want %q (full %v)", i, got[i], id, got)
		}
	}
}

// assertSharedFromPrimary 断言同 id "shared" 的字段取自主路（桌面端）。
func assertSharedFromPrimary(t *testing.T, infos []ModelInfo) {
	t.Helper()

	var shared *ModelInfo

	for i := range infos {
		if infos[i].ID == "shared" {
			shared = &infos[i]
		}
	}

	if shared == nil {
		t.Fatal("shared model missing from infos")
	}

	if shared.ContextWindow != 1000000 || shared.MaxTokens != 128000 {
		t.Errorf("shared window = %d/%d, want 1000000/128000 (primary route must win)", shared.ContextWindow, shared.MaxTokens)
	}

	if shared.Credits != "x1.33" {
		t.Errorf("shared credits = %q, want x1.33", shared.Credits)
	}

	if len(shared.Efforts) != 2 || shared.Efforts[0] != "low" {
		t.Errorf("shared efforts = %v, want [low high]", shared.Efforts)
	}
}

// TestMergeV3RoutesPartialFailure 单路失败不拖垮整次探测：失败的路由被跳过，
// 成功路并集照常产出；全路失败才返回 err。
func TestMergeV3RoutesPartialFailure(t *testing.T) {
	t.Parallel()

	mk := func(id string) ModelInfo { return ModelInfo{ID: id} }
	ok := probeResult{names: []string{"a"}, infos: []ModelInfo{mk("a")}}
	bad := probeResult{err: errV3ConfigStatus500}

	got := mergeV3Routes([]struct {
		label string
		res   probeResult
	}{
		{"desktop-UA", bad},
		{"IDE-UA", ok},
		{"CLI-UA", bad},
	})
	if got.err != nil {
		t.Fatalf("partial failure should not error, got %v", got.err)
	}

	if len(got.names) != 1 || got.names[0] != "a" {
		t.Errorf("names = %v, want [a]", got.names)
	}

	// 全路失败 → err（取首路错误，供调用方降级到企业端点）。
	all := mergeV3Routes([]struct {
		label string
		res   probeResult
	}{
		{"desktop-UA", bad},
		{"IDE-UA", bad},
		{"CLI-UA", bad},
	})
	if all.err == nil {
		t.Fatal("all routes failed: want err, got nil")
	}
}

// TestNonChatModelGenerationTags 生成类（图片/视频）模型不得进对话目录。
// 回归用例：nonChatModel 早期只拦 text-to-image，桌面端目录下的
// text-to-video / image-to-video（seedance 系列）会漏过并混进 /v1/models。
func TestNonChatModelGenerationTags(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		id   string
		out  int64
		tags []string
		want bool
	}{
		{"图片生成（既有规则）", "hunyuan-image-alpha", 0, []string{"text-to-image"}, true},
		{"图生图", "gpt-image-2.5-sunburst", 0, []string{"text-to-image", "image-to-image"}, true},
		{"文生视频（本次修复）", "seedance-2.5", 0, []string{"text-to-video", "image-to-video"}, true},
		{"图生视频（本次修复）", "seedance-2.5-pro", 1024, []string{"image-to-video"}, true},
		{"普通对话模型", "gpt-6-sol", 128000, nil, false},
		{"新模型无 tags", "grok-4.7", 128000, []string{}, false},
		{"对话模型带无关 tag", "balanced-model", 32000, []string{"craft"}, false},
		{"tiny 输出仍剔除", "completion-1.0", 256, nil, true},
		{"nes 前缀仍剔除", "nes-1.2", 8192, nil, true},
	}
	for _, c := range cases {
		if got := nonChatModel(c.id, c.out, c.tags); got != c.want {
			t.Errorf("%s: nonChatModel(%q, %d, %v) = %v, want %v", c.name, c.id, c.out, c.tags, got, c.want)
		}
	}
}

// TestDesktopUAForGlobalAccount 桌面端 UA 第二段按官方分发包恒为 `WorkBuddy`
// （CN 5.8.2 / global 5.7.6 的 daemon UA 字面量均如此），无 realm 品牌差异；
// 仅版本号按 realm 分档。探测主路用这个 UA，与 chat 路径同源。
func TestDesktopUAForGlobalAccount(t *testing.T) {
	t.Parallel()

	client := New()
	global := &auth.Auth{UID: "u1", Domain: "www.workbuddy.ai"}
	global.BackfillRealm()

	str := client.defaultWorkBuddyUAFor(global)
	if want := "WorkBuddy/5.7.6"; !contains(str, want) {
		t.Errorf("global desktop UA = %q, want it to contain %q", str, want)
	}

	if !contains(str, "CLI/2.156.0") {
		t.Errorf("global desktop UA = %q, want CLI/2.156.0 segment", str)
	}

	if contains(str, "WorkBuddy AI") {
		t.Errorf("global desktop UA = %q, must not contain 'WorkBuddy AI' (两份官方包均无)", str)
	}
	// CN 账号版本分档为 5.8.2。
	cn := &auth.Auth{UID: "u2", Domain: "copilot.tencent.com"}
	cn.BackfillRealm()

	if ua := client.defaultWorkBuddyUAFor(cn); !contains(ua, "WorkBuddy/5.8.2") {
		t.Errorf("cn desktop UA = %q, want WorkBuddy/5.8.2", ua)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}

	return false
}
