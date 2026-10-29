package agentcfg

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// applyBytes 直接跑单家客户端的 apply，便于断言生成文本（不经 Manager 的备份逻辑）。
func applyBytes(t *testing.T, id string, existing []byte, opts Options) []byte {
	t.Helper()

	if err := validateOptions(&opts); err != nil {
		t.Fatalf("validateOptions: %v", err)
	}

	c, ok := (&Manager{}).client(id)
	if !ok {
		t.Fatalf("unknown client %q", id)
	}

	out, err := c.apply(existing, opts)
	if err != nil {
		t.Fatalf("apply %s: %v", id, err)
	}

	return out
}

func TestModelListEmptyKeepsSingleModel(t *testing.T) {
	t.Parallel()

	opts := Options{BaseURL: "http://127.0.0.1:7863", APIKey: "sk", Model: "glm-5.2"}
	if err := validateOptions(&opts); err != nil {
		t.Fatal(err)
	}

	if got := modelList(opts); len(got) != 1 || got[0] != "glm-5.2" {
		t.Fatalf("modelList = %v, want [glm-5.2]", got)
	}

	// Models 为空时单模型输出与改动前一致：OpenCode 只含一个 model 键。
	out := applyBytes(t, "opencode", nil, opts)

	var doc struct {
		Provider map[string]struct {
			Models map[string]any `json:"models"`
		} `json:"provider"`
	}

	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}

	if len(doc.Provider["wokbuddy"].Models) != 1 {
		t.Fatalf("models = %v, want 仅 1 个", doc.Provider["wokbuddy"].Models)
	}
}

func TestValidateOptionsNormalizesModels(t *testing.T) {
	t.Parallel()

	opts := Options{
		BaseURL: "http://127.0.0.1:7863",
		APIKey:  "sk",
		Models:  []string{" cn:glm-5.3 ", "", "cn:glm-5.3", "global:kimi-k3", "   "},
	}
	if err := validateOptions(&opts); err != nil {
		t.Fatal(err)
	}

	if want := []string{"cn:glm-5.3", "global:kimi-k3"}; strings.Join(opts.Models, ",") != strings.Join(want, ",") {
		t.Fatalf("Models = %v, want %v（去重保序 + 丢空）", opts.Models, want)
	}

	if opts.Model != "cn:glm-5.3" {
		t.Fatalf("Model = %q, want 列表第一项", opts.Model)
	}
}

func TestValidateOptionsTruncatesModelList(t *testing.T) {
	t.Parallel()

	in := make([]string, 0, 250)
	for i := range 250 {
		in = append(in, "cn:m-"+strconv.Itoa(i))
	}

	opts := Options{BaseURL: "http://127.0.0.1:7863", APIKey: "sk", Models: in}
	if err := validateOptions(&opts); err != nil {
		t.Fatal(err)
	}

	if len(opts.Models) != maxModelList {
		t.Fatalf("len(Models) = %d, want %d（超出截断不报错）", len(opts.Models), maxModelList)
	}
}

func TestValidateOptionsRejectsControlCharsInModels(t *testing.T) {
	t.Parallel()

	for _, bad := range []string{"cn:a\nb", "cn:a\rb", "cn:a\tb"} {
		opts := Options{BaseURL: "http://127.0.0.1:7863", APIKey: "sk", Models: []string{bad}}
		if err := validateOptions(&opts); !errors.Is(err, errModelList) {
			t.Fatalf("模型 id %q 应被拒绝，得到 %v", bad, err)
		}
	}
}

// codexConfigPaths 断言 codex 配置里各模型的出现形态。
func codexConfigPaths(t *testing.T, text string, models []string) {
	t.Helper()

	if !strings.Contains(text, `model = "`+models[0]+`"`) {
		t.Fatalf("根段默认模型 %q 缺失：\n%s", models[0], text)
	}

	for _, id := range models[1:] {
		for _, want := range []string{`[profiles."` + id + `"]`, `model = "` + id + `"`, `model_provider = "wokbuddy"`} {
			if !strings.Contains(text, want) {
				t.Fatalf("缺少 %q：\n%s", want, text)
			}
		}
	}

	// 默认模型不重复生成 profile（它在根段）；去重后 profile 段数 = 模型数-1。
	if n := strings.Count(text, "[profiles."); n != len(models)-1 {
		t.Fatalf("profiles 段数 = %d, want %d", n, len(models)-1)
	}
}

func TestApplyCodexWritesProfiles(t *testing.T) {
	t.Parallel()

	home := t.TempDir()
	m := New(home, filepath.Join(home, "backups"))
	opts := Options{
		BaseURL: "http://127.0.0.1:7863",
		APIKey:  "sk-test",
		Models:  []string{"cn:glm-5.3", "global:kimi-k3", "cn:glm-5.3"},
	}

	path := filepath.Join(home, ".codex", "config.toml")

	if _, err := m.Apply("codex", opts); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// 去重后的模型序列：默认 + 1 个 bonus profile。
	codexConfigPaths(t, string(raw), []string{"cn:glm-5.3", "global:kimi-k3"})

	// 幂等：重复 Apply 不累加托管段。
	if _, applyErr := m.Apply("codex", opts); applyErr != nil {
		t.Fatal(applyErr)
	}

	again, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	if n := strings.Count(string(again), codexStart); n != 1 {
		t.Fatalf("托管段起始标记出现 %d 次，want 1", n)
	}

	codexConfigPaths(t, string(again), []string{"cn:glm-5.3", "global:kimi-k3"})
}

func TestApplyCodexKeepsUserProfiles(t *testing.T) {
	t.Parallel()

	existing := []byte("model = \"old\"\n\n[profiles.mine]\nmodel = \"user-model\"\n")
	opts := Options{
		BaseURL: "http://127.0.0.1:7863",
		APIKey:  "sk-test",
		Models:  []string{"cn:glm-5.3", "cn:glm-5.4"},
	}

	out := string(applyBytes(t, "codex", existing, opts))
	if !strings.Contains(out, "[profiles.mine]") || !strings.Contains(out, "model = \"user-model\"") {
		t.Fatalf("用户自有 profile 被破坏：\n%s", out)
	}

	if !strings.Contains(out, `[profiles."cn:glm-5.4"]`) {
		t.Fatalf("缺少新 profile：\n%s", out)
	}
}

// TestApplyCodexReplacesSameNameUserProfile 用户自有 profile 与托管 profile 同名时
// 必须让位：同名 `[profiles.<id>]` 重复声明会让整份 config.toml（TOML）解析失败，
// Codex 直接加载不了配置。
func TestApplyCodexReplacesSameNameUserProfile(t *testing.T) {
	t.Parallel()

	existing := []byte("model = \"old\"\n\n[profiles.\"global:kimi-k3\"]\nmodel = \"user-thing\"\n\n[profiles.mine]\nmodel = \"keep-me\"\n")
	opts := Options{
		BaseURL: "http://127.0.0.1:7863",
		APIKey:  "sk-test",
		Models:  []string{"cn:glm-5.3", "global:kimi-k3"},
	}

	out := string(applyBytes(t, "codex", existing, opts))

	if n := strings.Count(out, `[profiles."global:kimi-k3"]`); n != 1 {
		t.Fatalf("同名 profile 声明 %d 次（TOML 重复声明会整份失效）：\n%s", n, out)
	}

	if strings.Contains(out, "user-thing") {
		t.Fatalf("同名用户 profile 的键行应被让出：\n%s", out)
	}

	if !strings.Contains(out, `[profiles.mine]`) || !strings.Contains(out, `model = "keep-me"`) {
		t.Fatalf("无关用户 profile 被破坏：\n%s", out)
	}
}

func TestApplyOpenCodeWritesAllModels(t *testing.T) {
	t.Parallel()

	opts := Options{
		BaseURL: "http://127.0.0.1:7863",
		APIKey:  "sk-test",
		Models:  []string{"cn:glm-5.3", "global:kimi-k3"},
	}

	out := applyBytes(t, "opencode", nil, opts)

	var doc struct {
		Provider map[string]struct {
			Models  map[string]any `json:"models"`
			Options struct {
				BaseURL string `json:"baseURL"`
			} `json:"options"`
		} `json:"provider"`
	}

	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatalf("输出不是合法 JSON: %v", err)
	}

	models := doc.Provider["wokbuddy"].Models
	if len(models) != 2 {
		t.Fatalf("models = %v, want 全部 2 个 id", models)
	}

	for _, id := range opts.Models {
		if _, ok := models[id]; !ok {
			t.Fatalf("缺少模型 %q：%v", id, models)
		}
	}

	if doc.Provider["wokbuddy"].Options.BaseURL != "http://127.0.0.1:7863/v1" {
		t.Fatalf("baseURL = %q", doc.Provider["wokbuddy"].Options.BaseURL)
	}
}

func TestApplyClaudeUsesFirstModel(t *testing.T) {
	t.Parallel()

	opts := Options{
		BaseURL: "http://127.0.0.1:7863/v1",
		APIKey:  "sk-test",
		Models:  []string{"cn:glm-5.3", "global:kimi-k3"},
	}

	out := applyBytes(t, "claude", nil, opts)

	var doc struct {
		Env map[string]string `json:"env"`
	}

	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}

	if doc.Env["ANTHROPIC_MODEL"] != "cn:glm-5.3" {
		t.Fatalf("ANTHROPIC_MODEL = %q, want 列表第一项", doc.Env["ANTHROPIC_MODEL"])
	}
	// Claude Code 只支持单模型：其余 id 不写进 settings.json。
	if strings.Contains(string(out), "kimi-k3") {
		t.Fatalf("Claude 配置不应包含非默认模型：\n%s", out)
	}

	if doc.Env["ANTHROPIC_BASE_URL"] != "http://127.0.0.1:7863" {
		t.Fatalf("ANTHROPIC_BASE_URL = %q，必须是不带 /v1 的根地址", doc.Env["ANTHROPIC_BASE_URL"])
	}
}

// TestNormalizeModelListRejectsControlRunes 控制字符必须整条拒绝：Codex 配置走
// Go 的 %q 输出，`\x01` 这种转义 TOML 不认，会连累整份 config.toml 解析失败。
func TestNormalizeModelListRejectsControlRunes(t *testing.T) {
	t.Parallel()

	for _, id := range []string{"cn:a\x01b", "cn:a\nb", "cn:a\tb", "cn:a\x7fb"} {
		if _, err := normalizeModelList([]string{"cn:ok", id}); err == nil {
			t.Fatalf("模型 id %q 含控制字符却未被拒绝", id)
		}
	}

	// 正常 id（含 : . / - 与中文）照旧放行。
	got, err := normalizeModelList([]string{"cn:glm-5.2", " global:kimi-k3 ", "cn:模型/v1"})
	if err != nil {
		t.Fatal(err)
	}

	if len(got) != 3 {
		t.Fatalf("合法 id 被拒：%v", got)
	}
}
