package panel

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
)

// 批量导入（cockpit / 桌面端 / 加密备份）共用的资源上限（issue #107）。
//
// 此前 ParseMultipartForm(32<<20) 的 32MiB 只是内存/临时文件切分阈值，不限制
// 请求体总大小；随后 io.ReadAll(file) 无上限，accounts 数组也没有数量上限，
// 循环内逐号同步签到/激活/余额查询——错误的客户端、误选大文件或恶意管理员
// 都能造成内存/磁盘耗尽与上游请求风暴。
const (
	// cockpitMaxUploadBytes 请求体上限（与加密导入 secureMaxUploadBytes 同口径：
	// 正常 cockpit 导出远小于此）。http.MaxBytesReader 在解析前兜住整个 body，
	// io.LimitReader 作为第二道防线。
	cockpitMaxUploadBytes = 8 << 20
	// maxImportAccounts 单次导入账号数上限：超过整体拒绝（不静默截断，
	// 避免用户以为全导入了）。
	maxImportAccounts = 1000
	// importWorkers 导入后签到/激活/余额查询的并发上限。这些是逐号上游
	// 调用，串行执行会让单请求长时间占用且放大延迟；无界并发又会瞬时打满上游。
	importWorkers = 3
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
	accounts, ok := readCockpitAccounts(w, r)
	if !ok {
		return
	}

	importedAuths, imported, skipped, errs := p.persistCockpitAccounts(accounts)

	// 阶段 2（有界并发）：签到/激活/余额查询。失败仅记日志，不阻断导入。
	p.postImportAccounts(importedAuths)

	total := len(accounts)
	log.Printf("panel: cockpit import finished total=%d imported=%d skipped=%d", total, imported, skipped)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"total":    total,
		"imported": imported,
		"skipped":  skipped,
		"errors":   errs,
	})
}

// readCockpitAccounts 解析 multipart 上传并校验账号数组；失败时已写回响应。
func readCockpitAccounts(w http.ResponseWriter, r *http.Request) ([]cockpitAccount, bool) {
	// 解析前先兜住请求体总大小（issue #107）：ParseMultipartForm 的 maxMemory
	// 只决定内存/临时文件切分，不限制大小。超限时 ParseMultipartForm 返回错误，
	// 不会把剩余 body 写满临时目录。
	r.Body = http.MaxBytesReader(w, r.Body, cockpitMaxUploadBytes)
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		writeErr(w, http.StatusBadRequest, "parse form: "+err.Error())
		return nil, false
	}

	file, _, err := r.FormFile("file")
	if err != nil {
		writeErr(w, http.StatusBadRequest, "missing file field: "+err.Error())
		return nil, false
	}
	defer file.Close()

	// 第二道防线：即使 multipart 解析路径变化，读取也不会无界扩容。
	raw, err := io.ReadAll(io.LimitReader(file, cockpitMaxUploadBytes+1))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "read file: "+err.Error())
		return nil, false
	}

	if len(raw) > cockpitMaxUploadBytes {
		writeErr(w, http.StatusRequestEntityTooLarge,
			fmt.Sprintf("file exceeds %d bytes", cockpitMaxUploadBytes))

		return nil, false
	}

	var accounts []cockpitAccount
	if err := json.Unmarshal(raw, &accounts); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return nil, false
	}

	if len(accounts) == 0 {
		writeErr(w, http.StatusBadRequest, "empty accounts array")
		return nil, false
	}

	if len(accounts) > maxImportAccounts {
		// 整体拒绝：静默截断会让用户以为全部导入成功。
		writeErr(w, http.StatusBadRequest,
			fmt.Sprintf("too many accounts: %d (max %d per import)", len(accounts), maxImportAccounts))

		return nil, false
	}

	return accounts, true
}

// persistCockpitAccounts 串行落盘并热加载；失败项逐条返回原因。
func (p *Panel) persistCockpitAccounts(accounts []cockpitAccount) ([]*auth.Auth, int, int, []string) {
	// 阶段 1（串行）：字段校验 + 落盘 + 热加载。落盘是本地操作且池写入不并发，
	// 保持逐号顺序，失败项继续逐条返回（与旧实现逐字一致）。
	return persistAccounts(accounts, p.persistCockpitAccount)
}

// persistAccounts 串行落盘并热加载账号；失败项逐条返回原因。
func persistAccounts[T any](
	accounts []T, persist func(*T) (*auth.Auth, string),
) ([]*auth.Auth, int, int, []string) {
	importedAuths := make([]*auth.Auth, 0, len(accounts))
	errs := make([]string, 0, len(accounts))

	var imported, skipped int

	for i := range accounts {
		account, msg := persist(&accounts[i])
		if account == nil {
			skipped++

			errs = append(errs, msg)

			continue
		}

		imported++

		importedAuths = append(importedAuths, account)
	}

	return importedAuths, imported, skipped, errs
}

// persistCockpitAccount 校验并落盘单个 cockpit 账号（含热加载进池）。
// 返回 (账号, 空串) 表示导入成功；账号为 nil 时第二返回值是失败原因。
// 失败原因与旧实现逐字一致。上游调用（签到/余额）不在此处，见阶段 2。
func (p *Panel) persistCockpitAccount(acc *cockpitAccount) (*auth.Auth, string) {
	uid := strings.TrimSpace(acc.UID)
	at := strings.TrimSpace(acc.AccessToken)

	str := strings.TrimSpace(acc.RefreshToken)
	if uid == "" || at == "" || str == "" {
		return nil, fmt.Sprintf("missing required fields (id=%s)", acc.ID)
	}

	if !validUID(uid) {
		return nil, fmt.Sprintf("invalid uid (id=%s)", acc.ID)
	}

	// 按 domain 推断 realm：workbuddy.ai / codebuddy.ai 家族 → global，否则 cn。
	realm := auth.ResolveRealm("", acc.Domain)

	a := buildImportedAuth(acc, uid, at, str, p.cfg.AuthDir)
	if err := p.persistImportedAuth(a, realm, uid); err != nil {
		return nil, err.Error()
	}

	return a, ""
}

// postImportAccounts 对已落盘的账号做签到/激活/余额查询，并发有上限
// （issue #107）：逐号上游调用不再串行放大单请求时长，也不会无界并发打满上游。
// cockpit / 桌面端 / 加密备份导入共用（加密备份此前串行逐号调用）。
// 所有失败只记日志，不改变导入结果。
func (p *Panel) postImportAccounts(accounts []*auth.Auth) {
	sem := make(chan struct{}, importWorkers)

	var wg sync.WaitGroup

	for _, item := range accounts {
		// 先取名额再起 goroutine：导入上限 1000 个账号时不在信号量上堆积阻塞协程。
		sem <- struct{}{}

		wg.Add(1)
		go func(account3 *auth.Auth) {
			defer wg.Done()
			defer func() { <-sem }()

			uid := account3.UID
			p.importCheckin(account3, uid)

			if rm, tt, err := p.cfg.Upstream.UserResource(account3); err == nil {
				p.cfg.Pool.ReenableIfCredits(uid, rm, tt)
			}
		}(item)
	}

	wg.Wait()
}

// buildImportedAuth 由 cockpit 账号构造 auth：expires_at 毫秒转秒（缺省 +365 天），
// nickname 缺失时回落 email。
func buildImportedAuth(acc *cockpitAccount, strVal, at, str2, authDir string) *auth.Auth {
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
		RefreshToken: str2,
		ExpiresAt:    expiresAt,
		Domain:       acc.Domain,
		UID:          strVal,
		Nickname:     nickname,
		FilePath:     filepath.Join(authDir, fmt.Sprintf("workbuddy-%s.json", strVal)),
	}
}

// persistImportedAuth 按 realm 补 realm 键、落盘并热加载进池；失败返回与旧实现
// 逐字一致的原因串（调用方计入 skipped）。
func (p *Panel) persistImportedAuth(account *auth.Auth, realm, uid string) error {
	if realm == "global" {
		if _, err := auth.BackfillRealmFor(account, "global"); err != nil {
			return fmt.Errorf("uid=%s: set realm failed: %w", uid, err)
		}
	} else {
		_, _ = account.BackfillRealm()
	}

	if err := account.SaveAtomic(); err != nil {
		return fmt.Errorf("uid=%s: save auth failed: %w", uid, err)
	}

	p.cfg.Pool.Add(account)
	p.cfg.Pool.Revive(uid)

	return nil
}

// importCheckin 导入后的签到/激活（幂等；失败仅记日志，不阻断导入）。realm 取落盘
// 后的 a.Realm()，与 persistImportedAuth 的 BackfillRealm/iss 兜底结果一致，避免
// 本地按 domain 推断的 realm 与最终写入的 realm 分叉而漏掉 global 激活/trial。
func (p *Panel) importCheckin(account4 *auth.Auth, uid string) {
	if account4.Realm() == "global" {
		p.importGlobalActivation(account4, uid)
		return
	}

	if err := p.cfg.Upstream.DailyCheckin(account4); err != nil {
		log.Printf("panel: import checkin uid=%s: %v", uid, err)
	}
}

// importGlobalActivation global 导入后的注册激活 + trial 领取（幂等；失败仅记日志）。
func (p *Panel) importGlobalActivation(account2 *auth.Auth, uid string) {
	if activated, err := p.cfg.Upstream.GlobalCompleteRegistration(account2); err != nil {
		log.Printf("panel: import global 注册激活 uid=%s: %v", uid, err)
	} else if activated {
		log.Printf("panel: import global 注册激活 uid=%s 完成", uid)
	}

	if claimed, err := p.cfg.Upstream.ClaimTrial(account2); err != nil {
		log.Printf("panel: import global trial uid=%s: %v", uid, err)
	} else if claimed {
		log.Printf("panel: import global trial uid=%s 已领", uid)
	}
}
