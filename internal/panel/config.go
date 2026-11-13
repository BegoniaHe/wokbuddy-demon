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
	"net/url"
	"strings"
)

// redactedConfigPaths 配置回显时必须抹掉的凭据字段（点分路径）。前端 secret 字段
// 留空表示「保持原值」，保存时 merge 不会清空未提交的键。
//
// 只抹顶层三把 key 不够：upstash.token 会拼进 rediss:// 连接串、
// upstream.device_token 是设备风控凭据、notify.webhook_url 可内嵌 token——
// 它们同样是「配置不得回显凭证」的覆盖范围。
var redactedConfigPaths = [][]string{
	{"api_key"},
	{"admin_api_key"},
	{"readonly_api_key"},
	{"upstash", "token"},
	{"upstream", "device_token"},
	{"notify", "webhook_url"},
}

// MaskProxyEntry 单个代理条目的打码口径：只留 scheme://host[:port]，userinfo、
// 路径、query 全部丢弃；无法解析时返回固定占位串。
//
// 面板回显与保存侧还原（cmd/server 的 restoreMaskedProxySecrets）必须用同一实现：
// 口径不一致时「没改动」会被判成「改成了掩码值」，把掩码字面量写进配置，代理凭据
// 一次保存就被抹空。四段式 host:port:user:pass 走不了 url.Parse，两侧都落到占位串。
func MaskProxyEntry(raw string) string {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Scheme == "" {
		return "[unparsable proxy entry]"
	}

	if parsed.Host == "" {
		return parsed.Scheme + "://"
	}

	return parsed.Scheme + "://" + parsed.Host
}

// MaskProxySubURL 订阅 URL 的打码口径：只留 scheme://host（host 缺失即视为非法，
// 订阅 URL 没有 host 就无从请求）。同样必须与保存侧还原口径一致。
func MaskProxySubURL(raw string) string {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "[invalid proxy subscription url]"
	}

	return parsed.Scheme + "://" + parsed.Host
}

// redactProxyFields 抹掉 upstream 里代理相关数组的凭据。
//
// redactedConfigPaths 只能表达「标量字段」，而代理凭据在数组元素里：
// upstream.proxies[]、upstream.proxy_slots[].url、upstream.proxy_subscriptions[]。
// 漏掉它们等于把代理口令与订阅 token 原样回显在 GET /panel/api/config。
func redactProxyFields(doc map[string]any) {
	upstream, isMap := doc["upstream"].(map[string]any)
	if !isMap {
		return
	}

	redactProxyStringList(upstream, "proxies", MaskProxyEntry)
	redactProxyStringList(upstream, "proxy_subscriptions", MaskProxySubURL)
	redactProxySlotURLs(upstream)
}

// redactProxyStringList 把 upstream[key] 里的每个字符串换成掩码形态。
func redactProxyStringList(upstream map[string]any, key string, mask func(string) string) {
	list, isList := upstream[key].([]any)
	if !isList {
		return
	}

	for i, item := range list {
		if raw, isStr := item.(string); isStr {
			list[i] = mask(raw)
		}
	}
}

// redactProxySlotURLs 抹掉 proxy_slots[].url 的凭据（槽条目是对象，不能用上面的列表口径）。
func redactProxySlotURLs(upstream map[string]any) {
	slots, isList := upstream["proxy_slots"].([]any)
	if !isList {
		return
	}

	for _, item := range slots {
		if slot, isMap := item.(map[string]any); isMap {
			if raw, isStr := slot["url"].(string); isStr {
				slot["url"] = MaskProxyEntry(raw)
			}
		}
	}
}

// redactConfigDoc 按 redactedConfigPaths 把凭据字段置空（路径不存在时跳过）。
func redactConfigDoc(doc map[string]any) {
	for _, path := range redactedConfigPaths {
		obj := doc

		for _, key := range path[:len(path)-1] {
			next, ok := obj[key].(map[string]any)
			if !ok {
				obj = nil

				break
			}

			obj = next
		}

		if obj == nil {
			continue
		}

		leaf := path[len(path)-1]
		if _, ok := obj[leaf]; ok {
			obj[leaf] = ""
		}
	}
}

// getConfig 返回当前配置文件内容与路径（前端按 schema 渲染表单）。
// 凭据字段统一置空（见 redactedConfigPaths）：GET /panel/api/config 仅 admin 可达，
// 但仍不回显原文。
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

	redactConfigDoc(doc)
	redactProxyFields(doc)

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

	// 读到统一上限 +1 字节即可判超限：原先固定 1 MiB 会把合法的大配置（代理槽、
	// model_timeouts 多）静默截断，最终落成语焉不详的 400「invalid JSON」。
	raw, err := io.ReadAll(io.LimitReader(r.Body, panelAPIMaxBodyBytes+1))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "read body: "+err.Error())
		return
	}

	if int64(len(raw)) > panelAPIMaxBodyBytes {
		writeErr(w, http.StatusRequestEntityTooLarge, "config body too large")
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
