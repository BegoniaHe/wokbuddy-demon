// quota_realm_key_test.go 钉住事件去重键含 realm：同一 uid 在两域都存在时，
// 缺 realm 会让第二域的样本被当成重复静默丢弃。
package quota

import (
	"testing"
	"time"
)

func TestEventKeyIncludesRealm(t *testing.T) {
	t.Parallel()

	store := New("")
	reset := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	at := time.Time{}

	if !store.Add(&Event{UID: "u1", Realm: "cn", Model: "m", ResetAt: reset, Used: 100, At: at}) {
		t.Fatal("cn 样本应被接受")
	}

	if !store.Add(&Event{UID: "u1", Realm: "global", Model: "m", ResetAt: reset, Used: 300, At: at}) {
		t.Fatal("global 样本被当成重复丢弃（eventKey 缺 realm）")
	}

	budgets := store.Budgets()

	if got := budgets["cn|m"]; got.Samples != 1 || got.Used != 100 {
		t.Fatalf("cn budget=%+v want used 100 samples 1", got)
	}

	if got := budgets["global|m"]; got.Samples != 1 || got.Used != 300 {
		t.Fatalf("global budget=%+v want used 300 samples 1", got)
	}
}
