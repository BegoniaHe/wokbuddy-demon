package apikey

import (
	"testing"
	"time"
)

func newLimited(t *testing.T, lim Limits) (*Store, string) {
	t.Helper()
	s := New("")
	rec, _, err := s.Create("k")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if ok, err := s.UpdateLimits(rec.ID, lim); err != nil || !ok {
		t.Fatalf("update limits: %v %v", ok, err)
	}
	return s, rec.ID
}

func TestAcquireRPM(t *testing.T) {
	s, id := newLimited(t, Limits{RPM: 2})
	for i := range 2 {
		rel, _, ok := s.Acquire(id)
		if !ok {
			t.Fatalf("request %d 应放行", i)
		}
		rel()
	}
	rel, retry, ok := s.Acquire(id)
	if ok || rel != nil {
		t.Fatal("超过 RPM 应拒绝")
	}
	if retry <= 0 || retry > time.Minute {
		t.Fatalf("retryAfter=%v", retry)
	}
}

func TestAcquireConcurrency(t *testing.T) {
	s, id := newLimited(t, Limits{MaxConcurrent: 1})
	rel, _, ok := s.Acquire(id)
	if !ok {
		t.Fatal("首个请求应放行")
	}
	if _, _, ok := s.Acquire(id); ok {
		t.Fatal("并发超限应拒绝")
	}
	rel()
	if rel2, _, ok := s.Acquire(id); !ok {
		t.Fatal("释放后应放行")
	} else {
		rel2()
	}
}

func TestAcquireTPM(t *testing.T) {
	s, id := newLimited(t, Limits{TPM: 100})
	s.RecordUsage(id, 0, 60, 60)
	if _, _, ok := s.Acquire(id); !ok {
		t.Fatal("未达 TPM 应放行")
	}
	s.RecordUsage(id, 0, 50, 50)
	if _, _, ok := s.Acquire(id); ok {
		t.Fatal("超过 TPM 应拒绝")
	}
}

func TestModelAllowed(t *testing.T) {
	s, id := newLimited(t, Limits{Models: []string{"cn:glm-*", "gpt-5*"}})
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
		if got := s.ModelAllowed(id, tc.model); got != tc.want {
			t.Errorf("ModelAllowed(%q)=%v want %v", tc.model, got, tc.want)
		}
	}
}

func TestUnlimitedKeyAllowsEverything(t *testing.T) {
	s := New("")
	rec, _, _ := s.Create("k")
	for range 100 {
		rel, _, ok := s.Acquire(rec.ID)
		if !ok {
			t.Fatal("未配置限制应始终放行")
		}
		rel()
	}
	if !s.ModelAllowed(rec.ID, "any:model") {
		t.Fatal("未配置白名单应允许所有模型")
	}
}
