package desktopcred

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
)

func testSecret(t *testing.T) string {
	t.Helper()

	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		t.Fatal(err)
	}

	return base64.StdEncoding.EncodeToString(raw)
}

// sealField 按客户端字段格式加密，用于构造测试 auth 文件。
func sealField(t *testing.T, secret, plaintext string) map[string]any {
	t.Helper()

	keyMaterial2, err := deriveKey(secret)
	if err != nil {
		t.Fatal(err)
	}

	nonce := make([]byte, 12)
	if _, rerr := rand.Read(nonce); rerr != nil {
		t.Fatal(rerr)
	}

	block, err := aes.NewCipher(keyMaterial2.key)
	if err != nil {
		t.Fatal(err)
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}

	nums := gcm.Seal(nil, nonce, []byte(plaintext), fieldAAD(keyMaterial2.keyID))
	tag := nums[len(nums)-16:]
	nums = nums[:len(nums)-16]
	env, _ := json.Marshal(map[string]any{
		"suite": 1, "keyId": keyMaterial2.keyID,
		"nonce":      base64.StdEncoding.EncodeToString(nonce),
		"authTag":    base64.StdEncoding.EncodeToString(tag),
		"ciphertext": base64.StdEncoding.EncodeToString(nums),
	})

	return map[string]any{
		"$wbEncrypted": 1,
		"envelope":     base64.StdEncoding.EncodeToString(env),
	}
}

func writeAuthFile(t *testing.T, secret, domain string, encrypted bool) string {
	t.Helper()

	return writeAuthFileWithExpiry(t, secret, domain, encrypted, 1893456000)
}

func writeAuthFileWithExpiry(t *testing.T, secret, domain string, encrypted bool, expiresAt int64) string {
	t.Helper()

	dir := filepath.Join(t.TempDir(), "Data", "Public", "auth")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}

	iface, iface2 := any("access-token"), any("refresh-token")
	if encrypted {
		iface, iface2 = sealField(t, secret, "access-token"), sealField(t, secret, "refresh-token")
	}

	doc := map[string]any{
		"auth": map[string]any{
			"accessToken": iface, "refreshToken": iface2,
			"expiresAt": expiresAt, "domain": domain,
		},
		"account": map[string]any{"uid": "u1", "enterpriseId": "e1", "nickname": "nick"},
	}
	raw, _ := json.Marshal(doc)

	path := filepath.Join(dir, "wb.info")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	return path
}

func TestImportEncryptedCredential(t *testing.T) {
	t.Parallel()
	secret := testSecret(t)
	path := writeAuthFile(t, secret, "www.workbuddy.ai", true)

	a, err := ImportFile(path, secret)
	if err != nil {
		t.Fatalf("import: %v", err)
	}

	assertImportedAuth(t, a)

	meta, err := InspectFile(path)
	if err != nil {
		t.Fatal(err)
	}

	assertEncryptedMetadata(t, meta)
}

func assertImportedAuth(t *testing.T, account *auth.Auth) {
	t.Helper()

	if account.AccessToken != "access-token" || account.RefreshToken != "refresh-token" {
		t.Fatalf("tokens=%q/%q", account.AccessToken, account.RefreshToken)
	}

	if account.UID != "u1" || account.EnterpriseID != "e1" || account.Nickname != "nick" {
		t.Fatalf("account=%+v", account)
	}

	if !account.IsGlobal() {
		t.Fatal("workbuddy.ai credential should be global")
	}
}

func assertEncryptedMetadata(t *testing.T, meta Account) {
	t.Helper()

	if !meta.Encrypted || meta.UID != "u1" || meta.Realm != "global" || meta.ExpiresAt != 1893456000 {
		t.Fatalf("metadata=%+v", meta)
	}
}

func TestImportWrongKeyFails(t *testing.T) {
	t.Parallel()

	path := writeAuthFile(t, testSecret(t), "", true)
	if _, err := ImportFile(path, testSecret(t)); err == nil {
		t.Fatal("wrong key should fail")
	}
}

func TestImportPlaintextCredential(t *testing.T) {
	t.Parallel()
	path := writeAuthFile(t, "", "", false)

	account, err := ImportFile(path, "")
	if err != nil {
		t.Fatalf("import plaintext: %v", err)
	}

	if account.AccessToken != "access-token" || account.IsGlobal() {
		t.Fatalf("auth=%+v", account)
	}
}

// TestExpiresAtMillisNormalized 官方客户端把 expiresAt 写成 Unix 毫秒；
// 导入构造的 Auth 与扫描元数据都必须归一为秒，否则 NeedsRefresh 永假。
func TestExpiresAtMillisNormalized(t *testing.T) {
	t.Parallel()

	path := writeAuthFileWithExpiry(t, "", "", false, 1893456000000)

	account, err := ImportFile(path, "")
	if err != nil {
		t.Fatalf("import plaintext: %v", err)
	}

	if account.ExpiresAt != 1893456000 {
		t.Errorf("import expiresAt=%d want 1893456000（毫秒归一为秒）", account.ExpiresAt)
	}

	meta, err := InspectFile(path)
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}

	if meta.ExpiresAt != 1893456000 {
		t.Errorf("inspect expiresAt=%d want 1893456000（毫秒归一为秒）", meta.ExpiresAt)
	}
}

func TestDiscoverRoot(t *testing.T) {
	t.Parallel()
	path := writeAuthFile(t, "", "", false)
	root := filepath.Dir(filepath.Dir(filepath.Dir(filepath.Dir(path)))) // .../Data/Public/auth -> temp

	got := discoverRoot(root)
	if len(got) != 1 || got[0] != path {
		t.Fatalf("discoverRoot=%v want [%s]", got, path)
	}
}

//nolint:paralleltest // 该用例改写进程级状态，必须串行执行
func TestRootsIncludeWorkbuddyBase(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	want := filepath.Join(home, ".workbuddy")
	if slices.Contains(Roots(), want) {
		return
	}

	t.Fatalf("Roots() missing %s: %v", want, Roots())
}
