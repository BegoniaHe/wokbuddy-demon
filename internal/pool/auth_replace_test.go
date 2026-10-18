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

	p := newTestPool(t, "")
	fp := filepath.Join(t.TempDir(), "workbuddy-u1.json")

	oldAuth := &auth.Auth{UID: "u1", AccessToken: "old", RefreshToken: "old-rt", FilePath: fp}
	p.Add(oldAuth)

	replaced := &auth.Auth{UID: "u1", AccessToken: "new", RefreshToken: "new-rt", FilePath: fp}
	p.Add(replaced)

	if got := p.AuthByUID("u1"); got != replaced {
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

	p := newTestPool(t, "")
	fp := filepath.Join(t.TempDir(), "workbuddy-u2.json")
	account := &auth.Auth{UID: "u2", AccessToken: "at", RefreshToken: "rt", FilePath: fp}

	p.Add(account)
	p.Add(account)

	if err := account.SaveAtomic(); err != nil {
		t.Fatalf("same-pointer upsert retired the active auth: %v", err)
	}
}
