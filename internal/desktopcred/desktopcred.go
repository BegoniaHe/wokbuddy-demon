// Package desktopcred 读取 WorkBuddy 桌面客户端落盘的登录态（issue #98）。
//
// 客户端把 session 写到 <base>/Data/Public/auth/<id>.info；敏感字段是
// `$wbEncrypted` wrapper（AES-256-GCM，字段级 AAD）。本包只读扫描，不修改客户端
// 文件；密钥仅用于本次解密，不落盘、不写日志。
package desktopcred

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
)

// Account 扫描结果：只含面板展示所需元数据，不含 token。
type Account struct {
	Path      string `json:"path"`
	UID       string `json:"uid"`
	Nickname  string `json:"nickname,omitempty"`
	Domain    string `json:"domain,omitempty"`
	Realm     string `json:"realm"`
	ExpiresAt int64  `json:"expires_at,omitempty"`
	Encrypted bool   `json:"encrypted"`
}

// Roots 返回各平台桌面客户端的候选数据目录（只读扫描）。
func Roots() []string {
	home, _ := os.UserHomeDir()
	bases := make([]string, 0, 1)
	switch runtime.GOOS {
	case "darwin":
		bases = []string{filepath.Join(home, "Library", "Application Support")}
	case "windows":
		if local := os.Getenv("LOCALAPPDATA"); local != "" {
			bases = []string{local}
		}
	case "linux":
		bases = []string{filepath.Join(home, ".local", "share"), filepath.Join(home, ".config")}
	}
	names := []string{"WorkBuddy", "WorkBuddy AI"}
	if v := strings.TrimSpace(os.Getenv("WORKBUDDY_APP_NAME")); v != "" {
		names = append(names, v)
	}
	seen := map[string]bool{}
	var out []string
	add := func(root string) {
		root = filepath.Clean(root)
		if seen[root] {
			return
		}
		seen[root] = true
		out = append(out, root)
	}
	for _, base := range bases {
		for _, name := range names {
			add(filepath.Join(base, name))
		}
	}
	// CN 桌面客户端的 dataFolderName 为 .workbuddy：sharedDataPath 直接位于
	// ~/.workbuddy/Data/Public，而不是 ~/.workbuddy/<产品名>/Data/Public。
	add(filepath.Join(home, ".workbuddy"))
	return out
}

// Discover 扫描本机所有候选目录，返回 *.info 路径（已去重、排序）。
func Discover() []string {
	seen := map[string]bool{}
	var out []string
	for _, root := range Roots() {
		for _, p := range discoverRoot(root) {
			if !seen[p] {
				seen[p] = true
				out = append(out, p)
			}
		}
	}
	sort.Strings(out)
	return out
}

// Scan 扫描并返回账号元数据；单个文件损坏不影响其它账号。
func Scan() []Account {
	out := make([]Account, 0, 4)
	for _, p := range Discover() {
		if a, err := InspectFile(p); err == nil {
			out = append(out, a)
		}
	}
	return out
}

// discoverRoot 扫描一个根目录下的 Data/Public/auth/*.info。
func discoverRoot(root string) []string {
	patterns := []string{
		filepath.Join(root, "Data", "Public", "auth", "*.info"),
		filepath.Join(root, "data", "public", "auth", "*.info"),
	}
	var out []string
	for _, pattern := range patterns {
		matches, _ := filepath.Glob(pattern)
		out = append(out, matches...)
	}
	return out
}

// InspectFile 只读元数据；不要求密钥，也不解密 token。
func InspectFile(path string) (Account, error) {
	doc, err := readDoc(path)
	if err != nil {
		return Account{}, err
	}
	a := Account{Path: path}
	if m := object(doc["auth"]); m != nil {
		a.Domain, _ = m["domain"].(string)
		if n, ok := int64Value(m["expiresAt"]); ok {
			a.ExpiresAt = n
		}
		if n, ok := int64Value(m["expires_at"]); ok {
			a.ExpiresAt = n
		}
		if s, ok := m["realm"].(string); ok {
			a.Realm = s
		}
		a.Encrypted = isEncrypted(m["accessToken"]) || isEncrypted(m["refreshToken"])
	}
	if m := object(doc["account"]); m != nil {
		a.UID, _ = m["uid"].(string)
		a.Nickname, _ = m["nickname"].(string)
	}
	a.Realm = auth.ResolveRealm(a.Realm, a.Domain)
	return a, nil
}

// ImportFile 解密并构造网关 Auth。secret 为客户端 at-rest secret 的 base64 原文；
// 明文 token 可传空串。密钥只在内存中使用。
func ImportFile(path, secret string) (*auth.Auth, error) {
	doc, err := readDoc(path)
	if err != nil {
		return nil, err
	}
	authObj, accountObj := authObjects(doc)
	if authObj == nil {
		return nil, errors.New("auth object missing")
	}
	key, err := keyFromSecret(secret)
	if err != nil {
		return nil, err
	}
	a, err := buildAuth(doc, authObj, accountObj, key)
	if err != nil {
		return nil, err
	}
	applyRealm(a, stringField(authObj, "realm"))
	return a, nil
}

// authObjects 兼容嵌套形与扁平形：{auth,account} 或顶层 accessToken/uid。
func authObjects(doc map[string]any) (map[string]any, map[string]any) {
	authObj := object(doc["auth"])
	if authObj == nil && doc["accessToken"] != nil {
		authObj = doc
	}
	accountObj := object(doc["account"])
	if accountObj == nil && doc["uid"] != nil {
		accountObj = doc
	}
	return authObj, accountObj
}

// keyFromSecret 空串表示明文凭据（不要求密钥）。
func keyFromSecret(secret string) (keyMaterial, error) {
	if strings.TrimSpace(secret) == "" {
		return keyMaterial{}, nil
	}
	return deriveKey(secret)
}

// buildAuth 解密 token 并映射为网关 Auth。
func buildAuth(doc, authObj, accountObj map[string]any, key keyMaterial) (*auth.Auth, error) {
	at, err := decryptField(authObj["accessToken"], key)
	if err != nil {
		return nil, fmt.Errorf("decrypt accessToken: %w", err)
	}
	rt, err := decryptField(authObj["refreshToken"], key)
	if err != nil {
		return nil, fmt.Errorf("decrypt refreshToken: %w", err)
	}
	if strings.TrimSpace(at) == "" || strings.TrimSpace(rt) == "" {
		return nil, errors.New("accessToken/refreshToken is empty")
	}
	a := &auth.Auth{
		AccessToken:  at,
		RefreshToken: rt,
		Domain:       stringField(authObj, "domain"),
		UID:          stringField(accountObj, "uid"),
		EnterpriseID: stringField(accountObj, "enterpriseId"),
		Nickname:     stringField(accountObj, "nickname"),
		ExpiresAt:    authExpiry(authObj),
		DeviceToken:  deviceToken(doc, key),
	}
	if a.Nickname == "" {
		a.Nickname = stringField(accountObj, "email")
	}
	return a, nil
}

func authExpiry(authObj map[string]any) int64 {
	for _, k := range []string{"expiresAt", "expires_at"} {
		if n, ok := int64Value(authObj[k]); ok {
			return n
		}
	}
	return time.Now().Add(365 * 24 * time.Hour).Unix()
}

func deviceToken(doc map[string]any, key keyMaterial) string {
	for _, k := range []string{"device_token", "deviceToken"} {
		if v, err := decryptField(doc[k], key); err == nil && v != "" {
			return v
		}
	}
	return ""
}

func applyRealm(a *auth.Auth, explicit string) {
	if explicit != "" {
		_, _ = auth.BackfillRealmFor(a, explicit)
		return
	}
	_, _ = a.BackfillRealm()
}

// keyMaterial 派生后的 AES key 与 keyId（不导出，避免误打印）。
type keyMaterial struct {
	key   []byte
	keyID string
}

func deriveKey(secret string) (keyMaterial, error) {
	secret = strings.TrimSpace(secret)
	raw, err := base64.StdEncoding.DecodeString(secret)
	if err != nil || len(raw) != 32 || base64.StdEncoding.EncodeToString(raw) != secret {
		return keyMaterial{}, errors.New("invalid at-rest secret: expected canonical base64 of 32 bytes")
	}
	sum := sha256.Sum256([]byte(secret))
	keyIDSum := sha256.Sum256(sum[:])
	return keyMaterial{key: sum[:], keyID: hex.EncodeToString(keyIDSum[:])[:16]}, nil
}

// encryptedEnvelope 客户端 envelope（suite 1）。
type encryptedEnvelope struct {
	Suite      int    `json:"suite"`
	KeyID      string `json:"keyId"`
	Nonce      string `json:"nonce"`
	AuthTag    string `json:"authTag"`
	Ciphertext string `json:"ciphertext"`
}

func isEncrypted(raw any) bool {
	m := object(raw)
	if m == nil {
		return false
	}
	_, ok := m["$wbEncrypted"]
	return ok
}

func decryptField(raw any, key keyMaterial) (string, error) {
	switch v := raw.(type) {
	case nil:
		return "", nil
	case string:
		return v, nil
	case map[string]any:
		return openWrapper(v, key)
	default:
		return "", fmt.Errorf("unexpected token value %T", raw)
	}
}

func openWrapper(m map[string]any, key keyMaterial) (string, error) {
	if v, ok := int64Value(m["$wbEncrypted"]); !ok || v != 1 {
		return "", errors.New("unsupported $wbEncrypted version")
	}
	if scheme, _ := m["scheme"].(string); scheme != "" {
		return "", fmt.Errorf("unsupported encryption scheme %q (only sym-v1)", scheme)
	}
	envB64, _ := m["envelope"].(string)
	if envB64 == "" {
		return "", errors.New("encrypted wrapper missing envelope")
	}
	if key.keyID == "" {
		return "", errors.New("encrypted credential needs at-rest secret")
	}
	env, nonce, tag, ct, err := parseEnvelope(envB64)
	if err != nil {
		return "", err
	}
	if env.KeyID != key.keyID {
		return "", errors.New("at-rest secret does not match this credential")
	}
	return decryptAESGCM(key.key, nonce, tag, ct, fieldAAD(key.keyID))
}

// parseEnvelope 校验 wrapper envelope 的 canonical base64 与固定长度字段。
func parseEnvelope(envB64 string) (encryptedEnvelope, []byte, []byte, []byte, error) {
	var env encryptedEnvelope
	envRaw, err := base64.StdEncoding.DecodeString(envB64)
	if err != nil || base64.StdEncoding.EncodeToString(envRaw) != envB64 {
		return env, nil, nil, nil, errors.New("envelope is not canonical base64")
	}
	if uerr := json.Unmarshal(envRaw, &env); uerr != nil {
		return env, nil, nil, nil, fmt.Errorf("invalid envelope: %w", uerr)
	}
	if env.Suite != 1 {
		return env, nil, nil, nil, fmt.Errorf("unsupported envelope suite %d", env.Suite)
	}
	nonce, err := decodeCanonical(env.Nonce)
	if err != nil || len(nonce) != 12 {
		return env, nil, nil, nil, errors.New("invalid envelope nonce")
	}
	tag, err := decodeCanonical(env.AuthTag)
	if err != nil || len(tag) != 16 {
		return env, nil, nil, nil, errors.New("invalid envelope authTag")
	}
	ct, err := decodeCanonical(env.Ciphertext)
	if err != nil {
		return env, nil, nil, nil, errors.New("invalid envelope ciphertext")
	}
	return env, nonce, tag, ct, nil
}

func decryptAESGCM(key, nonce, tag, ct, aad []byte) (string, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	pt, err := gcm.Open(nil, nonce, append(append([]byte(nil), ct...), tag...), aad)
	if err != nil {
		return "", errors.New("decrypt failed: wrong key or corrupted credential")
	}
	return string(pt), nil
}

func decodeCanonical(s string) ([]byte, error) {
	raw, err := base64.StdEncoding.DecodeString(s)
	if err != nil || base64.StdEncoding.EncodeToString(raw) != s {
		return nil, errors.New("not canonical base64")
	}
	return raw, nil
}

// fieldAAD 复刻客户端 buildAuthenticatedContextAad（sym-v1, framing=field）：
// WB-AAD\0 | version=1 | "WBEV1" | "sym-v1" | suite=1 | keyId | framing=2 | no-seq | final=0。
func fieldAAD(keyID string) []byte {
	var b bytes.Buffer
	b.WriteString("WB-AAD\x00")
	b.WriteByte(1)
	writeLP(&b, "WBEV1")
	writeLP(&b, "sym-v1")
	writeU32(&b, 1)
	writeLP(&b, keyID)
	b.WriteByte(2)
	b.WriteByte(0)
	b.WriteByte(0)
	return b.Bytes()
}

func writeU32(b *bytes.Buffer, v uint32) {
	var raw [4]byte
	binary.BigEndian.PutUint32(raw[:], v)
	b.Write(raw[:])
}

func writeLP(b *bytes.Buffer, s string) {
	if uint64(len(s)) > uint64(^uint32(0)) {
		panic("aad component too long")
	}
	writeU32(b, uint32(len(s))) //nolint:gosec // 长度已在上方校验
	b.WriteString(s)
}

func readDoc(path string) (map[string]any, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var doc map[string]any
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("parse auth file: %w", err)
	}
	return doc, nil
}

func object(raw any) map[string]any {
	m, _ := raw.(map[string]any)
	return m
}

func stringField(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	s, _ := m[key].(string)
	return s
}

func int64Value(raw any) (int64, bool) {
	switch v := raw.(type) {
	case json.Number:
		n, err := v.Int64()
		return n, err == nil
	case float64:
		return int64(v), true
	case int64:
		return v, true
	case string:
		n, err := strconv.ParseInt(v, 10, 64)
		return n, err == nil
	default:
		return 0, false
	}
}
