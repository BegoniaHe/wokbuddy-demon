package agentcfg

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestApplyBackupRestoreCreated(t *testing.T) {
	home := t.TempDir()
	m := New(home, filepath.Join(home, "backups"))
	opts := Options{BaseURL: "http://127.0.0.1:7863", APIKey: "sk-test", Model: "glm-5.2"}
	st, err := m.Apply("claude", opts)
	if err != nil {
		t.Fatal(err)
	}
	if !st.Applied || st.Modified {
		t.Fatalf("status=%+v", st)
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
