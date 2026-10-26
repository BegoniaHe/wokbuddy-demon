// config.go 面板配置页接口：读取当前配置、校验并保存（热生效 + 重启项标注）。
//
// 分工：cmd/server 持有 Config 类型与校验逻辑（Load/normalize），此处只做
// HTTP 编排——GET 回显、POST 透传给注入的 SaveConfig 闭包（由 main 完成
// "校验 → 落盘 → 热应用 → 返回需重启字段列表"）。
package panel

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
)

// redactedConfigKeys 配置回显时必须抹掉的顶层凭据字段。前端 secret 字段留空
// 表示「保持原值」，保存时 merge 不会清空未提交的键。
var redactedConfigKeys = []string{"api_key", "admin_api_key", "readonly_api_key"}

// getConfig 返回当前配置文件内容与路径（前端按 schema 渲染表单）。
// 三把顶层凭据统一置空：GET /panel/api/config 仅 admin 可达，但仍不回显原文。
func (p *Panel) getConfig(w http.ResponseWriter, r *http.Request) {
	if p.cfg.LoadConfig == nil {
		writeErr(w, http.StatusNotImplemented, "config api not available")
		return
	}

	cfg, err := p.cfg.LoadConfig()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "load config: "+err.Error())
		return
	}

	raw, err := json.Marshal(cfg)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "encode config: "+err.Error())
		return
	}

	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		writeErr(w, http.StatusInternalServerError, "encode config: "+err.Error())
		return
	}

	for _, key := range redactedConfigKeys {
		doc[key] = ""
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":     true,
		"path":   p.cfg.ConfigPath,
		"config": doc,
	})
}

// saveConfig 保存配置：body 直接是配置 JSON（前端按 schema 组装完整对象）。
// SaveConfig 闭包内部完成校验+落盘+热应用；校验失败返回 400 且不写盘。
func (p *Panel) saveConfig(w http.ResponseWriter, r *http.Request) {
	if p.cfg.SaveConfig == nil {
		writeErr(w, http.StatusNotImplemented, "config api not available")
		return
	}

	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "read body: "+err.Error())
		return
	}

	restartRequired, err := p.cfg.SaveConfig(raw)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}

	if restartRequired == nil {
		restartRequired = []string{}
	}

	log.Printf("panel: 配置已保存（热生效完成；需重启字段 %d 个）", len(restartRequired))
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":               true,
		"restart_required": restartRequired,
	})
}

// restoreConfig 从 config.json.1 恢复上一版配置；恢复后仍走同一套热应用与重启提示。
func (p *Panel) restoreConfig(w http.ResponseWriter, _ *http.Request) {
	if p.cfg.RestoreConfig == nil {
		writeErr(w, http.StatusNotImplemented, "config restore not available")
		return
	}

	restartRequired, err := p.cfg.RestoreConfig()
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}

	if restartRequired == nil {
		restartRequired = []string{}
	}

	log.Printf("panel: 配置已恢复上一版（需重启字段 %d 个）", len(restartRequired))
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":               true,
		"restart_required": restartRequired,
	})
}
