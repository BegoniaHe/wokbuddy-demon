package main

import (
	"net"
	"strings"
)

// weakAPIKeyListenWarning 判断「弱 api_key + 非回环监听」组合并返回启动 WARN
// 文案；无需告警时返回空串。只判断，不阻塞启动，返回文案不含 key 值。
//
// 弱 api_key：空串或仍是公开占位值 test_key。非回环：空 host、0.0.0.0、::、
// 具体非回环地址都算暴露；127.0.0.1、::1、localhost 静默。无法解析的 listen
// 按暴露处理（保守侧）。
func weakAPIKeyListenWarning(apiKey, listen string) string {
	key := strings.TrimSpace(apiKey)
	if key != "" && key != placeholderAPIKey {
		return ""
	}

	if isLoopbackListen(listen) {
		return ""
	}

	// 不把「面板不鉴权」写死：管理面可能另有凭据（后续 admin/readonly 分层），
	// 告警只强调当前可确认的数据面暴露与面板风险。
	return "WARN: api_key 为空或仍为公开占位值，且 listen 不是回环地址；" +
		"网关数据面将不鉴权或使用公开占位 key；面板同样可能被未授权访问，" +
		"请尽快修改 api_key"
}

// isLoopbackListen 判断 listen 地址是否只绑定回环接口。
func isLoopbackListen(listen string) bool {
	host, _, err := net.SplitHostPort(strings.TrimSpace(listen))
	if err != nil {
		return false
	}

	host = strings.TrimSpace(host)
	if host == "" {
		return false
	}

	if strings.EqualFold(host, "localhost") {
		return true
	}

	ip := net.ParseIP(host)

	return ip != nil && ip.IsLoopback()
}
