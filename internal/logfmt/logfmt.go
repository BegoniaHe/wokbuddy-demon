// Package logfmt 统一网关日志的 uid 截断与模块前缀约定。
//
// 约定：
//   - uid 统一截 8 位：与 chat 流水行的 uid 展示口径一致，
//     日志行只留 uid 前 8 位。全量 uid 可从 data/state.json 反查，8 位前缀在单池内足以区分。
//   - 模块前缀：调度四类已有天然前缀（travel/activity/checkin/keepalive）保持；
//     其他补 [pool]/[auth]/[server] 等 [mod] 方括号前缀，redisstore/session 已有保持。
//   - 级别语义：正常流转不打级别字样（保持简洁）；可疑/降级/失败行加 WARN:/ERR: 前缀。
//
// 本包不引入日志库，只提供 UID8 截断 / Label 账号标签 / Pad 显示宽对齐三个纯字符串
// helper，供各包替代裸写 [:8] 与手算表格列宽（防 uid 短于 8 越界、防中文昵称错位）。
package logfmt

import (
	"strings"
	"unicode/utf8"
)

// Truncate 截断字符串到 n 字节上限（先 TrimSpace，与旧 upstream/内部实现口径
// 一致），切点落在多字节字符中间时回退到 UTF-8 rune 边界——错误 body 多为中文
// （"将在 … 重置"），按字节切会出半截序列乱码。短于 n 原样返回；n<=0 返回空串。
func Truncate(str2 string, n int) string {
	if n <= 0 {
		return ""
	}

	str2 = strings.TrimSpace(str2)
	if len(str2) > n {
		// s[n] 是切点后的首字节：是 rune 的后续字节（continuation）说明切点落在
		// 多字节字符中间，逐字节回退到 rune 边界（该字符整个让出）。
		for n > 0 && !utf8.RuneStart(str2[n]) {
			n--
		}

		return str2[:n]
	}

	return str2
}

// UID8 返回 uid 的前 8 位；空 uid 返回 "-"（与 chat 流水行同口径）。
//
// 用于调度类与非调度类日志行，把 <task> <full-uid>: ... 改为 <task> <uid8>: ...
// 全量 uid 留在 state.json 供排查，日志里 8 位足够唯一定位。
func UID8(uid string) string {
	if uid == "" {
		return "-"
	}

	if len(uid) > 8 {
		return uid[:8]
	}

	return uid
}

// Label 返回日志里的账号标签，形如 "示例昵称甲(a1b2c3d4)"；昵称为空时退回 "a1b2c3d4"。
//
// 为什么需要：uid8 是机器标识，排障时人眼无法直接判断"刚才那个 429/6004 是哪个号"，
// 必须再拿 uid8 去 auths/ 或 data/state.json 反查昵称，一条日志要多跳一步。昵称随
// 登录落在 auths/<uid>.json 的 account.nickname，这里把它与 uid8 拼成可直接辨认的
// 标签——昵称认人、uid8 供 grep，两者都保留。
//
// uid 与 nick 同时为空时返回 "-"（与 UID8 口径一致，避免打出 "(-)"）。
func Label(uid, nick string) string {
	short := UID8(uid)

	nick = strings.TrimSpace(nick)
	if nick == "" {
		return short
	}

	return nick + "(" + short + ")"
}

// DisplayWidth 返回 s 的终端显示列宽：CJK / 全角 / emoji 记 2 列，其余记 1 列。
//
// 存在意义：账号昵称是用户自定的中文（"猫" 是 3 字节但占 2 列，"sample" 是 6 字节占
// 6 列），用 len()（字节数）做表格对齐会导致列宽忽宽忽窄。Go 标准库没有显示宽度函数，
// 本仓库不引 go-runewidth（保持零第三方依赖），故内置这份覆盖常见宽字符区段的判定。
func DisplayWidth(s string) int {
	w := 0
	for _, r := range s {
		w += runeWidth(r)
	}

	return w
}

// Pad 把 s 右补空格到 width 显示列宽；已超宽或 width<=0 时原样返回（不截断）。
// 只补不截：截断会丢信息，超宽时让该行自然变宽，保持内容完整。
func Pad(str string, width int) string {
	if width <= 0 {
		return str
	}

	if d := width - DisplayWidth(str); d > 0 {
		return str + strings.Repeat(" ", d)
	}

	return str
}

// maxShortUALen ShortUA 的返回上限（显示列宽足够放下 "WorkBuddy/5.5.6"、
// "python-requests/2.31.0" 这类常见客户端标签）。
const maxShortUALen = 40

// shortUAEngines UA 里只说明渲染引擎、不说明"是什么客户端"的通用 token：浏览器 UA
// 恒定包含它们，拿它当客户端标签等于没信息。
var shortUAEngines = map[string]bool{
	"mozilla": true, "applewebkit": true, "gecko": true, "khtml": true,
	"like": true, "safari": true, "compatible": true, "msie": true, "trident": true,
}

// ShortUA 从 User-Agent 提取便于人眼识别的客户端标签（"curl/8.4.0"、
// "WorkBuddy/5.5.6"、"Chrome/120.0.0.0"）。
//
// 为什么需要：面板「运行日志」与 stdout 流水行都要展示调用来源，而完整 UA 动辄
// 120+ 字符（浏览器尤其），直接铺进表格会把其它列挤没。这里只留"是什么客户端"，
// 完整 UA 仍存在 reqlog.Event.UserAgent 里供面板悬停查看。
//
// 规则：取第一个形如 name/version 且 name 不是渲染引擎的 token；没有则回落整串
// 的前 maxShortUALen 字节（纯产品名 UA，如 "node"）。空 UA 返回空串。
func ShortUA(str3 string) string {
	str3 = strings.TrimSpace(str3)
	if str3 == "" {
		return ""
	}

	for tok := range strings.FieldsSeq(str3) {
		tok = strings.Trim(tok, "(),;")

		name, _, ok := strings.Cut(tok, "/")
		if !ok || name == "" {
			continue
		}

		if shortUAEngines[strings.ToLower(name)] {
			continue
		}

		return Truncate(tok, maxShortUALen)
	}

	return Truncate(str3, maxShortUALen)
}

// wideRanges 记 2 列宽字符的区段（闭区间），取自 Unicode East Asian Width 的
// Wide/Fullwidth 集合（与 go-runewidth 的默认表口径一致），只保留实际会用到的段。
// 用表驱动替代逐 case 分支，保持逐区间等价的同时降低圈复杂度。
var wideRanges = [][2]rune{
	{0x1100, 0x115f},   // Hangul Jamo 初声
	{0x2329, 0x232a},   // 尖括号
	{0x2e80, 0xa4cf},   // CJK 部首…Yi（303f 是窄字符，见 isWide 排除）
	{0xac00, 0xd7a3},   // Hangul 音节
	{0xf900, 0xfaff},   // CJK 兼容表意
	{0xfe30, 0xfe6f},   // CJK 兼容形式
	{0xff00, 0xff60},   // 全角 ASCII
	{0xffe0, 0xffe6},   // 全角符号
	{0x1f300, 0x1f9ff}, // emoji
	{0x20000, 0x3fffd}, // CJK 扩展 B 及以后
}

// isWide 判定 rune 是否占 2 显示列。
func isWide(r rune) bool {
	if r == 0x303f { // 窄字符，落在 0x2e80…0xa4cf 段内需单独排除
		return false
	}

	for _, rg := range wideRanges {
		if r >= rg[0] && r <= rg[1] {
			return true
		}
	}

	return false
}

// runeWidth 单个 rune 的显示列宽。区段判定见 isWide。
func runeWidth(r rune) int {
	switch {
	case r == 0:
		return 0
	case r < 0x20 || (r >= 0x7f && r < 0xa0):
		// 控制字符（含 DEL/C1）不占位：日志里若混入 \t \r 不破坏列宽计算。
		return 0
	case isWide(r):
		return 2
	}

	return 1
}
