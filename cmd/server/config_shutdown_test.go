package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BegoniaHe/wokbuddy-demon/internal/livecfg"
	"github.com/BegoniaHe/wokbuddy-demon/internal/pool"
	"github.com/BegoniaHe/wokbuddy-demon/internal/scheduler"
	"github.com/BegoniaHe/wokbuddy-demon/internal/upstream"
)

func TestShutdownConfigDefaultsAndNormalize(t *testing.T) {
	c := Default()
	if c.Server.RequestAcceptGraceSeconds != 0 || c.Server.ShutdownGraceSeconds != 30 {
		t.Fatalf("defaults grace=%d shutdown=%d want 0/30",
			c.Server.RequestAcceptGraceSeconds, c.Server.ShutdownGraceSeconds)
	}
	c = Default()
	c.Server.ShutdownGraceSeconds = 0
	if err := c.normalize(); err != nil {
		t.Fatal(err)
	}
	if c.Server.ShutdownGraceSeconds != 30 {
		t.Fatalf("shutdown grace=%d want 30 after normalize", c.Server.ShutdownGraceSeconds)
	}
	c = Default()
	c.Server.RequestAcceptGraceSeconds = -1
	if err := c.normalize(); err == nil {
		t.Fatal("negative request_accept_grace_seconds should fail")
	}
	c = Default()
	c.Server.ShutdownGraceSeconds = -1
	if err := c.normalize(); err == nil {
		t.Fatal("negative shutdown_grace_seconds should fail")
	}
}

func TestRestartRequiredIncludesShutdownGrace(t *testing.T) {
	cases := []struct {
		field  string
		mutate func(*Config)
	}{
		{"server.request_accept_grace_seconds", func(c *Config) { c.Server.RequestAcceptGraceSeconds = 5 }},
		{"server.shutdown_grace_seconds", func(c *Config) { c.Server.ShutdownGraceSeconds = 10 }},
	}
	for _, tc := range cases {
		oldCfg := Default()
		newCfg := Default()
		tc.mutate(newCfg)
		got := restartRequiredFields(oldCfg, newCfg)
		found := false
		for _, f := range got {
			if f == tc.field {
				found = true
			}
		}
		if !found {
			t.Errorf("restart fields %v missing %q", got, tc.field)
		}
	}
}

func TestSaveConfigBackupAndRestore(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"api_key":"old-key","listen":":7863"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	p := pool.New("")
	up := upstream.New()
	sch := scheduler.New(scheduler.Config{})
	live := livecfg.New(livecfg.Snapshot{APIKey: "old-key"})

	mustSaveConfig(t, path, live, p, up, sch)
	assertFileContains(t, configBackupPath(path), `"old-key"`)
	assertLiveKey(t, live, "new-key")

	mustRestoreConfig(t, path, live, p, up, sch)
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
