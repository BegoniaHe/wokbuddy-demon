// agents.go 本机智能体一键配置接口（issue #102）。仅回环来源可调用。
package panel

import (
	"errors"
	"io"
	"net"
	"net/http"

	"github.com/BegoniaHe/wokbuddy-demon/internal/agentcfg"
	"github.com/BegoniaHe/wokbuddy-demon/internal/httpauth"
)

// agentsStatus 返回本机客户端检测与写入状态。
func (p *Panel) agentsStatus(w http.ResponseWriter, r *http.Request) {
	if !p.loopbackRequest(r) {
		writeErr(w, http.StatusForbidden, "仅允许本机回环访问")
		return
	}

	if p.cfg.Agents == nil {
		writeErr(w, http.StatusNotImplemented, "agent config manager not available")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"clients": p.cfg.Agents.Status()})
}

// agentsApply 写入网关专属配置。
func (p *Panel) agentsApply(w http.ResponseWriter, r *http.Request) {
	if !p.loopbackRequest(r) {
		writeErr(w, http.StatusForbidden, "仅允许本机回环访问")
		return
	}

	if p.cfg.Agents == nil {
		writeErr(w, http.StatusNotImplemented, "agent config manager not available")
		return
	}

	var opts agentcfg.Options
	if err := decodeJSONBody(r.Body, &opts); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}

	status, err := p.cfg.Agents.Apply(r.PathValue("client"), opts)
	if errors.Is(err, agentcfg.ErrModified) {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}

	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"client": status})
}

// agentsRestore 还原首次写入前的配置。
func (p *Panel) agentsRestore(w http.ResponseWriter, r *http.Request) {
	if !p.loopbackRequest(r) {
		writeErr(w, http.StatusForbidden, "仅允许本机回环访问")
		return
	}

	if p.cfg.Agents == nil {
		writeErr(w, http.StatusNotImplemented, "agent config manager not available")
		return
	}

	var body struct {
		Force bool `json:"force"`
	}

	if err := decodeJSONBody(r.Body, &body); err != nil && !errors.Is(err, io.EOF) {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}

	err := p.cfg.Agents.Restore(r.PathValue("client"), body.Force)
	if errors.Is(err, agentcfg.ErrModified) {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}

	if errors.Is(err, agentcfg.ErrNotApplied) {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}

	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// loopbackRequest 判断请求是否来自本机回环。
//
// 必须经 httpauth.ClientIP 解析来源：面板常在同机反代之后，直接读 RemoteAddr
// 会把所有外部客户端都看成 127.0.0.1，使「仅回环」限制（以及写本机智能体配置的
// 权限边界）失效。ClientIP 只在 TCP 对端属于 trusted_proxies 时采信转发头，
// 直连时仍只看 RemoteAddr。
func (p *Panel) loopbackRequest(r *http.Request) bool {
	ip := net.ParseIP(httpauth.ClientIP(r, p.cfg.TrustedProxies))

	return ip != nil && ip.IsLoopback()
}
