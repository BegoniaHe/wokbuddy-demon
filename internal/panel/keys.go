// keys.go 命名 API 密钥的面板管理接口（签发 / 撤销 / 删除 / 列表）。
//
// 边界：面板自身鉴权走 admin/readonly（未配置时回落默认 api_key，见 panel.withReadAuth）；命名密钥是发给
// 网关客户端的，只作用于 /v1/*（见 server.withClientAuth），不获得管理权限。
// 明文仅在签发响应里返回一次，列表与持久化都只有哈希与展示前缀。
package panel

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	pathpkg "path"
	"strings"

	"github.com/BegoniaHe/wokbuddy-demon/internal/apikey"
)

var (
	errLimits                = errors.New("limits 不能为负")
	errNormalizeModelsFailed = errors.New("normalizeModels failed")
)

// keysList 列出全部命名密钥（不含哈希与明文）。
func (p *Panel) keysList(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Keys == nil {
		writeErr(w, http.StatusNotImplemented, "api key store not available")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"keys": p.cfg.Keys.List()})
}

// keysCreate 签发一把命名密钥；明文仅此一次随响应返回。
func (p *Panel) keysCreate(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Keys == nil {
		writeErr(w, http.StatusNotImplemented, "api key store not available")
		return
	}

	var body *struct {
		Name string `json:"name"`
	}
	// 指针接收：空 body / 畸形 JSON / 字面量 null 都拒绝，避免静默签发默认名密钥。
	if err := decodeJSONBody(r.Body, &body); err != nil || body == nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}

	rec, raw, err := p.cfg.Keys.Create(strings.TrimSpace(body.Name))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "create key: "+err.Error())
		return
	}

	log.Printf("panel: 签发命名密钥 id=%s name=%q prefix=%s", rec.ID, rec.Name, rec.Prefix)
	writeJSON(w, http.StatusOK, map[string]any{"key": raw, "record": rec})
}

// keysRevoke 软撤销一把密钥（保留记录与计数，立即从网关鉴权移除）。
//
//nolint:dupl // 撤销/删除仅文案与 store 方法不同，并排便于对照。
func (p *Panel) keysRevoke(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Keys == nil {
		writeErr(w, http.StatusNotImplemented, "api key store not available")
		return
	}

	id := r.PathValue("id")

	ok, err := p.cfg.Keys.Revoke(id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "revoke key: "+err.Error())
		return
	}

	if !ok {
		writeErr(w, http.StatusNotFound, "key not found or already revoked")
		return
	}

	log.Printf("panel: 撤销命名密钥 id=%s（已从网关鉴权移除）", id)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// keysDelete 物理删除一把密钥（连同其用量记录）。
//
//nolint:dupl // 同 keysRevoke：结构对照优先于抽 helper。
func (p *Panel) keysDelete(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Keys == nil {
		writeErr(w, http.StatusNotImplemented, "api key store not available")
		return
	}

	id := r.PathValue("id")

	ok, err := p.cfg.Keys.Delete(id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "delete key: "+err.Error())
		return
	}

	if !ok {
		writeErr(w, http.StatusNotFound, "key not found")
		return
	}

	log.Printf("panel: 删除命名密钥 id=%s", id)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// keysUpdateLimits 更新一把密钥的 RPM/TPM/并发/模型白名单（下一个请求生效）。
func (p *Panel) keysUpdateLimits(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Keys == nil {
		writeErr(w, http.StatusNotImplemented, "api key store not available")
		return
	}

	var body struct {
		Limits apikey.Limits `json:"limits"`
	}
	if err := decodeJSONBody(r.Body, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}

	limits, err := normalizeLimits(body.Limits)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}

	id := r.PathValue("id")

	ok, err := p.cfg.Keys.UpdateLimits(id, limits)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "update limits: "+err.Error())
		return
	}

	if !ok {
		writeErr(w, http.StatusNotFound, "key not found")
		return
	}

	log.Printf("panel: 更新命名密钥限制 id=%s rpm=%d tpm=%d concurrent=%d models=%v",
		id, limits.RPM, limits.TPM, limits.MaxConcurrent, limits.Models)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// keysUpdateRealm 更新命名密钥的出口绑定（issue #94）：空 = 不绑定，cn/global = 固定。
func (p *Panel) keysUpdateRealm(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Keys == nil {
		writeErr(w, http.StatusNotImplemented, "api key store not available")
		return
	}

	var body struct {
		Realm string `json:"realm"`
	}
	if err := decodeJSONBody(r.Body, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}

	id := r.PathValue("id")

	ok, err := p.cfg.Keys.SetRealm(id, strings.TrimSpace(body.Realm))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}

	if !ok {
		writeErr(w, http.StatusNotFound, "key not found")
		return
	}

	log.Printf("panel: 更新命名密钥出口 id=%s realm=%q", id, body.Realm)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// normalizeLimits 校验限制值并把模型 glob 去空白/去重。
func normalizeLimits(limits apikey.Limits) (apikey.Limits, error) {
	if limits.RPM < 0 || limits.TPM < 0 || limits.MaxConcurrent < 0 {
		return apikey.Limits{}, errLimits
	}

	models, err := normalizeModels(limits.Models)
	if err != nil {
		return apikey.Limits{}, err
	}

	return apikey.Limits{
		RPM: limits.RPM, TPM: limits.TPM, MaxConcurrent: limits.MaxConcurrent, Models: models,
	}, nil
}

// normalizeModels 去空白/去重并校验 glob 语法。
func normalizeModels(raw []string) ([]string, error) {
	seen := map[string]bool{}

	models := make([]string, 0, len(raw))
	for _, item := range raw {
		pat := strings.TrimSpace(item)
		if pat == "" {
			continue
		}

		bare := pat
		if i := strings.IndexByte(pat, ':'); i >= 0 && (pat[:i] == "cn" || pat[:i] == "global") {
			bare = pat[i+1:]
		}

		if bare == "" {
			return nil, fmt.Errorf("模型 glob %q 缺少模型名: %w", item, errNormalizeModelsFailed)
		}

		if _, err := pathpkg.Match(bare, "x"); err != nil {
			return nil, fmt.Errorf("非法模型 glob %q: %w", item, err)
		}

		if seen[pat] {
			continue
		}

		seen[pat] = true
		models = append(models, pat)
	}

	return models, nil
}
