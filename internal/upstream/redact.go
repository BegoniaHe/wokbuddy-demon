// redact.go 日志专用脱敏：上游错误原文可能包含 Authorization、token、
// URL 查询参数等高敏感值，写日志前统一替换为 [REDACTED]。
//
// 只用于日志。客户端可见的错误结构与 internal/upstream/sanitize.go 的出站
// 指纹策略都不在此处改变（issue #88）。
package upstream

import (
	"regexp"
	"strings"

	"github.com/BegoniaHe/wokbuddy-demon/internal/logfmt"
)

const redacted = "[REDACTED]"

var (
	// authorization: Bearer xxx / "authorization":"xxx"
	reAuthorization = regexp.MustCompile(`(?i)(authorization\s*[:=]\s*)(?:bearer\s+)?[^\s"',;}]+`)
	// 独立的 Bearer token。
	reBearer = regexp.MustCompile(`(?i)\bbearer\s+[A-Za-z0-9._~+/\-=]+`)
	// 常见敏感键值（JSON 或 query/header 形态）；只替换值，保留键名便于排障。
	reSecretKV = regexp.MustCompile(`(?i)("?(?:access_token|refresh_token|id_token|token|` +
		`api_key|apikey|secret|client_secret|password|passwd|device_token)"?\s*[:=]\s*)` +
		`("[^"]*"|'[^']*'|[^\s"',;}]+)`)
	// URL 查询参数值：?token=...&key=... 全部脱敏。
	reQueryValue = regexp.MustCompile(`([?&][^=&\s"']+=)[^&\s"']*`)
	// 长高熵串兜底（非 key=value 形态的裸 token）。
	reLongToken = regexp.MustCompile(`\b[A-Za-z0-9][A-Za-z0-9._~+/\-=]{31,}\b`)
)

// redactForLog 清理控制字符、脱敏敏感值，再截断到 limit 字节（limit<=0 不截断）。
func redactForLog(s string, limit int) string {
	clean := sanitizeLogLine(s)
	clean = reAuthorization.ReplaceAllString(clean, `$1`+redacted)
	clean = reBearer.ReplaceAllString(clean, `Bearer `+redacted)
	clean = reSecretKV.ReplaceAllString(clean, `$1"`+redacted+`"`)
	clean = reQueryValue.ReplaceAllString(clean, `$1`+redacted)
	clean = reLongToken.ReplaceAllStringFunc(clean, redactHighEntropy)
	if limit > 0 {
		clean = logfmt.Truncate(clean, limit)
	}
	return clean
}

// sanitizeLogLine 把换行/制表符与其他控制字符压成空格，避免日志注入与多行伪造。
func sanitizeLogLine(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, s)
}

// redactHighEntropy 只替换明显像 token 的长串：同时含大小写与数字，或含
// 非字母数字符号（_ / + = 等）。长模型名通常只有小写与连字符，保持原样。
func redactHighEntropy(s string) string {
	lower := strings.IndexFunc(s, isLowerASCII) >= 0
	upper := strings.IndexFunc(s, isUpperASCII) >= 0
	digit := strings.IndexFunc(s, isDigitASCII) >= 0
	symbol := strings.ContainsAny(s, "_+=~/")
	if (lower && upper && digit) || symbol {
		return redacted
	}
	return s
}

func isLowerASCII(r rune) bool { return r >= 'a' && r <= 'z' }

func isUpperASCII(r rune) bool { return r >= 'A' && r <= 'Z' }

func isDigitASCII(r rune) bool { return r >= '0' && r <= '9' }
