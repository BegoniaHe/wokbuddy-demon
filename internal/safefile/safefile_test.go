package safefile

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

func TestWriteKeepAndReadBackup(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "state.json")
	if err := WriteKeep(path, []byte(`{"v":1}`), 0o600, nil); err != nil {
		t.Fatal(err)
	}

	if err := WriteKeep(path, []byte(`{"v":2}`), 0o600, nil); err != nil {
		t.Fatal(err)
	}

	got, err := Read(path, nil)
	if err != nil || string(got) != `{"v":2}` {
		t.Fatalf("read=%s err=%v", got, err)
	}

	bak, err := os.ReadFile(path + ".bak")
	if err != nil || string(bak) != `{"v":1}` {
		t.Fatalf("bak=%s err=%v", bak, err)
	}

	// 正文损坏 → 回退 .bak。
	if writeErr := os.WriteFile(path, []byte(`{"v":`), 0o600); writeErr != nil {
		t.Fatal(writeErr)
	}

	got, err = Read(path, nil)
	if err != nil || string(got) != `{"v":1}` {
		t.Fatalf("fallback=%s err=%v", got, err)
	}
}

func TestWriteKeepDoesNotPoisonBackup(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte(`{"v":1}`), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := WriteKeep(path, []byte(`{"v":2}`), 0o600, nil); err != nil {
		t.Fatal(err)
	}

	// 正文坏了，但 .bak 仍是有效的 v1；再次 WriteKeep 不得用坏内容覆盖 .bak。
	if err := os.WriteFile(path, []byte("bad"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := WriteKeep(path, []byte(`{"v":3}`), 0o600, nil); err != nil {
		t.Fatal(err)
	}

	bak, err := os.ReadFile(path + ".bak")
	if err != nil || string(bak) != `{"v":1}` {
		t.Fatalf("bak=%s err=%v want v1", bak, err)
	}
}

// TestWriteKeepUsesCallerValidator 正文是合法 JSON 但通不过调用方语义校验时，
// 不得用它覆盖 .bak（auth 的有效性判定比 json.Valid 更严）。
func TestWriteKeepUsesCallerValidator(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "auth.json")
	if err := os.WriteFile(path+".bak", []byte(`{"v":1}`), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(path, []byte(`{"other":1}`), 0o600); err != nil {
		t.Fatal(err)
	}

	valid := func(b []byte) bool { return bytes.Contains(b, []byte(`"v"`)) }
	if err := WriteKeep(path, []byte(`{"v":2}`), 0o600, valid); err != nil {
		t.Fatal(err)
	}

	bak, err := os.ReadFile(path + ".bak")
	if err != nil || string(bak) != `{"v":1}` {
		t.Fatalf("bak=%s err=%v want v1", bak, err)
	}

	got, err := Read(path, valid)
	if err != nil || string(got) != `{"v":2}` {
		t.Fatalf("main=%s err=%v want v2", got, err)
	}
}

func TestReadMissing(t *testing.T) {
	t.Parallel()

	_, err := Read(filepath.Join(t.TempDir(), "missing.json"), nil)
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("err=%v want fs.ErrNotExist", err)
	}
}

func TestReadBothCorrupt(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte("bad"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(path+".bak", []byte("also bad"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := Read(path, nil); err == nil {
		t.Fatal("expected error when both files are corrupt")
	}
}
