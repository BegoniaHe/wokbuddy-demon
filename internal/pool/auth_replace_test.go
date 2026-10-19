package pool

import (
	"path/filepath"
	"testing"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
)

// TestAddRetiresReplacedAuth（R1）：同名账号 upsert 后，旧凭证必须退役，
// 其 SaveAtomic 不得再写回同一 auth 文件；池内保留新凭证。
func TestAddRetiresReplacedAuth(t *testing.T) {
	t.Parallel()

	testPool := newTestPool(t, "")
	fp := filepath.Join(t.TempDir(), "workbuddy-u1.json")

	oldAuth := &auth.Auth{UID: "u1", AccessToken: "old", RefreshToken: "old-rt", FilePath: fp}
	testPool.Add(oldAuth)

	replaced := &auth.Auth{UID: "u1", AccessToken: "new", RefreshToken: "new-rt", FilePath: fp}
	testPool.Add(replaced)

	if got := testPool.AuthByUID("u1"); got != replaced {
		t.Fatalf("pool kept %p, want new auth %p", got, replaced)
	}

	if err := oldAuth.SaveAtomic(); err == nil {
		t.Fatal("replaced auth SaveAtomic should be refused")
	}

	if err := replaced.SaveAtomic(); err != nil {
		t.Fatalf("active auth SaveAtomic failed: %v", err)
	}
}

// TestAddSameAuthPointerNotRetired：同指针重复 upsert 是幂等操作，不得退役仍在池内的凭证。
func TestAddSameAuthPointerNotRetired(t *testing.T) {
	t.Parallel()

	testPool := newTestPool(t, "")
	fp := filepath.Join(t.TempDir(), "workbuddy-u2.json")
	account := &auth.Auth{UID: "u2", AccessToken: "at", RefreshToken: "rt", FilePath: fp}

	testPool.Add(account)
	testPool.Add(account)

	if err := account.SaveAtomic(); err != nil {
		t.Fatalf("same-pointer upsert retired the active auth: %v", err)
	}
}

// TestSyncToDirRetiresRemovedAuth：目录扫描剔除的账号也必须退役，
// 否则在途 token refresh 仍可把已删账号的 auth 文件写回来。
func TestSyncToDirRetiresRemovedAuth(t *testing.T) {
	t.Parallel()

	testPool := newTestPool(t, "")
	dir := t.TempDir()
	removedPath := filepath.Join(dir, "workbuddy-u1.json")
	keptPath := filepath.Join(dir, "workbuddy-u2.json")

	removed := &auth.Auth{UID: "u1", AccessToken: "at1", RefreshToken: "rt1", FilePath: removedPath}
	kept := &auth.Auth{UID: "u2", AccessToken: "at2", RefreshToken: "rt2", FilePath: keptPath}

	testPool.SyncToDir([]*auth.Auth{removed, kept})
	testPool.SyncToDir([]*auth.Auth{kept})

	if err := removed.SaveAtomic(); err == nil {
		t.Fatal("removed auth SaveAtomic should be refused")
	}

	if err := kept.SaveAtomic(); err != nil {
		t.Fatalf("kept auth was retired: %v", err)
	}
}
