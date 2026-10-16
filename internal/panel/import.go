package panel

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
)

// cockpitAccount 映射 cockpit tools 导出格式的单个账号。
type cockpitAccount struct {
	ID             string `json:"id"`
	Email          string `json:"email"`
	UID            string `json:"uid"`
	Nickname       string `json:"nickname"`
	AccessToken    string `json:"access_token"`
	RefreshToken   string `json:"refresh_token"`
	TokenType      string `json:"token_type"`
	ExpiresAt      int64  `json:"expires_at"`
	Domain         string `json:"domain"`
	DosageNotify   string `json:"dosage_notify_code"`
	PaymentType    string `json:"payment_type"`
	Status         string `json:"status"`
	UsageUpdatedAt int64  `json:"usage_updated_at"`
	LastCheckin    int64  `json:"last_checkin_time"`
	CheckinStreak  int    `json:"checkin_streak"`
	CreatedAt      int64  `json:"created_at"`
	LastUsed       int64  `json:"last_used"`
}

// importCockpit 接收 cockpit tools 导出的 JSON 文件，批量导入账号到池中。
//
//	POST /panel/api/import/cockpit
//	Content-Type: multipart/form-data
//	Body: file=<json>
//
// 返回 {ok, total, imported, skipped, errors}。
func (p *Panel) importCockpit(w http.ResponseWriter, r *http.Request) {
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

	var accounts []cockpitAccount
	if err := json.Unmarshal(raw, &accounts); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	if len(accounts) == 0 {
		writeErr(w, http.StatusBadRequest, "empty accounts array")
		return
	}

	var total, imported, skipped int
	var errs []string

	for _, acc := range accounts {
		ok, msg := p.importOneCockpitAccount(acc)
		if ok {
			imported++
			continue
		}
		skipped++
		errs = append(errs, msg)
	}

	total = len(accounts)
	log.Printf("panel: cockpit import finished total=%d imported=%d skipped=%d", total, imported, skipped)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"total":    total,
		"imported": imported,
		"skipped":  skipped,
		"errors":   errs,
	})
}

// importOneCockpitAccount 导入单个 cockpit 账号：字段校验、落盘、热加载并顺带
// 签到/激活。返回 (是否导入成功, 失败原因)；失败原因与旧实现逐字一致。
func (p *Panel) importOneCockpitAccount(acc cockpitAccount) (bool, string) {
	uid := strings.TrimSpace(acc.UID)
	at := strings.TrimSpace(acc.AccessToken)
	rt := strings.TrimSpace(acc.RefreshToken)
	if uid == "" || at == "" || rt == "" {
		return false, fmt.Sprintf("missing required fields (id=%s)", acc.ID)
	}
	if !validImportUID(uid) {
		return false, fmt.Sprintf("invalid uid (id=%s)", acc.ID)
	}

	// 按 domain 推断 realm：workbuddy.ai 家族 → global，否则 cn。
	realm := auth.ResolveRealm("", acc.Domain)
	a := buildImportedAuth(acc, uid, at, rt, p.cfg.AuthDir)
	if err := p.persistImportedAuth(a, realm, uid); err != nil {
		return false, err.Error()
	}

	// 顺带签到/激活（幂等；失败仅记日志，不阻断导入）。
	p.importCheckin(a, uid)
	if rm, tt, err := p.cfg.Upstream.UserResource(a); err == nil {
		p.cfg.Pool.ReenableIfCredits(uid, rm, tt)
	}
	return true, ""
}

// buildImportedAuth 由 cockpit 账号构造 auth：expires_at 毫秒转秒（缺省 +365 天），
// nickname 缺失时回落 email。
func buildImportedAuth(acc cockpitAccount, uid, at, rt, authDir string) *auth.Auth {
	// cockpit tools 的 expires_at 为毫秒时间戳，转为秒。
	expiresAt := acc.ExpiresAt / 1000
	if expiresAt <= 0 {
		expiresAt = time.Now().Add(365 * 24 * time.Hour).Unix()
	}
	nickname := acc.Nickname
	if strings.TrimSpace(nickname) == "" {
		nickname = acc.Email
	}
	return &auth.Auth{
		AccessToken:  at,
		RefreshToken: rt,
		ExpiresAt:    expiresAt,
		Domain:       acc.Domain,
		UID:          uid,
		Nickname:     nickname,
		FilePath:     filepath.Join(authDir, fmt.Sprintf("workbuddy-%s.json", uid)),
	}
}

// persistImportedAuth 按 realm 补 realm 键、落盘并热加载进池；失败返回与旧实现
// 逐字一致的原因串（调用方计入 skipped）。
func (p *Panel) persistImportedAuth(a *auth.Auth, realm, uid string) error {
	if realm == "global" {
		if _, err := auth.BackfillRealmFor(a, "global"); err != nil {
			return fmt.Errorf("uid=%s: set realm failed: %w", uid, err)
		}
	} else {
		_, _ = a.BackfillRealm()
	}
	if err := a.SaveAtomic(); err != nil {
		return fmt.Errorf("uid=%s: save auth failed: %w", uid, err)
	}
	p.cfg.Pool.Add(a)
	p.cfg.Pool.Revive(uid)
	return nil
}

// importCheckin 导入后的签到/激活（幂等；失败仅记日志，不阻断导入）。realm 取落盘
// 后的 a.Realm()，与 persistImportedAuth 的 BackfillRealm/iss 兜底结果一致，避免
// 本地按 domain 推断的 realm 与最终写入的 realm 分叉而漏掉 global 激活/trial。
func (p *Panel) importCheckin(a *auth.Auth, uid string) {
	if a.Realm() == "global" {
		p.importGlobalActivation(a, uid)
		return
	}
	if err := p.cfg.Upstream.DailyCheckin(a); err != nil {
		log.Printf("panel: import checkin uid=%s: %v", uid, err)
	}
}

// importGlobalActivation global 导入后的注册激活 + trial 领取（幂等；失败仅记日志）。
func (p *Panel) importGlobalActivation(a *auth.Auth, uid string) {
	if activated, err := p.cfg.Upstream.GlobalCompleteRegistration(a); err != nil {
		log.Printf("panel: import global 注册激活 uid=%s: %v", uid, err)
	} else if activated {
		log.Printf("panel: import global 注册激活 uid=%s 完成", uid)
	}
	if claimed, err := p.cfg.Upstream.ClaimTrial(a); err != nil {
		log.Printf("panel: import global trial uid=%s: %v", uid, err)
	} else if claimed {
		log.Printf("panel: import global trial uid=%s 已领", uid)
	}
}

// validImportUID 校验导入 uid 是否可用于拼文件名（同 login.go validUID 口径）。
func validImportUID(uid string) bool {
	if uid == "" || len(uid) > 64 {
		return false
	}
	for _, c := range uid {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_':
		default:
			return false
		}
	}
	return true
}
