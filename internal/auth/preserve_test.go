// preserve_test.go 钉住刷新回写对来源文件的保守语义（issue #540）：未建模键
// （account.phoneNumber / uin / email、顶层扩展键）原样保留，expiresAt 沿用来源
// 文件的单位——网关目录可能同时被官方客户端读取，丢键或换单位都会让客户端读错。
package auth

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// readDoc 把落盘文件解码成通用文档，用于断言未建模键。
func readDoc(t *testing.T, path string) map[string]any {
	t.Helper()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("unmarshal %s: %v", path, err)
	}

	return doc
}

// TestSaveAtomicPreservesUnknownKeys 嵌套形来源文件刷新一次后，未建模键与值仍在，
// 已知键照常更新。
func TestSaveAtomicPreservesUnknownKeys(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "workbuddy-u1.json")
	source := `{
  "auth": {
    "accessToken": "old-at",
    "refreshToken": "old-rt",
    "expiresAt": 1753600000,
    "domain": "",
    "unknownAuthKey": "keep-auth"
  },
  "account": {
    "uid": "u1",
    "enterpriseId": "e1",
    "nickname": "n1",
    "phoneNumber": "13800000000",
    "uin": "10001",
    "email": "u1@example.com"
  },
  "client": {"installId": "dev-1"},
  "expiresIn": 3600
}`

	if err := os.WriteFile(path, []byte(source), 0o600); err != nil {
		t.Fatalf("write source: %v", err)
	}

	account, err := Parse([]byte(source))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	account.FilePath = path
	account.AccessToken = "new-at"
	account.RefreshToken = "new-rt"

	if err := account.SaveAtomic(); err != nil {
		t.Fatalf("save: %v", err)
	}

	doc := readDoc(t, path)
	assertUpdatedTokens(t, doc, "new-at", "new-rt")
	assertUnknownKeysPreserved(t, doc)
}

// assertUpdatedTokens 断言 auth 对象的已知 token 已按本次刷新更新。
func assertUpdatedTokens(t *testing.T, doc map[string]any, access, refresh string) {
	t.Helper()

	authDoc, _ := doc["auth"].(map[string]any)
	if authDoc["accessToken"] != access || authDoc["refreshToken"] != refresh {
		t.Errorf("已知 token 未更新: %v", authDoc)
	}
}

// assertUnknownKeysPreserved 断言未建模键（auth/account/顶层）与值原样保留。
func assertUnknownKeysPreserved(t *testing.T, doc map[string]any) {
	t.Helper()

	authDoc, _ := doc["auth"].(map[string]any)
	if authDoc["unknownAuthKey"] != "keep-auth" {
		t.Errorf("auth.unknownAuthKey=%v want keep-auth", authDoc["unknownAuthKey"])
	}

	accountDoc, _ := doc["account"].(map[string]any)
	for key, want := range map[string]string{
		"phoneNumber": "13800000000",
		"uin":         "10001",
		"email":       "u1@example.com",
	} {
		if accountDoc[key] != want {
			t.Errorf("account.%s=%v want %s", key, accountDoc[key], want)
		}
	}

	if accountDoc["uid"] != "u1" || accountDoc["enterpriseId"] != "e1" {
		t.Errorf("已知账号字段被改写: %v", accountDoc)
	}

	if client, _ := doc["client"].(map[string]any); client["installId"] != "dev-1" {
		t.Errorf("顶层未知对象丢失: %v", doc["client"])
	}

	if got, _ := doc["expiresIn"].(float64); got != 3600 {
		t.Errorf("顶层未知标量 expiresIn=%v want 3600", doc["expiresIn"])
	}
}

// TestSaveAtomicKeepsExpiresAtMillisUnit 来源是毫秒时，回写仍按毫秒，不做单位
// 换算（内部仍按秒计算，只在落盘时还原）。
func TestSaveAtomicKeepsExpiresAtMillisUnit(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "workbuddy-u2.json")
	millis := time.Now().Add(time.Hour).UnixMilli()
	source := fmt.Sprintf(
		`{"auth":{"accessToken":"old","refreshToken":"rt","expiresAt":%d,"domain":""},"account":{"uid":"u2"}}`,
		millis)

	if err := os.WriteFile(path, []byte(source), 0o600); err != nil {
		t.Fatalf("write source: %v", err)
	}

	account, err := Parse([]byte(source))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	if account.ExpiresAt != millis/1000 {
		t.Fatalf("内部 ExpiresAt=%d want 秒", account.ExpiresAt)
	}

	account.FilePath = path
	account.AccessToken = "new"

	if err := account.SaveAtomic(); err != nil {
		t.Fatalf("save: %v", err)
	}

	authDoc, _ := readDoc(t, path)["auth"].(map[string]any)
	// 内部按秒归一，毫秒单位写回时亚秒位被截断（刷新本就会把 expiresAt 重算为
	// now+ttl，这里只保证单位不变成秒）。
	want := millis / 1000 * 1000
	if got, _ := authDoc["expiresAt"].(float64); int64(got) != want {
		t.Errorf("expiresAt=%v want %d（沿用来源毫秒单位）", authDoc["expiresAt"], want)
	}
}

// TestSaveAtomicKeepsSecondsUnit 秒级来源仍写秒（不因引入单位记录而翻倍）。
func TestSaveAtomicKeepsSecondsUnit(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "workbuddy-u3.json")
	seconds := time.Now().Add(time.Hour).Unix()
	source := fmt.Sprintf(
		`{"auth":{"accessToken":"old","refreshToken":"rt","expiresAt":%d,"domain":""},"account":{"uid":"u3"}}`,
		seconds)

	if err := os.WriteFile(path, []byte(source), 0o600); err != nil {
		t.Fatalf("write source: %v", err)
	}

	account, err := Parse([]byte(source))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	account.FilePath = path
	account.AccessToken = "new"

	if err := account.SaveAtomic(); err != nil {
		t.Fatalf("save: %v", err)
	}

	authDoc, _ := readDoc(t, path)["auth"].(map[string]any)
	if got, _ := authDoc["expiresAt"].(float64); int64(got) != seconds {
		t.Errorf("expiresAt=%v want %d（秒）", authDoc["expiresAt"], seconds)
	}
}

// TestSaveAtomicFlatSourceMigratesUnknownKeys 扁平形来源转嵌套形时，已知顶层键
// 不重复留旧值，未知键仍留在顶层（不臆造归属层级）。
func TestSaveAtomicFlatSourceMigratesUnknownKeys(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "workbuddy-u4.json")
	source := `{"accessToken":"old","refreshToken":"rt","expiresAt":1753600000,"uid":"u4","phoneNumber":"13800000004","uin":"4004"}`

	if err := os.WriteFile(path, []byte(source), 0o600); err != nil {
		t.Fatalf("write source: %v", err)
	}

	account, err := Parse([]byte(source))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	account.FilePath = path
	account.AccessToken = "new"

	if err := account.SaveAtomic(); err != nil {
		t.Fatalf("save: %v", err)
	}

	doc := readDoc(t, path)

	authDoc, _ := doc["auth"].(map[string]any)
	if authDoc["accessToken"] != "new" {
		t.Errorf("auth.accessToken=%v want new", authDoc["accessToken"])
	}

	for _, key := range []string{"accessToken", "refreshToken", "expiresAt", "uid"} {
		if _, exists := doc[key]; exists {
			t.Errorf("顶层残留旧键 %s=%v（应迁入嵌套形）", key, doc[key])
		}
	}

	if doc["phoneNumber"] != "13800000004" || doc["uin"] != "4004" {
		t.Errorf("顶层未知键丢失: %v", doc)
	}
}

// TestSaveAtomicWithoutSourceFileStillWritesNested 文件不存在时保持全量重建
// （首次落盘路径不受保留逻辑影响）。
func TestSaveAtomicWithoutSourceFileStillWritesNested(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "workbuddy-u5.json")

	account := &Auth{
		AccessToken: "at", RefreshToken: "rt", ExpiresAt: 1753600000,
		UID: "u5", FilePath: path,
	}
	if err := account.SaveAtomic(); err != nil {
		t.Fatalf("save: %v", err)
	}

	doc := readDoc(t, path)
	if _, ok := doc["auth"].(map[string]any); !ok {
		t.Fatalf("首次落盘应写嵌套形: %v", doc)
	}

	if _, ok := doc["account"].(map[string]any); !ok {
		t.Fatalf("首次落盘应写 account: %v", doc)
	}
}

// TestSaveAtomicPreservesBigIntUnknownKeys 未建模键必须按原始字节保留：经
// map[string]any 往返会把整数换成 float64，openId / uin 这类大整数最低位就变了。
func TestSaveAtomicPreservesBigIntUnknownKeys(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "workbuddy-u6.json")
	source := `{"auth":{"accessToken":"old","refreshToken":"rt","expiresAt":1753600000},` +
		`"account":{"uid":"u6","openId":1234567890123456789}}`

	if err := os.WriteFile(path, []byte(source), 0o600); err != nil {
		t.Fatalf("write source: %v", err)
	}

	account, err := Parse([]byte(source))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	account.FilePath = path
	account.AccessToken = "new"

	if saveErr := account.SaveAtomic(); saveErr != nil {
		t.Fatalf("save: %v", saveErr)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	if !strings.Contains(string(raw), "1234567890123456789") {
		t.Fatalf("未建模大整数被改写: %s", raw)
	}
}
