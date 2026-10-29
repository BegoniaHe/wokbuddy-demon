package agentcfg

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestApplyBackupRestoreCreated(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	m := New(home, filepath.Join(home, "backups"))
	opts := Options{BaseURL: "http://127.0.0.1:7863", APIKey: "sk-test", Model: "glm-5.2"}

	status, err := m.Apply("claude", opts)
	if err != nil {
		t.Fatal(err)
	}

	if !status.Applied || status.Modified {
		t.Fatalf("status=%+v", status)
	}

	if _, err := os.Stat(filepath.Join(home, ".claude", "settings.json")); err != nil {
		t.Fatal(err)
	}

	if err := m.Restore("claude", false); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(filepath.Join(home, ".claude", "settings.json")); !os.IsNotExist(err) {
		t.Fatal("created file should be removed on restore")
	}
}

func TestRestoreExistingBytes(t *testing.T) {
	t.Parallel()
	home := t.TempDir()

	path := filepath.Join(home, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}

	original := []byte("{\n  \"env\": {\"KEEP\": \"1\"}\n}\n")
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}

	m := New(home, filepath.Join(home, "backups"))
	if _, err := m.Apply("claude", Options{BaseURL: "http://127.0.0.1:7863", APIKey: "sk"}); err != nil {
		t.Fatal(err)
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	if string(after) == string(original) {
		t.Fatal("apply should change file")
	}

	if err := m.Restore("claude", false); err != nil {
		t.Fatal(err)
	}

	restored, _ := os.ReadFile(path)
	if string(restored) != string(original) {
		t.Fatalf("restore mismatch:\n%s", restored)
	}
}

func TestModifiedRequiresForce(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	m := New(home, filepath.Join(home, "backups"))

	opts := Options{BaseURL: "http://127.0.0.1:7863", APIKey: "sk"}
	if _, err := m.Apply("opencode", opts); err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(home, ".config", "opencode", "opencode.json")
	if err := os.WriteFile(path, []byte(`{"manual":true}`), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := m.Apply("opencode", opts); !errors.Is(err, ErrModified) {
		t.Fatalf("apply err=%v want ErrModified", err)
	}

	if err := m.Restore("opencode", false); !errors.Is(err, ErrModified) {
		t.Fatalf("restore err=%v want ErrModified", err)
	}

	if err := m.Restore("opencode", true); err != nil {
		t.Fatal(err)
	}
}

func TestValidateOptionsRejectsMalformedBaseURL(t *testing.T) {
	t.Parallel()

	for _, baseURL := range []string{"", "ftp://example.com", "http://", "http:///path", "not a url"} {
		t.Run(baseURL, func(t *testing.T) {
			t.Parallel()

			opts := Options{BaseURL: baseURL, APIKey: "sk-test", Model: "glm-5.2"}
			if err := validateOptions(&opts); !errors.Is(err, errBaseURLHTTPS) {
				t.Errorf("validateOptions(base_url=%q) err=%v want %v", baseURL, err, errBaseURLHTTPS)
			}
		})
	}
}

func TestValidateOptionsTrimsBaseURL(t *testing.T) {
	t.Parallel()

	opts := Options{BaseURL: "  https://example.com/v1  ", APIKey: "sk-test", Model: "glm-5.2"}
	if err := validateOptions(&opts); err != nil {
		t.Fatal(err)
	}

	if opts.BaseURL != "https://example.com/v1" {
		t.Errorf("BaseURL=%q want trimmed value", opts.BaseURL)
	}
}

func TestApplyCodexKeepsRootKeysOutOfTables(t *testing.T) {
	t.Parallel()
	home := t.TempDir()

	path := filepath.Join(home, ".codex", "config.toml")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}

	original := "model_provider = \"openai\"\nmodel = \"gpt-5\"\n\n[profiles.fast]\nmodel = \"gpt-5-mini\"\n"
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}

	m := New(home, filepath.Join(home, "backups"))

	opts := Options{BaseURL: "http://127.0.0.1:7863", APIKey: "sk-test", Model: "glm-5.2"}
	if _, err := m.Apply("codex", opts); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	assertCodexApplied(t, string(raw))
}

// assertCodexApplied 校验根键位置、/v1 base_url、bearer token 与既有 profile 保留。
func assertCodexApplied(t *testing.T, text string) {
	t.Helper()

	if strings.Count(text, `model_provider =`) != 1 || strings.Count(text, `model =`) != 2 {
		t.Fatalf("unexpected duplicate root keys:\n%s", text)
	}

	firstTable := strings.Index(text, "[")
	if firstTable < 0 || strings.Index(text, `model_provider = "wokbuddy"`) > firstTable {
		t.Fatalf("root keys must precede tables:\n%s", text)
	}

	for _, want := range []string{
		`base_url = "http://127.0.0.1:7863/v1"`,
		`experimental_bearer_token = "sk-test"`,
		`[profiles.fast]`,
		`model = "gpt-5-mini"`,
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q:\n%s", want, text)
		}
	}
}

func TestApplyCodexReplacesPriorBlock(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	m := New(home, filepath.Join(home, "backups"))

	opts := Options{BaseURL: "http://127.0.0.1:7863/v1", APIKey: "sk-one", Model: "m1"}
	if _, err := m.Apply("codex", opts); err != nil {
		t.Fatal(err)
	}

	opts.APIKey, opts.Model = "sk-two", "m2"
	if _, err := m.Apply("codex", opts); err != nil {
		t.Fatal(err)
	}

	raw, _ := os.ReadFile(filepath.Join(home, ".codex", "config.toml"))

	text := string(raw)
	if strings.Count(text, codexStart) != 1 || strings.Contains(text, "sk-one") {
		t.Fatalf("old block not replaced:\n%s", text)
	}

	if !strings.Contains(text, `experimental_bearer_token = "sk-two"`) || strings.Count(text, "model =") != 1 {
		t.Fatalf("new block wrong:\n%s", text)
	}
}

func TestClaudeUsesGatewayRoot(t *testing.T) {
	t.Parallel()
	home := t.TempDir()

	m := New(home, filepath.Join(home, "backups"))
	if _, err := m.Apply("claude", Options{BaseURL: "http://127.0.0.1:7863/v1", APIKey: "sk"}); err != nil {
		t.Fatal(err)
	}

	raw, _ := os.ReadFile(filepath.Join(home, ".claude", "settings.json"))
	if !strings.Contains(string(raw), `"ANTHROPIC_BASE_URL":"http://127.0.0.1:7863"`) &&
		!strings.Contains(string(raw), `"ANTHROPIC_BASE_URL": "http://127.0.0.1:7863"`) {
		t.Fatalf("claude should use root without /v1:\n%s", raw)
	}
}

// TestConcurrentApplyRestore（R2）：并发 Apply/Restore 必须串行化，不能出现
// meta 与配置内容不一致（此前读-改-写无锁，-race 下会丢更新）。
func TestConcurrentApplyRestore(t *testing.T) {
	t.Parallel()

	home := t.TempDir()
	m := New(home, filepath.Join(home, "backups"))
	opts := Options{BaseURL: "http://127.0.0.1:7863", APIKey: "sk-test", Model: "glm-5.2"}

	if _, err := m.Apply("claude", opts); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			_, _ = m.Apply("claude", opts)
		})

		wg.Go(func() {
			_ = m.Restore("claude", true)
		})
	}

	wg.Wait()

	if _, err := m.Apply("claude", opts); err != nil {
		t.Fatalf("apply after concurrent churn: %v", err)
	}

	status := m.Status()
	for _, item := range status {
		if item.ID == "claude" && item.Modified {
			t.Fatalf("meta and config diverged after concurrent writes: %+v", item)
		}
	}
}

// TestApplyNullExistingConfig 顶层 null 会被 json.Unmarshal 置成 nil map；apply 必须
// 按空配置处理，不能写 env/provider 时 panic。
func TestApplyNullExistingConfig(t *testing.T) {
	t.Parallel()

	opts := Options{BaseURL: "http://127.0.0.1:7863", APIKey: "sk-test", Model: "glm-5.2"}

	claudeRaw, err := applyClaude([]byte("null"), opts)
	if err != nil {
		t.Fatalf("applyClaude: %v", err)
	}

	var claude map[string]any
	if uerr := json.Unmarshal(claudeRaw, &claude); uerr != nil {
		t.Fatalf("claude output: %v", uerr)
	}

	env, _ := claude["env"].(map[string]any)
	if env["ANTHROPIC_AUTH_TOKEN"] != "sk-test" {
		t.Fatalf("claude env=%v", env)
	}

	opencodeRaw, err := applyOpenCode([]byte("null"), opts)
	if err != nil {
		t.Fatalf("applyOpenCode: %v", err)
	}

	var opencode map[string]any
	if uerr := json.Unmarshal(opencodeRaw, &opencode); uerr != nil {
		t.Fatalf("opencode output: %v", uerr)
	}

	provider, _ := opencode["provider"].(map[string]any)
	if _, ok := provider["wokbuddy"].(map[string]any); !ok {
		t.Fatalf("opencode provider=%v", provider)
	}
}

// TestApplyCodexKeepsUserRootKeysOutOfBlock 用户根键与注释必须留在托管段之外，
// 否则第二次 Apply 会随托管段一起被删掉（不可幂等）。
func TestApplyCodexKeepsUserRootKeysOutOfBlock(t *testing.T) {
	t.Parallel()
	home := t.TempDir()

	path := filepath.Join(home, ".codex", "config.toml")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}

	original := "# user config\nmodel = \"o3\"\napproval_policy = \"never\"\n\n[profiles.mine]\nmodel = \"o3\"\n"
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}

	m := New(home, filepath.Join(home, "backups"))
	opts := Options{BaseURL: "http://127.0.0.1:7863", APIKey: "sk-test", Model: "glm-5.2"}

	for i := range 2 {
		if _, err := m.Apply("codex", opts); err != nil {
			t.Fatalf("apply #%d: %v", i+1, err)
		}

		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}

		assertCodexUserRootPreserved(t, i+1, string(raw))
	}
}

// assertCodexUserRootPreserved 校验用户根键留在托管段外且未被重复写入。
func assertCodexUserRootPreserved(t *testing.T, round int, text string) {
	t.Helper()

	for _, want := range []string{"# user config", `approval_policy = "never"`, `[profiles.mine]`} {
		if !strings.Contains(text, want) {
			t.Fatalf("apply #%d 丢失 %q:\n%s", round, want, text)
		}
	}

	if strings.Count(text, `approval_policy =`) != 1 || strings.Count(text, codexStart) != 1 {
		t.Fatalf("apply #%d 出现重复/缺失:\n%s", round, text)
	}

	block, ok := codexBlockSpan(text)
	if !ok {
		t.Fatalf("apply #%d 托管段标记缺失:\n%s", round, text)
	}

	if strings.Contains(block, "approval_policy") || strings.Contains(block, "# user config") {
		t.Fatalf("apply #%d 用户根键被写进托管段:\n%s", round, text)
	}

	for _, want := range []string{`base_url = "http://127.0.0.1:7863/v1"`, `experimental_bearer_token = "sk-test"`} {
		if !strings.Contains(text, want) {
			t.Fatalf("apply #%d 托管 provider 表残缺 %q:\n%s", round, want, text)
		}
	}
}

// codexBlockSpan 返回托管段内容（不含标记），标记缺失时返回 false。
func codexBlockSpan(text string) (string, bool) {
	_, after, ok := strings.Cut(text, codexStart)
	if !ok {
		return "", false
	}

	block, _, ok := strings.Cut(after, codexEnd)

	return block, ok
}

// TestApplyCodexSalvagesLegacyBlock 旧格式把用户根键写进了托管段：重建时应先捞出
// 再删除段，避免升级即丢配置。
func TestApplyCodexSalvagesLegacyBlock(t *testing.T) {
	t.Parallel()
	home := t.TempDir()

	path := filepath.Join(home, ".codex", "config.toml")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}

	legacy := codexStart + "\nmodel_provider = \"wokbuddy\"\nmodel = \"m1\"\n\n" +
		"# user config\napproval_policy = \"never\"\n\n" +
		"[model_providers.wokbuddy]\nname = \"WokBuddy Demon\"\n" + codexEnd + "\n"
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}

	m := New(home, filepath.Join(home, "backups"))
	opts := Options{BaseURL: "http://127.0.0.1:7863", APIKey: "sk-test", Model: "glm-5.2"}

	if _, err := m.Apply("codex", opts); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	text := string(raw)
	for _, want := range []string{"# user config", `approval_policy = "never"`} {
		if !strings.Contains(text, want) {
			t.Fatalf("旧格式用户根键未保留 %q:\n%s", want, text)
		}
	}

	if strings.Count(text, "model =") != 1 || !strings.Contains(text, `experimental_bearer_token = "sk-test"`) {
		t.Fatalf("旧格式重建结果异常:\n%s", text)
	}
}

// TestValidateOptionsRejectsControlCharsInAPIKey api_key 与 model 一样经 Go 的 %q
// 写进 Codex 的 config.toml：0x01 会写成 TOML 不认的 `\x01` 转义，整份
// config.toml 解析失败（Codex 是 Rust toml crate，只认 \uXXXX 与短转义）。
// 单模型校验补上后，同一处遗漏仍留在 api_key 上。
func TestValidateOptionsRejectsControlCharsInAPIKey(t *testing.T) {
	t.Parallel()

	// 只测 trim 之后仍残留的控制字符：首尾空白（含 \n / \r / U+0085）由 trim
	// 正常吸收（见下一条用例），内嵌的 \t / \x01 / DEL / C1 会原样落进 config.toml。
	for _, bad := range []string{"sk\x01bad", "sk\tbad", "sk\x7f", "sk\u0085bad"} {
		opts := Options{BaseURL: "http://127.0.0.1:7863", APIKey: bad, Model: "glm-5.2"}
		if err := validateOptions(&opts); !errors.Is(err, errAPIKey) {
			t.Fatalf("api_key %q 应被拒绝，得到 %v", bad, err)
		}
	}
}

// TestApplyCodexKeySurvivesTOMLRoundTrip 端到端：正常密钥写出的 config.toml
// 不含 TOML 不认的转义；粘贴带首尾空白的密钥落盘前被 trim（否则请求恒 401，
// 却表现为「配置写入成功」）。
func TestApplyCodexKeySurvivesTOMLRoundTrip(t *testing.T) {
	t.Parallel()

	home := t.TempDir()
	m := New(home, filepath.Join(home, "backups"))

	opts := Options{BaseURL: "http://127.0.0.1:7863", APIKey: "  sk-pasted  ", Model: "glm-5.2"}
	if _, err := m.Apply("codex", opts); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(filepath.Join(home, ".codex", "config.toml"))
	if err != nil {
		t.Fatal(err)
	}

	text := string(raw)
	if !strings.Contains(text, `experimental_bearer_token = "sk-pasted"`) {
		t.Fatalf("api_key 未 trim 或写出形态异常:\n%s", text)
	}
	// Go 的 %q 只会对这种字符写 \xNN，而 toml crate 不认该转义。
	if strings.Contains(text, `\x`) {
		t.Fatalf("config.toml 含 TOML 不认的 \\xNN 转义:\n%s", text)
	}
}
