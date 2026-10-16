package panel

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
)

// 加密账号备份（issue #30）：AES-256-GCM + PBKDF2-HMAC-SHA256 派生密钥，每次导出
// 随机 salt。密码不写入导出文件。
//
// KDF 选择：issue 建议 scrypt，但 scrypt 不在 Go 标准库；本仓库优先零新依赖（KISS），
// 故用 Go 1.24+ 标准库 crypto/pbkdf2（HMAC-SHA256，高迭代）作等价密码 KDF。
const (
	secureEnvelopeVersion = 1
	secureKDF             = "pbkdf2-sha256"
	secureIterations      = 600_000
	secureSaltLen         = 16
	// secureMaxIter 解密侧上限：拒绝畸形文件声明的超大迭代（防 CPU DoS）。
	secureMaxIter = 5_000_000
	secureMinPass = 8
	// secureMaxUploadBytes 加密导入请求体上限（防无界读；正常导出远小于此）。
	// ParseMultipartForm 的 maxMemory 只决定内存/临时文件切分，不限制大小，
	// 故在解析前用 http.MaxBytesReader 兜住整个请求体。
	secureMaxUploadBytes = 8 << 20
)

// errSecureDecrypt 通用解密失败：不区分「密码错」与「文件损坏」（避免侧信道）。
var errSecureDecrypt = errors.New("解密失败：密码错误或文件损坏")

// secureAccount 加密备份中的单个账号（含可恢复登录态的 token）。
type secureAccount struct {
	UID          string `json:"uid"`
	Nickname     string `json:"nickname,omitempty"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresAt    int64  `json:"expires_at"` // Unix 秒
	Domain       string `json:"domain,omitempty"`
	Realm        string `json:"realm,omitempty"`
	EnterpriseID string `json:"enterprise_id,omitempty"`
	DeviceToken  string `json:"device_token,omitempty"`
}

// secureEnvelope 加密备份外层（明文头 + base64 密文）。
type secureEnvelope struct {
	Version int    `json:"v"`
	KDF     string `json:"kdf"`
	Iter    int    `json:"iter"`
	Salt    string `json:"salt"`
	Nonce   string `json:"nonce"`
	Cipher  string `json:"ct"`
}

// encryptPayload 用 password 加密 plaintext，返回 envelope JSON。
func encryptPayload(password string, plaintext []byte) ([]byte, error) {
	salt := make([]byte, secureSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return nil, err
	}
	gcm, err := secureGCM(password, salt, secureIterations)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	ct := gcm.Seal(nil, nonce, plaintext, nil)
	env := secureEnvelope{
		Version: secureEnvelopeVersion,
		KDF:     secureKDF,
		Iter:    secureIterations,
		Salt:    base64.StdEncoding.EncodeToString(salt),
		Nonce:   base64.StdEncoding.EncodeToString(nonce),
		Cipher:  base64.StdEncoding.EncodeToString(ct),
	}
	return json.Marshal(env)
}

// decryptPayload 校验 envelope 并解密；任何失败（含 GCM auth tag 校验失败）统一返回
// errSecureDecrypt，不泄露密码正确性。
func decryptPayload(password string, raw []byte) ([]byte, error) {
	env, err := parseSecureEnvelope(raw)
	if err != nil {
		return nil, err
	}
	salt, e1 := base64.StdEncoding.DecodeString(env.Salt)
	nonce, e2 := base64.StdEncoding.DecodeString(env.Nonce)
	ct, e3 := base64.StdEncoding.DecodeString(env.Cipher)
	if e1 != nil || e2 != nil || e3 != nil {
		return nil, errSecureDecrypt
	}
	gcm, err := secureGCM(password, salt, env.Iter)
	if err != nil {
		return nil, err
	}
	pt, err := gcm.Open(nil, nonce, ct, nil)
	if err != nil {
		return nil, errSecureDecrypt
	}
	return pt, nil
}

// parseSecureEnvelope 解析并校验 envelope 头（版本/KDF/迭代上限）。
func parseSecureEnvelope(raw []byte) (secureEnvelope, error) {
	var env secureEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return env, errSecureDecrypt
	}
	if env.Version != secureEnvelopeVersion || env.KDF != secureKDF ||
		env.Iter <= 0 || env.Iter > secureMaxIter {
		return env, errSecureDecrypt
	}
	return env, nil
}

// secureGCM 由密码 + salt 派生密钥并构造 AES-256-GCM AEAD。
func secureGCM(password string, salt []byte, iter int) (cipher.AEAD, error) {
	key, err := pbkdf2.Key(sha256.New, password, salt, iter, 32)
	if err != nil {
		return nil, errSecureDecrypt
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, errSecureDecrypt
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, errSecureDecrypt
	}
	return gcm, nil
}

// collectSecureAccounts 汇总池内全部账号的可导出凭证（token 原样，加密后传输）。
// 经加锁访问器读取会被 refresh 改写的字段；UID/Nickname/EnterpriseID/DeviceToken
// 解析后不再改写，直接读。
func (p *Panel) collectSecureAccounts() []secureAccount {
	var out []secureAccount
	for _, st := range p.cfg.Pool.List() {
		a := p.cfg.Pool.AuthByUID(st.UID)
		if a == nil {
			continue
		}
		out = append(out, secureAccount{
			UID:          a.UID,
			Nickname:     a.Nickname,
			AccessToken:  a.AccessTokenValue(),
			RefreshToken: a.RefreshTokenValue(),
			ExpiresAt:    a.ExpiresAtValue(),
			Domain:       a.DomainValue(),
			Realm:        a.Realm(),
			EnterpriseID: a.EnterpriseID,
			DeviceToken:  a.DeviceToken,
		})
	}
	return out
}

// exportEncrypted 加密导出池内全部账号。
//
//	POST /panel/api/export/encrypted
//	Body: {"password":"..."}
//	→ 加密 envelope JSON（Content-Disposition 附件）。
func (p *Panel) exportEncrypted(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	if len(req.Password) < secureMinPass {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf("密码至少 %d 位", secureMinPass))
		return
	}
	accounts := p.collectSecureAccounts()
	if len(accounts) == 0 {
		writeErr(w, http.StatusBadRequest, "池内无账号可导出")
		return
	}
	plain, err := json.Marshal(accounts)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "序列化失败")
		return
	}
	env, err := encryptPayload(req.Password, plain)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "加密失败")
		return
	}
	name := "wokbuddy-accounts-" + time.Now().Format("20060102-150405") + ".wbenc.json"
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	_, _ = w.Write(env)
	log.Printf("panel: 加密导出 %d 个账号（明文 token 未落日志）", len(accounts))
}

// importEncrypted 解密导入账号。
//
//	POST /panel/api/import/encrypted
//	Content-Type: multipart/form-data
//	Body: file=<envelope json>, password=<...>
//
// 返回 {ok, total, imported, skipped, errors}。
func (p *Panel) importEncrypted(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, secureMaxUploadBytes)
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		writeErr(w, http.StatusBadRequest, "parse form: "+err.Error())
		return
	}
	file, _, err := r.FormFile("file")
	if err != nil {
		writeErr(w, http.StatusBadRequest, "missing file field: "+err.Error())
		return
	}
	defer file.Close()
	raw, err := io.ReadAll(file)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "read file: "+err.Error())
		return
	}
	password := r.FormValue("password")
	if password == "" {
		writeErr(w, http.StatusBadRequest, "missing password")
		return
	}
	plain, err := decryptPayload(password, raw)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	var accounts []secureAccount
	if err := json.Unmarshal(plain, &accounts); err != nil {
		writeErr(w, http.StatusBadRequest, "解密后内容不是有效账号数组")
		return
	}
	if len(accounts) == 0 {
		writeErr(w, http.StatusBadRequest, "账号数组为空")
		return
	}

	var imported, skipped int
	var errs []string
	for _, acc := range accounts {
		if ok, msg := p.importSecureAccount(acc); ok {
			imported++
		} else {
			skipped++
			errs = append(errs, msg)
		}
	}
	log.Printf("panel: 加密导入 finished total=%d imported=%d skipped=%d", len(accounts), imported, skipped)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"total":    len(accounts),
		"imported": imported,
		"skipped":  skipped,
		"errors":   errs,
	})
}

// importSecureAccount 导入单个解密账号：字段校验、落盘、热加载并顺带签到/激活。
// 失败原因不回显 token（只标 uid）。
func (p *Panel) importSecureAccount(acc secureAccount) (bool, string) {
	uid := strings.TrimSpace(acc.UID)
	at := strings.TrimSpace(acc.AccessToken)
	rt := strings.TrimSpace(acc.RefreshToken)
	if uid == "" || at == "" || rt == "" {
		return false, fmt.Sprintf("missing required fields (uid=%q)", acc.UID)
	}
	if !validImportUID(uid) {
		return false, fmt.Sprintf("invalid uid (uid=%q)", acc.UID)
	}
	realm := strings.TrimSpace(acc.Realm)
	if realm == "" {
		realm = auth.ResolveRealm("", acc.Domain)
	}
	a := buildSecureAuth(acc, uid, at, rt, p.cfg.AuthDir)
	if err := p.persistImportedAuth(a, realm, uid); err != nil {
		return false, err.Error()
	}
	p.importCheckin(a, realm, uid)
	if rm, tt, err := p.cfg.Upstream.UserResource(a); err == nil {
		p.cfg.Pool.ReenableIfCredits(uid, rm, tt)
	}
	return true, ""
}

// buildSecureAuth 由解密账号构造 auth：ExpiresAt 缺失时回落 +365 天。
func buildSecureAuth(acc secureAccount, uid, at, rt, authDir string) *auth.Auth {
	expiresAt := acc.ExpiresAt
	if expiresAt <= 0 {
		expiresAt = time.Now().Add(365 * 24 * time.Hour).Unix()
	}
	return &auth.Auth{
		AccessToken:  at,
		RefreshToken: rt,
		ExpiresAt:    expiresAt,
		Domain:       acc.Domain,
		UID:          uid,
		Nickname:     acc.Nickname,
		EnterpriseID: acc.EnterpriseID,
		DeviceToken:  acc.DeviceToken,
		FilePath:     filepath.Join(authDir, fmt.Sprintf("workbuddy-%s.json", uid)),
	}
}
