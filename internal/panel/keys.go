// keys.go 命名 API 密钥的面板管理接口（签发 / 撤销 / 删除 / 列表）。
//
// 边界：面板自身鉴权仍只认默认 api_key（见 panel.withAuth）；命名密钥是发给
// 网关客户端的，只作用于 /v1/*（见 server.withClientAuth），不获得管理权限。
// 明文仅在签发响应里返回一次，列表与持久化都只有哈希与展示前缀。
package panel

import (
	"encoding/json"
	"log"
	"net/http"
	"strings"
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
	var body struct {
		Name string `json:"name"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	rec, raw, err := p.cfg.Keys.Create(strings.TrimSpace(body.Name))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "create key: "+err.Error())
		return
	}
	log.Printf("panel: 签发命名密钥 id=%s name=%q prefix=%s", rec.ID, rec.Name, rec.Prefix)
	writeJSON(w, http.StatusOK, map[string]any{"key": raw, "record": rec})
}

// keysRevoke 软撤销一把密钥（保留记录与计数，立即从网关鉴权移除）。
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
