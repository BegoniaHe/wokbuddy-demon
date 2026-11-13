// partition_test.go 钉住号池分区（issue #491）：开启后媒体请求只从媒体号选、
// 文本请求只从文本号选；分区关闭或号耗尽时的口径明确。
package pool

import (
	"testing"
	"time"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
)

func partitionTestPool(t *testing.T) *Pool {
	t.Helper()

	testPool := newTestPool(t, "")
	testPool.Add(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	testPool.Add(&auth.Auth{UID: "u2", AccessToken: "at2", ExpiresAt: 9999999999})

	return testPool
}

// TestMediaPartitionSplitsPools 开启分区：媒体只选媒体号，文本只选其余账号。
func TestMediaPartitionSplitsPools(t *testing.T) {
	t.Parallel()

	testPool := partitionTestPool(t)
	testPool.SetMediaPartition(true, []string{"u1"})

	for range 5 {
		if acct := testPool.PickExcludingForRealmMedia(nil, "", "", true); acct == nil || acct.UID != "u1" {
			t.Fatalf("媒体请求应只选媒体号 u1, got %v", acct)
		}

		if acct := testPool.PickExcludingForRealmMedia(nil, "", "", false); acct == nil || acct.UID != "u2" {
			t.Fatalf("文本请求应只选文本号 u2, got %v", acct)
		}
	}
}

// TestMediaPartitionExhaustedFails media 号耗尽：明确失败，不跨区回落。
func TestMediaPartitionExhaustedFails(t *testing.T) {
	t.Parallel()

	testPool := partitionTestPool(t)
	testPool.SetMediaPartition(true, nil)

	if acct := testPool.PickExcludingForRealmMedia(nil, "", "", true); acct != nil {
		t.Fatalf("无媒体号时媒体请求应失败, got %s", acct.UID)
	}
}

// TestMediaPartitionFallbackStaysInPartition 全冷却兜底同样不跨区。
func TestMediaPartitionFallbackStaysInPartition(t *testing.T) {
	t.Parallel()

	testPool := partitionTestPool(t)
	testPool.SetMediaPartition(true, []string{"u1"})

	// 媒体号软冷却 → 正常候选为空，兜底也不许选文本号 u2。
	testPool.Cooldown("u1", CoolSoft, time.Minute, "429")

	if acct := testPool.PickExcludingForRealmMedia(nil, "", "", true); acct != nil && acct.UID == "u2" {
		t.Fatalf("兜底不应跨区选到文本号: %s", acct.UID)
	}
}

// TestMediaPartitionDisabledUnchanged 关闭分区：两号都参与选号。
func TestMediaPartitionDisabledUnchanged(t *testing.T) {
	t.Parallel()

	testPool := partitionTestPool(t)

	seen := map[string]bool{}

	for range 40 {
		acct := testPool.PickExcludingForRealmMedia(nil, "", "", true)
		if acct == nil {
			t.Fatal("分区关闭时媒体请求不应失败")
		}

		seen[acct.UID] = true
	}

	if !seen["u1"] || !seen["u2"] {
		t.Fatalf("分区关闭时两号都应可选中, seen=%v", seen)
	}
}

// TestMediaPartitionHotToggle 热切换：关闭分区后媒体请求重新可用。
func TestMediaPartitionHotToggle(t *testing.T) {
	t.Parallel()

	testPool := partitionTestPool(t)
	testPool.SetMediaPartition(true, nil)

	if acct := testPool.PickExcludingForRealmMedia(nil, "", "", true); acct != nil {
		t.Fatalf("分区开启且无媒体号应失败, got %s", acct.UID)
	}

	testPool.SetMediaPartition(false, nil)

	if acct := testPool.PickExcludingForRealmMedia(nil, "", "", true); acct == nil {
		t.Fatal("关闭分区后媒体请求应恢复可用")
	}
}
