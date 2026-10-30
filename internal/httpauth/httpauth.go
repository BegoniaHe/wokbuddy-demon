// Package httpauth 网关与面板共用的 Bearer / X-Api-Key 鉴权原语。
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

// HashKey 返回密钥的 SHA-256 摘要（与 Authenticate 内部 digest 同口径），
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

// Scope 一次鉴权命中的凭据作用域。ScopeNone 表示未命中任何候选。
type Scope int

const (
	ScopeNone Scope = iota
	// ScopeData 网关数据面凭据（默认 api_key / 命名密钥）。
	ScopeData
	// ScopeReadonly 管理面只读凭据。
	ScopeReadonly
	// ScopeAdmin 管理面完整凭据。
	ScopeAdmin
)

// ScopeResult 管理/运维面一次作用域鉴权的结果。
type ScopeResult struct {
	OK        bool
	Scope     Scope
	IsDefault bool // 命中默认密钥（或未配置管理凭据时未鉴权放行）
}

// managementCredential 管理面候选凭据（摘要 + 作用域）。
type managementCredential struct {
	hash      []byte
	scope     Scope
	isDefault bool
}

// AuthenticateManagement 是管理面与运维面统一的作用域鉴权入口。
//
// 候选集合：
//   - adminKey（admin）与 readonlyKey（readonly）任一非空时，只接受这两者；
//     默认 api_key 不再获得管理权限（readonly 单独配置时没有 admin 候选）。
//   - 两者都为空时回落到默认 api_key（ScopeAdmin）；默认也空则未鉴权放行。
//
// 常量时间口径与 Authenticate 一致：presented token 先摘要为定长，再对**全部**
// 候选逐条 subtle.ConstantTimeCompare，不因命中提前返回。admin/readonly 只在
// 这里参与比较，不会进入数据面 Authenticate 的候选集合。
func AuthenticateManagement(r *http.Request, defaultKey, adminKey, readonlyKey string) ScopeResult {
	candidates := managementCredentials(defaultKey, adminKey, readonlyKey)
	if len(candidates) == 0 {
		return ScopeResult{OK: true, Scope: ScopeAdmin, IsDefault: true}
	}

	got := digest(bearerToken(r))
	matched := -1

	for i := range candidates {
		eq := subtle.ConstantTimeCompare(got, candidates[i].hash)
		if eq == 1 && matched < 0 {
			matched = i
		}
	}

	if matched < 0 {
		return ScopeResult{OK: false, Scope: ScopeNone}
	}

	hit := candidates[matched]

	return ScopeResult{OK: true, Scope: hit.scope, IsDefault: hit.isDefault}
}

// managementCredentials 构造管理面候选凭据；返回空表示未启用鉴权。
func managementCredentials(defaultKey, adminKey, readonlyKey string) []managementCredential {
	if adminKey == "" && readonlyKey == "" {
		if defaultKey == "" {
			return nil
		}

		return []managementCredential{{hash: digest(defaultKey), scope: ScopeAdmin, isDefault: true}}
	}

	out := make([]managementCredential, 0, 2)
	if adminKey != "" {
		out = append(out, managementCredential{hash: digest(adminKey), scope: ScopeAdmin})
	}

	if readonlyKey != "" {
		out = append(out, managementCredential{hash: digest(readonlyKey), scope: ScopeReadonly})
	}

	return out
}

// bearerToken 从 Authorization 头提取 Bearer token；缺头或方案不符时回退到
// Anthropic 客户端使用的 X-Api-Key 头。两者都缺失返回空串（调用方仍会摘要后
// 参与比较，保持耗时形状一致）。
func bearerToken(r *http.Request) string {
	authz := r.Header.Get("Authorization")
	if strings.HasPrefix(authz, bearerPrefix) {
		return authz[len(bearerPrefix):]
	}

	// Anthropic SDK 在 api_key 模式下只发 x-api-key；Gemini CLI/SDK 发
	// x-goog-api-key。与 Bearer 同口径比较，不新增第二条鉴权路径。
	if key := strings.TrimSpace(r.Header.Get("X-Api-Key")); key != "" {
		return key
	}

	return strings.TrimSpace(r.Header.Get("X-Goog-Api-Key"))
}

// digest 返回 s 的 SHA-256（定长 32 字节，供常量时间比较）。
func digest(s string) []byte {
	sum := sha256.Sum256([]byte(s))
	return sum[:]
}
