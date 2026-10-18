package apikey

import (
	"testing"
	"time"
)

func newLimited(t *testing.T, lim Limits) (*Store, string) {
	t.Helper()

	store := New("")

	rec, _, err := store.Create("k")
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	if ok, err := store.UpdateLimits(rec.ID, lim); err != nil || !ok {
		t.Fatalf("update limits: %v %v", ok, err)
	}

	return store, rec.ID
}

func TestAcquireRPM(t *testing.T) {
	t.Parallel()

	store, id := newLimited(t, Limits{RPM: 2, TPM: 0, MaxConcurrent: 0, Models: nil})
	for i := range 2 {
		rel, _, ok := store.Acquire(id)
		if !ok {
			t.Fatalf("request %d 应放行", i)
		}

		rel()
	}

	rel, retry, ok := store.Acquire(id)
	if ok || rel != nil {
		t.Fatal("超过 RPM 应拒绝")
	}

	if retry <= 0 || retry > time.Minute {
		t.Fatalf("retryAfter=%v", retry)
	}
}

func TestAcquireConcurrency(t *testing.T) {
	t.Parallel()
	store, id := newLimited(t, Limits{MaxConcurrent: 1, RPM: 0, TPM: 0, Models: nil})

	rel, _, ok := store.Acquire(id)
	if !ok {
		t.Fatal("首个请求应放行")
	}

	if _, _, ok := store.Acquire(id); ok {
		t.Fatal("并发超限应拒绝")
	}

	rel()

	if rel2, _, ok := store.Acquire(id); !ok {
		t.Fatal("释放后应放行")
	} else {
		rel2()
	}
}

func TestAcquireTPM(t *testing.T) {
	t.Parallel()
	store, id := newLimited(t, Limits{TPM: 100, RPM: 0, MaxConcurrent: 0, Models: nil})
	store.RecordUsage(id, 0, 60, 60)

	if _, _, ok := store.Acquire(id); !ok {
		t.Fatal("未达 TPM 应放行")
	}

	store.RecordUsage(id, 0, 50, 50)

	if _, _, ok := store.Acquire(id); ok {
		t.Fatal("超过 TPM 应拒绝")
	}
}

func TestModelAllowed(t *testing.T) {
	t.Parallel()
	store, id := newLimited(t, Limits{Models: []string{"cn:glm-*", "gpt-5*"}, RPM: 0, TPM: 0, MaxConcurrent: 0})

	cases := []struct {
		model string
		want  bool
	}{
		{"cn:glm-5.2", true},
		{"glm-5.2", true}, // 无前缀按 cn
		{"global:glm-5.2", false},
		{"gpt-5-codex", true},
		{"global:gpt-5-codex", true}, // 无 realm 模式匹配任意域
		{"claude-x", false},
	}
	for _, tc := range cases {
		if got := store.ModelAllowed(id, tc.model); got != tc.want {
			t.Errorf("ModelAllowed(%q)=%v want %v", tc.model, got, tc.want)
		}
	}
}

func TestUnlimitedKeyAllowsEverything(t *testing.T) {
	t.Parallel()

	store := New("")

	rec, _, _ := store.Create("k")
	for range 100 {
		rel, _, ok := store.Acquire(rec.ID)
		if !ok {
			t.Fatal("未配置限制应始终放行")
		}

		rel()
	}

	if !store.ModelAllowed(rec.ID, "any:model") {
		t.Fatal("未配置白名单应允许所有模型")
	}
}
