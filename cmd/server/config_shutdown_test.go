package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/BegoniaHe/wokbuddy-demon/internal/livecfg"
	"github.com/BegoniaHe/wokbuddy-demon/internal/pool"
	"github.com/BegoniaHe/wokbuddy-demon/internal/scheduler"
	"github.com/BegoniaHe/wokbuddy-demon/internal/upstream"
)

func TestShutdownConfigDefaultsAndNormalize(t *testing.T) {
	t.Parallel()

	config := Default()
	if config.Server.RequestAcceptGraceSeconds != 0 || config.Server.ShutdownGraceSeconds != 30 {
		t.Fatalf("defaults grace=%d shutdown=%d want 0/30",
			config.Server.RequestAcceptGraceSeconds, config.Server.ShutdownGraceSeconds)
	}

	config = Default()

	config.Server.ShutdownGraceSeconds = 0
	if err := config.normalize(); err != nil {
		t.Fatal(err)
	}

	if config.Server.ShutdownGraceSeconds != 30 {
		t.Fatalf("shutdown grace=%d want 30 after normalize", config.Server.ShutdownGraceSeconds)
	}

	config = Default()

	config.Server.RequestAcceptGraceSeconds = -1
	if err := config.normalize(); err == nil {
		t.Fatal("negative request_accept_grace_seconds should fail")
	}

	config = Default()

	config.Server.ShutdownGraceSeconds = -1
	if err := config.normalize(); err == nil {
		t.Fatal("negative shutdown_grace_seconds should fail")
	}
}

func TestRestartRequiredIncludesShutdownGrace(t *testing.T) {
	t.Parallel()

	cases := []struct {
		field  string
		mutate func(*Config)
	}{
		{"server.request_accept_grace_seconds", func(c *Config) { c.Server.RequestAcceptGraceSeconds = 5 }},
		{"server.shutdown_grace_seconds", func(c *Config) { c.Server.ShutdownGraceSeconds = 10 }},
	}
	for _, testCase := range cases {
		oldCfg := Default()
		newCfg := Default()
		testCase.mutate(newCfg)
		got := restartRequiredFields(oldCfg, newCfg)
		found := false

		for _, f := range got {
			if f == testCase.field {
				found = true
			}
		}

		if !found {
			t.Errorf("restart fields %v missing %q", got, testCase.field)
		}
	}
}

func TestSaveConfigBackupAndRestore(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"api_key":"old-key","listen":":7863"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	state := pool.New("")
	client := upstream.New()
	sch := scheduler.New(&scheduler.Config{})
	live := livecfg.New(livecfg.Snapshot{APIKey: "old-key"})

	mustSaveConfig(t, path, live, state, client, sch)
	assertFileContains(t, configBackupPath(path), `"old-key"`)
	assertLiveKey(t, live, "new-key")

	mustRestoreConfig(t, path, live, state, client, sch)
	assertFileContains(t, path, `"old-key"`)
	assertLiveKey(t, live, "old-key")
	// 恢复不滚动备份：同一份 .1 仍可再次恢复。
	assertFileContains(t, configBackupPath(path), `"old-key"`)
}

func mustSaveConfig(t *testing.T, path string, live *livecfg.Holder, p *pool.Pool, up *upstream.Client, sch *scheduler.Scheduler) {
	t.Helper()

	if _, err := saveConfig([]byte(`{"api_key":"new-key"}`), path, live, p, up, sch); err != nil {
		t.Fatalf("saveConfig: %v", err)
	}
}

func mustRestoreConfig(t *testing.T, path string, live *livecfg.Holder, p *pool.Pool, up *upstream.Client, sch *scheduler.Scheduler) {
	t.Helper()

	if _, err := restoreConfig(path, live, p, up, sch); err != nil {
		t.Fatalf("restoreConfig: %v", err)
	}
}

func assertFileContains(t *testing.T, path, want string) {
	t.Helper()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	if !strings.Contains(string(raw), want) {
		t.Fatalf("%s=%s want %s", path, raw, want)
	}
}

func assertLiveKey(t *testing.T, live *livecfg.Holder, want string) {
	t.Helper()

	if got := live.Load().APIKey; got != want {
		t.Fatalf("live api_key=%q want %q", got, want)
	}
}

// 并发保存必须串行化：共用 config.json.tmp 时交错写入会产出半截 JSON。
func TestSaveConfigConcurrentStaysValid(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"api_key":"old-key"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	state := pool.New("")
	client := upstream.New()
	sch := scheduler.New(&scheduler.Config{})
	live := livecfg.New(livecfg.Snapshot{APIKey: "old-key"})

	const n = 8

	errs := make([]error, n)

	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()

			_, errs[i] = saveConfig([]byte(fmt.Sprintf(`{"api_key":"k-%d"}`, i)), path, live, state, client, sch)
		}(i)
	}

	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("save %d: %v", i, err)
		}
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("并发保存后配置不是有效 JSON: %v\n%s", err, raw)
	}

	if _, ok := got["api_key"].(string); !ok {
		t.Fatalf("api_key 丢失: %s", raw)
	}
}

// TestPrepareConfigMergeRejectsNull 顶层 null 是合法 JSON 但不是配置对象：
// 合并/落盘必须拒绝，不能把 null 当空配置整体写入。
func TestPrepareConfigMergeRejectsNull(t *testing.T) {
	t.Parallel()

	if _, _, _, err := prepareConfigMerge([]byte(`{"api_key":"old"}`), []byte(`null`), true); err == nil {
		t.Fatal("null submitted config must be rejected")
	}
}

// TestPrepareConfigMergeNullCurrentConfig 当前配置为 null 时按空对象合并，
// 不能对 nil map 写入（panel 保存会 panic）。
func TestPrepareConfigMergeNullCurrentConfig(t *testing.T) {
	t.Parallel()

	merged, _, _, err := prepareConfigMerge([]byte(`null`), []byte(`{"api_key":"new"}`), true)
	if err != nil {
		t.Fatalf("prepareConfigMerge: %v", err)
	}

	if merged["api_key"] != "new" {
		t.Fatalf("merged=%v", merged)
	}
}
