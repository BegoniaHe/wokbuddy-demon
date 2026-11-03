// credit_floor_config_test.go pool.credit_floor 配置测试：
// 默认 0（关闭，零回归）/ 文件覆盖 / 负值钳 0 / 大值合法。
package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestCreditFloorDefault 键缺席 → 默认 0（保底关闭，行为与引入前一致）。
func TestCreditFloorDefault(t *testing.T) {
	t.Parallel()

	config := Default()
	if err := config.normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}

	if config.Pool.CreditFloor != 0 {
		t.Errorf("credit_floor=%d want 0 (default off)", config.Pool.CreditFloor)
	}
}

// TestCreditFloorParsedFromFile 显式配置覆盖默认。
func TestCreditFloorParsedFromFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"pool":{"credit_floor":100}}`), 0o600)

	config, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}

	if config.Pool.CreditFloor != 100 {
		t.Errorf("credit_floor=%d want 100", config.Pool.CreditFloor)
	}
}

// TestCreditFloorNegativeClamped 负值钳 0（非法即关闭，不报错：老配置误写不炸启动）。
func TestCreditFloorNegativeClamped(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"pool":{"credit_floor":-5}}`), 0o600)

	config, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}

	if config.Pool.CreditFloor != 0 {
		t.Errorf("credit_floor=%d want 0 (negative clamped)", config.Pool.CreditFloor)
	}
}

// TestCreditFloorByRealmParsed 按域覆盖：cn=1000、global=350，未写的不出现。
func TestCreditFloorByRealmParsed(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"pool":{"credit_floor":100,"credit_floor_by_realm":{"cn":1000,"global":350}}}`), 0o600)

	config, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}

	if config.Pool.CreditFloor != 100 {
		t.Errorf("credit_floor=%d want 100", config.Pool.CreditFloor)
	}

	got := config.Pool.CreditFloorByRealm
	if got["cn"] == nil || *got["cn"] != 1000 || got["global"] == nil || *got["global"] != 350 {
		t.Fatalf("by realm = %#v want cn=1000 global=350", derefFloors(got))
	}
}

// TestCreditFloorByRealmZeroKept 显式 0 保留为该域关闭，不回落全局。
func TestCreditFloorByRealmZeroKept(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"pool":{"credit_floor":100,"credit_floor_by_realm":{"cn":0}}}`), 0o600)

	config, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}

	got := config.Pool.CreditFloorByRealm
	if got["cn"] == nil || *got["cn"] != 0 {
		t.Fatalf("cn 显式 0 应保留，got %#v", derefFloors(got))
	}

	if _, ok := got["global"]; ok {
		t.Fatalf("未写的 global 不应出现，got %#v", derefFloors(got))
	}
}

// TestCreditFloorByRealmDropsUnknownAndClampsNegative 非法键丢弃、负值钳 0。
func TestCreditFloorByRealmDropsUnknownAndClampsNegative(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"pool":{"credit_floor":80,"credit_floor_by_realm":{"eu":9,"global":-5,"CN":1}}}`), 0o600)

	config, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}

	got := config.Pool.CreditFloorByRealm
	if len(got) != 1 || got["global"] == nil || *got["global"] != 0 {
		t.Fatalf("只留 global 且钳 0，got %#v", derefFloors(got))
	}
}

func derefFloors(m map[string]*int64) map[string]int64 {
	out := map[string]int64{}

	for k, v := range m {
		if v == nil {
			continue
		}

		out[k] = *v
	}

	return out
}
