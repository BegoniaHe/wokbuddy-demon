// keys.go 命名 API 密钥的面板管理接口（签发 / 撤销 / 删除 / 列表）。
//
// 边界：面板自身鉴权走 admin/readonly（未配置时回落默认 api_key，见 panel.withReadAuth）；命名密钥是发给
// 网关客户端的，只作用于 /v1/*（见 server.withClientAuth），不获得管理权限。
// 明文仅在签发响应里返回一次，列表与持久化都只有哈希与展示前缀。
package panel

import (
	"errors"
	"log"
	"net/http"
	"strings"

	"github.com/BegoniaHe/wokbuddy-demon/internal/apikey"
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
		Name   string        `json:"name"`
		Limits apikey.Limits `json:"limits"`
	}
	// 指针接收：空 body / 畸形 JSON / 字面量 null 都拒绝，避免静默签发默认名密钥。
	if err := decodeJSONBody(r.Body, &body); err != nil || body == nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}

	rec, raw, err := p.cfg.Keys.Create(strings.TrimSpace(body.Name), body.Limits)
	if err != nil {
		if errors.Is(err, apikey.ErrInvalidLimits) {
			writeErr(w, http.StatusBadRequest, err.Error())

			return
		}

		writeErr(w, http.StatusInternalServerError, "create key: "+err.Error())

		return
	}

	log.Printf("panel: 签发命名密钥 id=%s name=%q", rec.ID, rec.Name)
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

	id := r.PathValue("id")

	ok, err := p.cfg.Keys.UpdateLimits(id, body.Limits)
	if err != nil {
		if errors.Is(err, apikey.ErrInvalidLimits) {
			writeErr(w, http.StatusBadRequest, err.Error())

			return
		}

		writeErr(w, http.StatusInternalServerError, "update limits: "+err.Error())

		return
	}

	if !ok {
		writeErr(w, http.StatusNotFound, "key not found")
		return
	}

	log.Printf("panel: 更新命名密钥限制 id=%s rpm=%d tpm=%d concurrent=%d quota=%d ips=%d models=%d",
		id, body.Limits.RPM, body.Limits.TPM, body.Limits.MaxConcurrent,
		body.Limits.QuotaTokens, len(body.Limits.IPAllowlist), len(body.Limits.Models))
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
