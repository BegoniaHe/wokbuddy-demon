package livecfg

import (
	"testing"
)

// TestStoreDetachesMaps Store 必须把名单 map 深拷贝归 Holder 独占：调用方在
// Store 之后改写自己那份 map 不得污染已发布的快照（浅拷贝会让底层数组跨读者共享）。
func TestStoreDetachesMaps(t *testing.T) {
	t.Parallel()

	names := map[string]bool{"glm-5.2": true}
	hidden := map[string]struct{}{"cn:kimi": {}}
	allowed := map[string]struct{}{"cn:glm-5.2": {}}

	holder := New(Snapshot{BackgroundNames: names, HiddenModels: hidden, AllowedModels: allowed})

	// 改写调用方持有的原始 map：Holder 里的快照不应随之变化。
	names["added-later"] = true
	delete(names, "glm-5.2")

	hidden["cn:added-later"] = struct{}{}

	allowed["cn:added-later"] = struct{}{}

	loaded := holder.Load()

	if loaded.BackgroundNames["added-later"] || !loaded.BackgroundNames["glm-5.2"] {
		t.Errorf("BackgroundNames 被外部写入污染: %v", loaded.BackgroundNames)
	}

	if _, ok := loaded.HiddenModels["cn:added-later"]; ok {
		t.Errorf("HiddenModels 被外部写入污染: %v", loaded.HiddenModels)
	}

	if _, ok := loaded.AllowedModels["cn:added-later"]; ok {
		t.Errorf("AllowedModels 被外部写入污染: %v", loaded.AllowedModels)
	}
}

// TestStoreDetachesMapsAcrossStores 连续 Store 之间同样互不共享底层 map。
func TestStoreDetachesMapsAcrossStores(t *testing.T) {
	t.Parallel()

	first := map[string]struct{}{"cn:a": {}}
	holder := New(Snapshot{HiddenModels: first})
	holder.Store(Snapshot{HiddenModels: map[string]struct{}{"cn:b": {}}})

	first["cn:injected"] = struct{}{}

	if _, ok := holder.Load().HiddenModels["cn:injected"]; ok {
		t.Error("旧快照的 map 仍与新 Holder 共享底层数组")
	}
}
