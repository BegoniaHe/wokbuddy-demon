package prompt

import (
	"encoding/json"
	"strings"
	"testing"
)

func firstRole(t *testing.T, body []byte) (string, string) {
	t.Helper()

	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		t.Fatalf("unmarshal: %v (%s)", err, body)
	}

	msgs, _ := obj["messages"].([]any)
	if len(msgs) == 0 {
		t.Fatal("messages empty")
	}

	first, _ := msgs[0].(map[string]any)
	role, _ := first["role"].(string)
	content, _ := first["content"].(string)

	return role, content
}

func TestEnsureSystemFirstPrepends(t *testing.T) {
	t.Parallel()

	out := EnsureSystemFirst([]byte(`{"messages":[{"role":"user","content":"hi"}]}`), "GW")

	role, content := firstRole(t, out)
	if role != "system" || content != "GW" {
		t.Fatalf("first=%s/%s want system/GW", role, content)
	}

	if !strings.Contains(string(out), `"role":"user"`) {
		t.Fatalf("original message lost: %s", out)
	}
}

func TestEnsureSystemFirstKeepsSystem(t *testing.T) {
	t.Parallel()

	in := []byte(`{"messages":[{"role":"system","content":"S"},{"role":"user","content":"hi"}]}`)

	out := EnsureSystemFirst(in, "GW")
	if string(out) != string(in) {
		t.Fatalf("system-first body changed: %s", out)
	}
}

func TestEnsureSystemFirstConvertsDeveloper(t *testing.T) {
	t.Parallel()

	out := EnsureSystemFirst([]byte(`{"messages":[{"role":"developer","content":"D"}]}`), "GW")

	role, content := firstRole(t, out)
	if role != "system" || content != "D" {
		t.Fatalf("first=%s/%s want system/D", role, content)
	}
}

func TestEnsureSystemFirstFallbackDegraded(t *testing.T) {
	t.Parallel()

	out := EnsureSystemFirst([]byte(`{"messages":[{"role":"user","content":"hi"}]}`), "")
	if role, content := firstRole(t, out); role != "system" || content != Degraded {
		t.Fatalf("first=%s/%s want system/Degraded", role, content)
	}
}
