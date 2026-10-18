package agentcfg

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
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
