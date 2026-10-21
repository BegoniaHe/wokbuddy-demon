package apikey

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/BegoniaHe/wokbuddy-demon/internal/httpauth"
)

func req(authz string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	if authz != "" {
		r.Header.Set("Authorization", authz)
	}

	return r
}

// brokenPath 返回一个必然写盘失败的路径：其父路径是一个普通文件（非目录），
// MkdirAll 会失败。与运行用户权限无关，可在 root/CI 下稳定复现。
func brokenPath(t *testing.T) string {
	t.Helper()

	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatalf("write blocker: %v", err)
	}

	return filepath.Join(blocker, "keys.json")
}

func TestCreateReturnsRawOnceAndHidesHash(t *testing.T) {
	t.Parallel()
	store := New(filepath.Join(t.TempDir(), "keys.json"))

	rec, raw, err := store.Create("laptop")
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	if raw == "" || rec.ID == "" || rec.Prefix == "" {
		t.Fatalf("bad create result: raw=%q rec=%+v", raw, rec)
	}

	if rec.Hash != "" {
		t.Error("returned record must not expose hash")
	}

	list := store.List()
	if len(list) != 1 {
		t.Fatalf("list len = %d, want 1", len(list))
	}

	if list[0].Hash != "" {
		t.Error("list must not expose hash")
	}
	// 校验快照含该密钥，且能用明文命中。
	got := httpauth.Authenticate(req("Bearer "+raw), "", store.Keys())
	if !got.OK || got.KeyID != rec.ID {
		t.Errorf("authenticate = %+v, want OK id=%s", got, rec.ID)
	}
}

func TestReloadPersistsAndAuthenticates(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "keys.json")
	store := New(path)

	_, raw, err := store.Create("phone")
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	store.RecordUsage(store.List()[0].ID, 10, 20, 30)
	store.Flush()

	store2 := New(path)
	if len(store2.List()) != 1 {
		t.Fatalf("reloaded list len = %d, want 1", len(store2.List()))
	}

	if r := store2.List()[0]; r.Requests != 1 || r.TotalTokens != 30 {
		t.Errorf("reloaded counters = %+v, want requests=1 total=30", r)
	}

	if got := httpauth.Authenticate(req("Bearer "+raw), "", store2.Keys()); !got.OK {
		t.Errorf("reloaded key should authenticate, got %+v", got)
	}
}

func TestCreateRollsBackWhenPersistenceFails(t *testing.T) {
	t.Parallel()

	store := New(brokenPath(t))
	if _, _, err := store.Create("x"); err == nil {
		t.Fatal("expected persistence error")
	}

	if len(store.records) != 0 || len(store.List()) != 0 {
		t.Fatalf("retained %d records after failed create", len(store.records))
	}
}

func TestRevokeRemovesFromKeys(t *testing.T) {
	t.Parallel()
	store := New(filepath.Join(t.TempDir(), "keys.json"))

	rec, raw, err := store.Create("x")
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	ok, err := store.Revoke(rec.ID)
	if err != nil || !ok {
		t.Fatalf("revoke = %v, %v", ok, err)
	}
	// 带一个默认密钥使鉴权生效；撤销后的命名密钥不应再命中。
	if got := httpauth.Authenticate(req("Bearer "+raw), "sk-admin", store.Keys()); got.OK {
		t.Errorf("revoked key must not authenticate, got %+v", got)
	}
	// 幂等：再次撤销返回 false, nil。
	if ok, err := store.Revoke(rec.ID); ok || err != nil {
		t.Errorf("second revoke = %v, %v, want false, nil", ok, err)
	}

	if r := store.List()[0]; !r.Revoked {
		t.Error("list should show revoked=true")
	}
}

func TestRevokeRollsBackWhenPersistenceFails(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "keys.json")
	store := New(path)

	rec, _, err := store.Create("x")
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	store.path = brokenPath(t)

	ok, err := store.Revoke(rec.ID)
	if err == nil || ok {
		t.Fatalf("revoke = %v, %v, want persistence failure", ok, err)
	}

	if store.records[0].Revoked {
		t.Fatal("key remained revoked after failed save")
	}
}

func TestDeletePhysicallyRemoves(t *testing.T) {
	t.Parallel()
	store := New(filepath.Join(t.TempDir(), "keys.json"))
	r1, _, _ := store.Create("one")
	record, _, _ := store.Create("two")

	ok, err := store.Delete(r1.ID)
	if err != nil || !ok {
		t.Fatalf("delete = %v, %v", ok, err)
	}

	list := store.List()
	if len(list) != 1 || list[0].ID != record.ID {
		t.Fatalf("unexpected remaining keys: %+v", list)
	}

	if ok, _ := store.Delete("no-such-id"); ok {
		t.Error("delete of unknown id should report false")
	}
}

func TestDeleteRollsBackWhenPersistenceFails(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "keys.json")
	store := New(path)
	rec, _, _ := store.Create("x")
	store.path = brokenPath(t)

	ok, err := store.Delete(rec.ID)
	if err == nil || ok {
		t.Fatalf("delete = %v, %v, want persistence failure", ok, err)
	}

	if len(store.records) != 1 || store.records[0].ID != rec.ID {
		t.Fatalf("key not restored after failed delete: %+v", store.records)
	}
}

func TestRecordUsageNilSafeAndCounts(t *testing.T) {
	t.Parallel()

	var nilStore *Store
	nilStore.RecordUsage("id", 1, 2, 3) // 不得 panic

	store := New("")
	rec, _, _ := store.Create("x")
	store.RecordUsage(rec.ID, 100, 200, 300)
	store.RecordUsage(rec.ID, 1, 2, 3)
	store.RecordUsage("missing", 9, 9, 9) // 未知 id 忽略

	got := store.List()[0]
	if got.Requests != 2 || got.PromptTokens != 101 || got.CompletionTokens != 202 || got.TotalTokens != 303 {
		t.Errorf("counters = %+v", got)
	}

	if got.LastUsedAt == nil || got.LastUsedAt.IsZero() {
		t.Error("last_used_at not set")
	}
}

func TestSetRealmPersistsAndValidates(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "keys.json")
	store := New(path)

	rec, _, err := store.Create("client")
	if err != nil {
		t.Fatal(err)
	}

	if ok, err := store.SetRealm(rec.ID, "global"); !ok || err != nil {
		t.Fatalf("set realm: ok=%v err=%v", ok, err)
	}

	if got := store.Realm(rec.ID); got != "global" {
		t.Fatalf("realm=%q want global", got)
	}

	if _, err := store.SetRealm(rec.ID, "mars"); err == nil {
		t.Error("invalid realm should fail")
	}

	reloaded := New(path)
	if got := reloaded.Realm(rec.ID); got != "global" {
		t.Errorf("reloaded realm=%q want global", got)
	}
}

// TestLoadFallsBackToBackup 主文件损坏时从 .bak 恢复上一份有效记录。
func TestLoadFallsBackToBackup(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "keys.json")

	store := New(path)
	if _, _, err := store.Create("a"); err != nil {
		t.Fatalf("create a: %v", err)
	}
	// 第二次写盘把「只有 a」的版本留成 .bak。
	if _, _, err := store.Create("b"); err != nil {
		t.Fatalf("create b: %v", err)
	}

	if err := os.WriteFile(path, []byte("bad"), 0o600); err != nil {
		t.Fatal(err)
	}

	reloaded := New(path)
	if got := len(reloaded.List()); got != 1 {
		t.Fatalf("records=%d want 1 (restored from .bak)", got)
	}
}
