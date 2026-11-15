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
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
)

// Account 扫描结果：只含面板展示所需元数据，不含 token。
var (
	errAuthObjectMissing                    = errors.New("auth object missing")
	errAccessTokenRefreshTokenIsEmpty       = errors.New("accessToken/refreshToken is empty")
	errInvalidAtRestSecretExpectedCanonical = errors.New(
		"invalid at-rest secret: expected canonical base64 of 32 bytes",
	)
	errDecryptFieldFailed                   = errors.New("decryptField failed")
	errUnsupportedWBEncryptedVersion        = errors.New("unsupported $wbEncrypted version")
	errOpenWrapperFailed                    = errors.New("openWrapper failed")
	errEncryptedWrapperMissingEnvelope      = errors.New("encrypted wrapper missing envelope")
	errEncryptedCredentialNeedsAtRestSecret = errors.New("encrypted credential needs at-rest secret")
	errAtRestSecretDoesNotMatch             = errors.New("at-rest secret does not match this credential")
	errEnvelopeIsNotCanonicalBase64         = errors.New("envelope is not canonical base64")
	errParseEnvelopeFailed                  = errors.New("parseEnvelope failed")
	errInvalidEnvelopeNonce                 = errors.New("invalid envelope nonce")
	errInvalidEnvelopeAuthTag               = errors.New("invalid envelope authTag")
	errInvalidEnvelopeCiphertext            = errors.New("invalid envelope ciphertext")
	errDecryptFailedWrongKeyOrCorrupted     = errors.New("decrypt failed: wrong key or corrupted credential")
	errNotCanonicalBase64                   = errors.New("not canonical base64")
	errAADComponentTooLong                  = errors.New("aad component too long")
)

type Account struct {
	Path      string `json:"path"`
	UID       string `json:"uid"`
	Nickname  string `json:"nickname,omitempty"`
	Domain    string `json:"domain,omitempty"`
	Realm     string `json:"realm"`
	ExpiresAt int64  `json:"expires_at,omitempty"`
	Encrypted bool   `json:"encrypted"`
}

// baseDirs 返回当前平台的基础数据目录（无匹配平台时为空）。
func baseDirs(home string) []string {
	switch runtime.GOOS {
	case "darwin":
		return []string{filepath.Join(home, "Library", "Application Support")}
	case "windows":
		if local := os.Getenv("LOCALAPPDATA"); local != "" {
			return []string{local}
		}
	case "linux":
		return []string{filepath.Join(home, ".local", "share"), filepath.Join(home, ".config")}
	}

	return nil
}

// Roots 返回各平台桌面客户端的候选数据目录（只读扫描）。
func Roots() []string {
	// home 取不到时不能继续：空串会让 filepath.Join 产出相对路径，glob 相对进程
	// CWD 解析，可能扫到无关目录。
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return nil
	}

	bases := baseDirs(home)

	names := []string{"WorkBuddy", "WorkBuddy AI"}
	// 只接受单段目录名：filepath.Join(base, "../..") 会经 Clean 逃出 base，扫到的
	// 路径又会进入面板的可导入集合。需要控制进程环境才可利用，但仍按最小权限拒绝。
	if v := strings.TrimSpace(os.Getenv("WORKBUDDY_APP_NAME")); v != "" &&
		v != "." && v != ".." && !strings.ContainsAny(v, `/\`) {
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

	slices.Sort(out)

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

	account := Account{Path: path, UID: "", Nickname: "", Domain: "", Realm: "", ExpiresAt: 0, Encrypted: false}
	if m := object(doc["auth"]); m != nil {
		account.Domain, _ = m["domain"].(string)
		// 与 ImportFile 同口径（authExpiry 内部先 expiresAt 后 expires_at，并做
		// 秒/毫秒归一）：同一份文件在两个入口必须给出同一个过期时间。先前这里
		// 顺序相反（expires_at 覆盖 expiresAt），两个键都存在时面板显示的是导入
		// 拿不到的另一个时刻。
		account.ExpiresAt = authExpiry(m)

		if s, ok := m["realm"].(string); ok {
			account.Realm = s
		}

		account.Encrypted = isEncrypted(m["accessToken"]) || isEncrypted(m["refreshToken"])
	}

	if m := object(doc["account"]); m != nil {
		account.UID, _ = m["uid"].(string)
		account.Nickname, _ = m["nickname"].(string)
	}

	account.Realm = auth.ResolveRealm(account.Realm, account.Domain)

	return account, nil
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
		return nil, errAuthObjectMissing
	}

	key, err := keyFromSecret(secret)
	if err != nil {
		return nil, err
	}

	account, err := buildAuth(doc, authObj, accountObj, key)
	if err != nil {
		return nil, err
	}

	applyRealm(account, stringField(authObj, "realm"))

	return account, nil
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

	str, err := decryptField(authObj["refreshToken"], key)
	if err != nil {
		return nil, fmt.Errorf("decrypt refreshToken: %w", err)
	}

	if strings.TrimSpace(at) == "" || strings.TrimSpace(str) == "" {
		return nil, errAccessTokenRefreshTokenIsEmpty
	}

	account := &auth.Auth{
		AccessToken:        at,
		RefreshToken:       str,
		Domain:             stringField(authObj, "domain"),
		UID:                stringField(accountObj, "uid"),
		EnterpriseID:       stringField(accountObj, "enterpriseId"),
		EditionType:        stringField(accountObj, "type"),
		IDSource:           stringField(accountObj, "idSource"),
		DepartmentFullName: stringField(accountObj, "departmentFullName"),
		Nickname:           stringField(accountObj, "nickname"),
		ExpiresAt:          authExpiry(authObj),
		DeviceToken:        deviceToken(doc, key),
	}
	if account.Nickname == "" {
		account.Nickname = stringField(accountObj, "email")
	}

	return account, nil
}

// authExpiry 取客户端文件里的过期时间；缺失返回 0（= 未知）。
//
// 曾返回「一年后」：客户端文件没写过期时间时，账号会被当成长期有效，
// NeedsRefresh 恒 false，保活与调度器的主动刷新全部跳过；真实 AT 通常几天即失效，
// 只能等请求 401 后的被动恢复。0 让 NeedsRefresh 视为需刷新，由上游 expiresIn 回填。
func authExpiry(authObj map[string]any) int64 {
	for _, k := range []string{"expiresAt", "expires_at"} {
		if n, ok := int64Value(authObj[k]); ok {
			return auth.NormalizeExpiresAt(n)
		}
	}

	return 0
}

func deviceToken(doc map[string]any, key keyMaterial) string {
	for _, k := range []string{"device_token", "deviceToken"} {
		if v, err := decryptField(doc[k], key); err == nil && v != "" {
			return v
		}
	}

	return ""
}

func applyRealm(account *auth.Auth, explicit string) {
	if explicit != "" {
		_, _ = auth.BackfillRealmFor(account, explicit)
		return
	}

	_, _ = account.BackfillRealm()
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
		return keyMaterial{}, errInvalidAtRestSecretExpectedCanonical
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
	switch item := raw.(type) {
	case nil:
		return "", nil
	case string:
		return item, nil
	case map[string]any:
		return openWrapper(item, key)
	default:
		return "", fmt.Errorf("unexpected token value %T: %w", raw, errDecryptFieldFailed)
	}
}

func openWrapper(m map[string]any, key keyMaterial) (string, error) {
	if v, ok := int64Value(m["$wbEncrypted"]); !ok || v != 1 {
		return "", errUnsupportedWBEncryptedVersion
	}

	if scheme, _ := m["scheme"].(string); scheme != "" {
		return "", fmt.Errorf("unsupported encryption scheme %q (only sym-v1): %w", scheme, errOpenWrapperFailed)
	}

	envB64, _ := m["envelope"].(string)
	if envB64 == "" {
		return "", errEncryptedWrapperMissingEnvelope
	}

	if key.keyID == "" {
		return "", errEncryptedCredentialNeedsAtRestSecret
	}

	encryptedEnvelope3, nonce, tag, nums, err := parseEnvelope(envB64)
	if err != nil {
		return "", err
	}

	if encryptedEnvelope3.KeyID != key.keyID {
		return "", errAtRestSecretDoesNotMatch
	}

	aad, err := fieldAAD(key.keyID)
	if err != nil {
		return "", err
	}

	return decryptAESGCM(key.key, nonce, tag, nums, aad)
}

// parseEnvelope 校验 wrapper envelope 的 canonical base64 与固定长度字段。
func parseEnvelope(envB64 string) (encryptedEnvelope, []byte, []byte, []byte, error) {
	var env encryptedEnvelope

	envRaw, err := base64.StdEncoding.DecodeString(envB64)
	if err != nil || base64.StdEncoding.EncodeToString(envRaw) != envB64 {
		return env, nil, nil, nil, errEnvelopeIsNotCanonicalBase64
	}

	if uerr := json.Unmarshal(envRaw, &env); uerr != nil {
		return env, nil, nil, nil, fmt.Errorf("invalid envelope: %w", uerr)
	}

	if env.Suite != 1 {
		return env, nil, nil, nil, fmt.Errorf("unsupported envelope suite %d: %w", env.Suite, errParseEnvelopeFailed)
	}

	nonce, err := decodeCanonical(env.Nonce)
	if err != nil || len(nonce) != 12 {
		return env, nil, nil, nil, errInvalidEnvelopeNonce
	}

	tag, err := decodeCanonical(env.AuthTag)
	if err != nil || len(tag) != 16 {
		return env, nil, nil, nil, errInvalidEnvelopeAuthTag
	}

	ct, err := decodeCanonical(env.Ciphertext)
	if err != nil {
		return env, nil, nil, nil, errInvalidEnvelopeCiphertext
	}

	return env, nonce, tag, ct, nil
}

func decryptAESGCM(numsVal, nonce, tag, nums, aad []byte) (string, error) {
	block, err := aes.NewCipher(numsVal)
	if err != nil {
		return "", fmt.Errorf("decryptAESGCM: %w", err)
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("decryptAESGCM: %w", err)
	}

	pt, err := gcm.Open(nil, nonce, append(append([]byte(nil), nums...), tag...), aad)
	if err != nil {
		return "", errDecryptFailedWrongKeyOrCorrupted
	}

	return string(pt), nil
}

func decodeCanonical(s string) ([]byte, error) {
	raw, err := base64.StdEncoding.DecodeString(s)
	if err != nil || base64.StdEncoding.EncodeToString(raw) != s {
		return nil, errNotCanonicalBase64
	}

	return raw, nil
}

// fieldAAD 复刻客户端 buildAuthenticatedContextAad（sym-v1, framing=field）：
// WB-AAD\0 | version=1 | "WBEV1" | "sym-v1" | suite=1 | keyId | framing=2 | no-seq | final=0。
func fieldAAD(keyID string) ([]byte, error) {
	if uint64(len(keyID)) > uint64(^uint32(0)) {
		return nil, errAADComponentTooLong
	}

	var buffer bytes.Buffer
	buffer.WriteString("WB-AAD\x00")
	buffer.WriteByte(1)
	writeLP(&buffer, "WBEV1")
	writeLP(&buffer, "sym-v1")
	writeU32(&buffer, 1)
	writeLP(&buffer, keyID)
	buffer.WriteByte(2)
	buffer.WriteByte(0)
	buffer.WriteByte(0)

	return buffer.Bytes(), nil
}

func writeU32(b *bytes.Buffer, v uint32) {
	var raw [4]byte
	binary.BigEndian.PutUint32(raw[:], v)
	b.Write(raw[:])
}

func writeLP(buffer *bytes.Buffer, str string) {
	// str 恒为固定常量或经 fieldAAD 校验的 keyID，长度必在 uint32 范围内。
	writeU32(buffer, uint32(len(str))) //nolint:gosec // 长度约束见 fieldAAD
	buffer.WriteString(str)
}

// maxAuthFileBytes 单个桌面客户端 auth 文件的大小上限。
//
// 文件路径来自目录扫描，内容不由本进程控制：被替换成巨型文件时 os.ReadFile 会整份
// 读进内存（面板导入路径会触发它），加一道上限把内存占用钉住。
const maxAuthFileBytes = 32 << 20

// errAuthFileTooLarge 单个 auth 文件超过读取上限。
var errAuthFileTooLarge = errors.New("auth file too large")

// errTrailingData auth 文件在合法 JSON 对象之后还有内容（被替换/损坏的形态）。
var errTrailingData = errors.New("auth file has trailing data")

func readDoc(path string) (map[string]any, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("readDoc: %w", err)
	}
	defer file.Close()

	raw, err := io.ReadAll(io.LimitReader(file, maxAuthFileBytes+1))
	if err != nil {
		return nil, fmt.Errorf("readDoc: %w", err)
	}

	if len(raw) > maxAuthFileBytes {
		return nil, fmt.Errorf("readDoc: %s 超过 %d 字节上限: %w",
			filepath.Base(path), maxAuthFileBytes, errAuthFileTooLarge)
	}

	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()

	var doc map[string]any
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("parse auth file: %w", err)
	}
	// 尾随内容守卫：被替换成「合法对象 + 垃圾尾巴」的文件不能当成有效凭据。
	if err := dec.Decode(new(any)); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("parse auth file: 存在尾随内容: %w", errTrailingData)
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
	switch item := raw.(type) {
	case json.Number:
		n, err := item.Int64()
		return n, err == nil
	case float64:
		return int64(item), true
	case int64:
		return item, true
	case string:
		n, err := strconv.ParseInt(item, 10, 64)
		return n, err == nil
	default:
		return 0, false
	}
}
