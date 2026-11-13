// hardening_test.go 钉住三处「静默降级」防线：
//   - 状态文件读不出来时拒绝写盘（不能用空集合覆盖磁盘上的真实密钥表）；
//   - Delete 回收该密钥的运行时账本（limits 原先只增不删）；
//   - RPM/TPM 有上界（过大时限速间隔归零、整形静默失效）。
package apikey

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestLoadFailureBlocksPersist 读不出来的状态文件不得被「空集合 + 新记录」覆盖。
func TestLoadFailureBlocksPersist(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "api_keys.json")
	corrupt := []byte(`{"version":2,"keys":[{"id":"half"`)

	if err := os.WriteFile(path, corrupt, 0o600); err != nil {
		t.Fatal(err)
	}

	store := New(path)
	if store.loadErr == nil {
		t.Fatal("损坏的状态文件应记录 loadErr")
	}

	if _, _, err := store.Create("laptop"); err == nil {
		t.Fatal("加载失败时 Create 必须报错，不得静默覆盖")
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	if string(after) != string(corrupt) {
		t.Fatalf("原文件被改写:\n got=%s\nwant=%s", after, corrupt)
	}
}

// TestDeleteDropsRuntimeLimits Delete 后运行时账本不得残留。
func TestDeleteDropsRuntimeLimits(t *testing.T) {
	t.Parallel()

	store := New("")

	rec, _, err := store.Create("laptop")
	if err != nil {
		t.Fatal(err)
	}

	store.limMu.Lock()
	store.runtimeLocked(rec.ID)
	store.limMu.Unlock()

	if ok, err := store.Delete(rec.ID); err != nil || !ok {
		t.Fatalf("Delete: ok=%v err=%v", ok, err)
	}

	store.limMu.Lock()
	_, leaked := store.limits[rec.ID]
	store.limMu.Unlock()

	if leaked {
		t.Fatal("Delete 后 limits 仍残留该密钥的运行时状态")
	}
}

// TestNormalizeLimitsRejectsHugeRates 过大的 RPM/TPM 会让 time.Minute/rpm 归零，
// TAT 永不前进、限速静默失效——必须按非法拒绝而不是悄悄关掉整形。
func TestNormalizeLimitsRejectsHugeRates(t *testing.T) {
	t.Parallel()

	for _, limits := range []Limits{
		{RPM: maxPacingRate + 1},
		{TPM: maxPacingRate + 1},
	} {
		if _, err := NormalizeLimits(limits); err == nil || !strings.Contains(err.Error(), "rpm/tpm") {
			t.Fatalf("limits=%+v err=%v want 上界拒绝", limits, err)
		}
	}

	if _, err := NormalizeLimits(Limits{RPM: maxPacingRate}); err != nil {
		t.Fatalf("上界本身应合法: %v", err)
	}
}

// TestLoadRejectsFutureVersion 更高格式版本必须拒绝加载：按当前二进制解释会出现
// 「哈希解不出来 → 密钥静默失效」而面板仍显示生效。
func TestLoadRejectsFutureVersion(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "api_keys.json")
	future := []byte(`{"version":99,"keys":[]}`)

	if err := os.WriteFile(path, future, 0o600); err != nil {
		t.Fatal(err)
	}

	store := New(path)
	if !errors.Is(store.loadErr, ErrUnsupportedVersion) {
		t.Fatalf("loadErr=%v want ErrUnsupportedVersion", store.loadErr)
	}
}

// TestLoadNormalizesLimits 落盘记录重跑一遍 Limits 归一化：否则手改/旧版本写出的
// rpm 会绕过 maxPacingRate 上界，time.Minute/rpm 归零后限速静默消失。
func TestLoadNormalizesLimits(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "api_keys.json")
	// 一条合法记录（hash 为 64 hex），limits 里带一个会被归一化的过期时间
	// （非 UTC 时区）。
	raw := []byte(`{"version":1,"keys":[{"id":"k1","name":"n","hash":"` +
		strings.Repeat("ab", 32) + `","limits":{"rpm":60,"expires_at":"2027-01-02T03:04:05+08:00"}}]}`)

	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	store := New(path)
	if store.loadErr != nil {
		t.Fatalf("loadErr=%v", store.loadErr)
	}

	store.mu.Lock()
	defer store.mu.Unlock()

	if len(store.records) != 1 {
		t.Fatalf("records=%d want 1", len(store.records))
	}

	exp := store.records[0].Limits.ExpiresAt
	if exp == nil || exp.Location() != time.UTC {
		t.Fatalf("expires_at=%v want UTC 归一化", exp)
	}
}

// TestLoadDropsInvalidLimitsRecord limits 非法（例如 rpm 超过上界会让时间间隔
// 归零、限速静默失效）的记录不能继续生效：与非法 hash 同口径整条丢弃，合法记录
// 照常加载。
func TestLoadDropsInvalidLimitsRecord(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "api_keys.json")
	raw := []byte(`{"version":1,"keys":[` +
		`{"id":"bad","name":"n","hash":"` + strings.Repeat("ab", 32) + `","limits":{"rpm":60000001}},` +
		`{"id":"good","name":"n","hash":"` + strings.Repeat("cd", 32) + `","limits":{"rpm":60}}]}`)

	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	store := New(path)
	if store.loadErr != nil {
		t.Fatalf("loadErr=%v", store.loadErr)
	}

	store.mu.Lock()
	defer store.mu.Unlock()

	if len(store.records) != 1 || store.records[0].ID != "good" {
		t.Fatalf("records=%+v want 仅 good", store.records)
	}
}
