// Package httpauth 网关与面板共用的 Bearer 鉴权原语。
//
// 单独成包的原因：server（/v1/*、/status）与 panel（/panel/api/*）两处鉴权
// 必须完全同口径——此前各自复制了一份"字符串直接比较"的实现，既容易漂移，
// 又都带计时侧信道。统一到这里后，口径只有一份，且天然常量时间比较。
package httpauth

import (
	"crypto/sha256"
	"crypto/subtle"
	"net/http"
	"strings"
)

// bearerPrefix 认证方案前缀（大小写敏感，与 HTTP 规范及既有实现一致）。
const bearerPrefix = "Bearer "

// VerifyBearer 校验请求头是否携带正确的 Bearer 密钥。
//
// key 为空表示"未启用鉴权"，恒返回 true（调用方据此放行）。
// 比较用 SHA-256 摘要 + subtle.ConstantTimeCompare：
//   - 常量时间，不因前缀匹配长度而泄露信息；
//   - 先摘要再比较，长度差异被吸收进摘要（不会因长度不同提前返回）；
//   - 摘要本身不可逆，即便有侧信道也拿不到密钥原文。
func VerifyBearer(r *http.Request, key string) bool {
	if key == "" {
		return true
	}
	// 缺头/方案不对时 bearerToken 返回空串，仍走一次摘要比较，保持耗时形状一致。
	return subtle.ConstantTimeCompare(digest(bearerToken(r)), digest(key)) == 1
}

// Key 一把命名密钥的最小校验视图：ID 用于回执与计数，Hash 是密钥的 SHA-256
// 摘要（定长 32 字节，多候选比较因此天然常量时间）。
type Key struct {
	ID   string
	Hash []byte
}

// AuthResult 一次多密钥鉴权的结果。
type AuthResult struct {
	OK        bool   // 是否通过
	KeyID     string // 命中的命名密钥 ID；默认密钥命中或未启用鉴权时为空串
	IsDefault bool   // 命中默认密钥（或未启用鉴权而放行）
}

// HashKey 返回密钥的 SHA-256 摘要（与 VerifyBearer 内部 digest 同口径），
// 供命名密钥库持久化与比较复用——比较原语只此一份，不另起实现。
func HashKey(key string) []byte { return digest(key) }

// Authenticate 校验请求头 Bearer 是否命中默认密钥或命名密钥集合。
//
// 语义：
//   - defaultKey 为空且 keys 为空 → 未启用鉴权，恒放行（OK=true, IsDefault=true）；
//   - 否则必须命中 defaultKey 或 keys 中任一未撤销密钥的摘要。
//
// 常量时间：presented token 先摘要为定长，再对默认密钥与**全部**候选逐条
// subtle.ConstantTimeCompare；不因命中位置提前返回，不泄露匹配前缀或候选数量形状。
func Authenticate(r *http.Request, defaultKey string, keys []Key) AuthResult {
	if defaultKey == "" && len(keys) == 0 {
		return AuthResult{OK: true, IsDefault: true, KeyID: ""}
	}

	got := digest(bearerToken(r))

	defaultMatch := 0
	if defaultKey != "" {
		defaultMatch = subtle.ConstantTimeCompare(got, digest(defaultKey))
	}

	matched := -1

	for i := range keys {
		eq := subtle.ConstantTimeCompare(got, keys[i].Hash)
		// 命中后仍继续比较其余候选，保持恒定遍历（不短路）。
		if eq == 1 && matched < 0 {
			matched = i
		}
	}

	if defaultMatch == 1 {
		return AuthResult{OK: true, IsDefault: true, KeyID: ""}
	}

	if matched >= 0 {
		return AuthResult{OK: true, KeyID: keys[matched].ID, IsDefault: false}
	}

	return AuthResult{OK: false, KeyID: "", IsDefault: false}
}

// bearerToken 从 Authorization 头提取 Bearer token；缺头或方案不符返回空串
// （调用方仍会摘要后参与比较，保持耗时形状一致）。
func bearerToken(r *http.Request) string {
	authz := r.Header.Get("Authorization")
	if !strings.HasPrefix(authz, bearerPrefix) {
		return ""
	}

	return authz[len(bearerPrefix):]
}

// digest 返回 s 的 SHA-256（定长 32 字节，供常量时间比较）。
func digest(s string) []byte {
	sum := sha256.Sum256([]byte(s))
	return sum[:]
}
