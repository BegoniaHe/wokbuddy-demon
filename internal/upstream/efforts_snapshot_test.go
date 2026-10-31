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

// TestModelMaxOutSnapshot storeModelMaxOut 只收录正上限、按 realm 分层；刷新后
// 已取出的旧快照不变（读路径零拷贝的前提）。
func TestModelMaxOutSnapshot(t *testing.T) {
	t.Parallel()

	client := New()
	client.storeModelMaxOut("cn", []ModelInfo{
		{ID: "m1", MaxTokens: 1000, Aliases: []string{"m1-alias"}},
		{ID: "m2"},
	})

	_, _, old := client.chatBodySnapshots("cn")
	if old["m1"] != 1000 {
		t.Fatalf("m1=%d want 1000", old["m1"])
	}

	if old["m1-alias"] != 1000 {
		t.Fatalf("别名 m1-alias=%d want 1000（别名请求同样要补默认上限）", old["m1-alias"])
	}

	if _, ok := old["m2"]; ok {
		t.Fatal("上限为零的模型不应入桶")
	}

	if _, _, global := client.chatBodySnapshots("global"); global != nil {
		t.Fatal("global 桶不应被 cn 写入污染")
	}

	client.storeModelMaxOut("cn", []ModelInfo{{ID: "m1", MaxTokens: 2000}})

	if old["m1"] != 1000 {
		t.Fatalf("旧快照被刷新改写: %d", old["m1"])
	}

	if _, _, next := client.chatBodySnapshots("cn"); next["m1"] != 2000 {
		t.Fatalf("新快照 m1=%d want 2000", next["m1"])
	}
}
