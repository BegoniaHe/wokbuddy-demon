package panel

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"slices"
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

// cockpitAccount 映射 cockpit tools 导出格式的单个账号：只保留导入实际消费的
// 字段（凭证 / realm 推断 / 企业标识 / 过期时间 / 昵称）。导出文件里的其余键由
// json 解码直接忽略——此前为「字段齐全」声明了 token_type/status/checkin_streak 等十来个
// 从不读取的字段，既无落盘也无展示，纯噪声。
type cockpitAccount struct {
	ID           string `json:"id"`
	Email        string `json:"email"`
	UID          string `json:"uid"`
	Nickname     string `json:"nickname"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresAt    int64  `json:"expires_at"`
	Domain       string `json:"domain"`
	// EnterpriseID 企业标识（issue #446）。cockpit 导出两种写法并存
	// （enterprise_id / enterpriseId），两个键分别解码到两个字段后由
	// enterpriseID() 归一（同一账号只会出现一种写法）。丢失该字段会让企业号
	// 以个人号形态进池，后续签到 / 成长任务全部按个人号打上游（注定被拒）。
	EnterpriseID string `json:"enterprise_id"`
	// EnterpriseIDCamel enterpriseId 驼峰写法的落点。
	EnterpriseIDCamel string `json:"enterpriseId"`
	// 以下驼峰写法与上面的 snake_case 并存（部分 cockpit 导出/第三方工具给的是
	// camelCase）：token/过期时间缺一个就整条落成「missing required fields」或
	// 静默过期，由 accessTokenValue()/refreshTokenValue()/expiresAtValue() 归一。
	AccessTokenCamel  string `json:"accessToken"`
	RefreshTokenCamel string `json:"refreshToken"`
	ExpiresAtCamel    int64  `json:"expiresAt"`
}

// enterpriseID 归一后的企业标识：两种 cockpit 导出写法取非空的那个（都不填 = 个人号）。
func (acc *cockpitAccount) enterpriseID() string {
	if id := strings.TrimSpace(acc.EnterpriseIDCamel); id != "" {
		return id
	}

	return strings.TrimSpace(acc.EnterpriseID)
}

// accessTokenValue / refreshTokenValue 取 snake_case 优先、驼峰回退的凭证字面量。
func (acc *cockpitAccount) accessTokenValue() string {
	if at := strings.TrimSpace(acc.AccessToken); at != "" {
		return at
	}

	return strings.TrimSpace(acc.AccessTokenCamel)
}

func (acc *cockpitAccount) refreshTokenValue() string {
	if rt := strings.TrimSpace(acc.RefreshToken); rt != "" {
		return rt
	}

	return strings.TrimSpace(acc.RefreshTokenCamel)
}

// expiresAtValue 取非零的过期时间字面量（两种写法只有一种会出现）。
func (acc *cockpitAccount) expiresAtValue() int64 {
	if acc.ExpiresAt > 0 {
		return acc.ExpiresAt
	}

	return acc.ExpiresAtCamel
}

// fillMissing 用 other 的非空字段补齐自身（{account,auth} 信封形态：账号元数据在
// account 段、凭证在 auth 段，两段都要消费）。
func (acc *cockpitAccount) fillMissing(other *cockpitAccount) {
	fill := func(dst *string, src string) {
		if strings.TrimSpace(*dst) == "" {
			*dst = src
		}
	}

	fill(&acc.ID, other.ID)
	fill(&acc.Email, other.Email)
	fill(&acc.UID, other.UID)
	fill(&acc.Nickname, other.Nickname)
	fill(&acc.AccessToken, other.accessTokenValue())
	fill(&acc.RefreshToken, other.refreshTokenValue())
	fill(&acc.Domain, other.Domain)
	fill(&acc.EnterpriseID, other.enterpriseID())

	if acc.expiresAtValue() <= 0 {
		acc.ExpiresAt = other.expiresAtValue()
	}
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
	p.postImportAccounts(r.Context(), importedAuths)

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
	if accounts, err = decodeCockpitAccounts(raw); err != nil {
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

// cockpitExpiresAtMillisThreshold 秒/毫秒分界：秒级时间戳到 5138 年也不会达到
// 1e11，毫秒级（1.7e12 量级）必然超过，单侧阈值足够判别。
const cockpitExpiresAtMillisThreshold = 1e11

// cockpit 导出形态归一失败的静态错误（err113：错误值不就地新建）。
var (
	errCockpitEmptyDocument = errors.New("empty document")
	errCockpitRootValue     = errors.New("root value must be a JSON array or object")
	errCockpitDictValue     = errors.New("dictionary entry is not an object")
)

// decodeCockpitAccounts 把 cockpit 导出归一成账号数组。cockpit tools 原生导出的
// 顶层数组之外，实际还会碰到单账号详情导出与其他工具产出的三种形态：
//
//   - 顶层单对象：`{id,uid,access_token,…}`；
//   - `{account,auth}` 信封：元数据在 account 段、凭证在 auth 段（两段都要消费，
//     信封键名不是 uid）；
//   - `{"<uid>":{…}}` 字典：值缺 uid 时用键补齐。
//
// 无法识别的形状返回错误（由调用方转 400），不做静默丢弃。
func decodeCockpitAccounts(raw []byte) ([]cockpitAccount, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil, errCockpitEmptyDocument
	}

	switch trimmed[0] {
	case '[':
		var accounts []cockpitAccount
		if err := json.Unmarshal(trimmed, &accounts); err != nil {
			return nil, fmt.Errorf("accounts array: %w", err)
		}

		return accounts, nil
	case '{':
	default:
		return nil, errCockpitRootValue
	}

	var obj map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &obj); err != nil {
		return nil, fmt.Errorf("root object: %w", err)
	}

	if acctRaw, ok := obj["account"]; ok {
		return decodeCockpitEnvelope(acctRaw, obj["auth"])
	}

	// 单账号对象：出现凭证/账号键就当一条账号解。若按字典处理，凭证值会被
	// 当成 uid 键，报出与真实原因无关的错误。
	if isCockpitAccountObject(obj) {
		var acc cockpitAccount
		if err := json.Unmarshal(trimmed, &acc); err != nil {
			return nil, fmt.Errorf("single account: %w", err)
		}

		return []cockpitAccount{acc}, nil
	}

	return decodeCockpitDict(obj)
}

// isCockpitAccountObject 判定对象是否为单条账号（任一账号键存在即算）。
func isCockpitAccountObject(obj map[string]json.RawMessage) bool {
	for _, key := range []string{"uid", "access_token", "accessToken"} {
		if _, ok := obj[key]; ok {
			return true
		}
	}

	return false
}

// decodeCockpitEnvelope 解 {account,auth} 信封：account 段可为单对象或数组，
// 凭证段 auth 的非空字段补齐到每个账号上。
func decodeCockpitEnvelope(accountRaw, authRaw json.RawMessage) ([]cockpitAccount, error) {
	var authPart cockpitAccount

	if len(bytes.TrimSpace(authRaw)) > 0 {
		if err := json.Unmarshal(authRaw, &authPart); err != nil {
			return nil, fmt.Errorf("auth segment: %w", err)
		}
	}

	var accounts []cockpitAccount
	if err := json.Unmarshal(accountRaw, &accounts); err == nil {
		for i := range accounts {
			accounts[i].fillMissing(&authPart)
		}

		return accounts, nil
	}

	var single cockpitAccount
	if err := json.Unmarshal(accountRaw, &single); err != nil {
		return nil, fmt.Errorf("account segment: %w", err)
	}

	single.fillMissing(&authPart)

	return []cockpitAccount{single}, nil
}

// decodeCockpitDict 解 `{"<uid>":{…}}` 字典；排序保证错误顺序与导入顺序可复现。
func decodeCockpitDict(obj map[string]json.RawMessage) ([]cockpitAccount, error) {
	keys := make([]string, 0, len(obj))
	for key := range obj {
		keys = append(keys, key)
	}

	slices.Sort(keys)

	accounts := make([]cockpitAccount, 0, len(obj))

	for _, key := range keys {
		val := bytes.TrimSpace(obj[key])
		if len(val) == 0 || val[0] != '{' {
			return nil, fmt.Errorf("dictionary entry %q: %w", key, errCockpitDictValue)
		}

		var acc cockpitAccount
		if err := json.Unmarshal(val, &acc); err != nil {
			return nil, fmt.Errorf("dictionary entry %q: %w", key, err)
		}

		if strings.TrimSpace(acc.UID) == "" {
			acc.UID = key
		}

		accounts = append(accounts, acc)
	}

	return accounts, nil
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
	at := acc.accessTokenValue()

	str := acc.refreshTokenValue()
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
func (p *Panel) postImportAccounts(ctx context.Context, accounts []*auth.Auth) {
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
			p.importCheckin(ctx, account3, uid)

			if rm, tt, err := p.cfg.Upstream.UserResource(ctx, account3); err == nil {
				p.cfg.Pool.ReenableIfCredits(uid, rm, tt)
			}
		}(item)
	}

	wg.Wait()
}

// normalizeCockpitExpiresAt 秒/毫秒归一：原生 cockpit 导出给毫秒，部分导出给秒。
// 非正值返回 0，由调用方回落到默认有效期。
func normalizeCockpitExpiresAt(value int64) int64 {
	switch {
	case value <= 0:
		return 0
	case value >= cockpitExpiresAtMillisThreshold:
		return value / 1000
	default:
		return value
	}
}

// buildImportedAuth 由 cockpit 账号构造 auth：expires_at 毫秒转秒（缺省 +365 天），
// nickname 缺失时回落 email，企业标识透传（issue #446）。
func buildImportedAuth(acc *cockpitAccount, strVal, at, str2, authDir string) *auth.Auth {
	// cockpit tools 的 expires_at 原生是毫秒，但部分导出给秒；归一后再判缺省，
	// 否则秒级值被再除 1000 会落到 1970（账号一导入即过期）。
	expiresAt := normalizeCockpitExpiresAt(acc.expiresAtValue())
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
		EnterpriseID: acc.enterpriseID(),
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

	// SaveAtomic 在 auth_dir 内建临时文件，父目录不存在直接 ENOENT；全新部署
	// （未走过 OAuth 登录）首次导入必踩。与 login.go 的落盘前建目录同口径。
	if dir := p.cfg.AuthDir; dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("uid=%s: mkdir auth dir: %w", uid, err)
		}
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
func (p *Panel) importCheckin(ctx context.Context, account4 *auth.Auth, uid string) {
	if account4.Realm() == "global" {
		p.importGlobalActivation(account4, uid)
		return
	}

	// 企业版无签到体系：上游 daily-checkin 恒 400「企业账号不支持该操作」
	// （scheduler.runCheckinAccount 同款实测门控，issue #446）。额度查询照常——
	// postImportAccounts 的 UserResource 已按 IsEnterprise 走企业口径。
	if account4.IsEnterprise() {
		return
	}

	if err := p.cfg.Upstream.DailyCheckin(ctx, account4); err != nil {
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
