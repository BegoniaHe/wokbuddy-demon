package upstream

import "testing"

// TestEffortsSnapshotImmutable storeEfforts 在写入边界克隆输入，快照发布后不可变；
// 后续刷新替换桶不影响已取出的旧快照。
func TestEffortsSnapshotImmutable(t *testing.T) {
	t.Parallel()

	client := New()
	in := map[string][]string{"m1": {"low", "high"}}
	client.storeEfforts("cn", in, map[string]string{"m1": "high"})
	// 写入后调用方改输入，不得污染缓存。
	in["m1"][0] = "mutated"

	old, defs := client.effortsSnapshot("cn"), client.defaultEffortsSnapshot("cn")
	if old["m1"][0] != "low" || defs["m1"] != "high" {
		t.Fatalf("store did not clone input: efforts=%v defaults=%v", old, defs)
	}

	client.storeEfforts("cn", map[string][]string{"m1": {"medium"}}, map[string]string{"m1": "medium"})

	if old["m1"][0] != "low" {
		t.Fatalf("old snapshot mutated by refresh: %v", old)
	}

	next, _ := client.effortsSnapshot("cn"), client.defaultEffortsSnapshot("cn")
	if next["m1"][0] != "medium" {
		t.Fatalf("new snapshot=%v want medium", next)
	}
}
