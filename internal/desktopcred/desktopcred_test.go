package desktopcred

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
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
	km, err := deriveKey(secret)
	if err != nil {
		t.Fatal(err)
	}
	nonce := make([]byte, 12)
	if _, rerr := rand.Read(nonce); rerr != nil {
		t.Fatal(rerr)
	}
	block, err := aes.NewCipher(km.key)
	if err != nil {
		t.Fatal(err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	ct := gcm.Seal(nil, nonce, []byte(plaintext), fieldAAD(km.keyID))
	tag := ct[len(ct)-16:]
	ct = ct[:len(ct)-16]
	env, _ := json.Marshal(map[string]any{
		"suite": 1, "keyId": km.keyID,
		"nonce":      base64.StdEncoding.EncodeToString(nonce),
		"authTag":    base64.StdEncoding.EncodeToString(tag),
		"ciphertext": base64.StdEncoding.EncodeToString(ct),
	})
	return map[string]any{
		"$wbEncrypted": 1,
		"envelope":     base64.StdEncoding.EncodeToString(env),
	}
}

func writeAuthFile(t *testing.T, secret, domain string, encrypted bool) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "Data", "Public", "auth")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	at, rt := any("access-token"), any("refresh-token")
	if encrypted {
		at, rt = sealField(t, secret, "access-token"), sealField(t, secret, "refresh-token")
	}
	doc := map[string]any{
		"auth": map[string]any{
			"accessToken": at, "refreshToken": rt,
			"expiresAt": 1893456000, "domain": domain,
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

func assertImportedAuth(t *testing.T, a *auth.Auth) {
	t.Helper()
	if a.AccessToken != "access-token" || a.RefreshToken != "refresh-token" {
		t.Fatalf("tokens=%q/%q", a.AccessToken, a.RefreshToken)
	}
	if a.UID != "u1" || a.EnterpriseID != "e1" || a.Nickname != "nick" {
		t.Fatalf("account=%+v", a)
	}
	if !a.IsGlobal() {
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
	path := writeAuthFile(t, testSecret(t), "", true)
	if _, err := ImportFile(path, testSecret(t)); err == nil {
		t.Fatal("wrong key should fail")
	}
}

func TestImportPlaintextCredential(t *testing.T) {
	path := writeAuthFile(t, "", "", false)
	a, err := ImportFile(path, "")
	if err != nil {
		t.Fatalf("import plaintext: %v", err)
	}
	if a.AccessToken != "access-token" || a.IsGlobal() {
		t.Fatalf("auth=%+v", a)
	}
}

func TestDiscoverRoot(t *testing.T) {
	path := writeAuthFile(t, "", "", false)
	root := filepath.Dir(filepath.Dir(filepath.Dir(filepath.Dir(path)))) // .../Data/Public/auth -> temp
	got := discoverRoot(root)
	if len(got) != 1 || got[0] != path {
		t.Fatalf("discoverRoot=%v want [%s]", got, path)
	}
}

func TestRootsIncludeWorkbuddyBase(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	want := filepath.Join(home, ".workbuddy")
	for _, root := range Roots() {
		if root == want {
			return
		}
	}
	t.Fatalf("Roots() missing %s: %v", want, Roots())
}
