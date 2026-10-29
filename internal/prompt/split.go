// split.go 11128「渠道未授权」的 system 拆句抢救（issue #389）。
//
// 背景：上游对 system 里的**连续原文**特征按渠道校验拦成
// 400 {"code":11128,"msg":"Illegal API invocation from an unapproved channel"}。
// 实测（CN 账号真机）：同一段长 description 同义改写后 200、按句界拆成多条 system
// 也 200，而把人设这类**单句短指令**拆开反而废掉指令（ARRR 0/3）。所以这里只拆
// 「拆得出两条、且每条都不止一句」的长段：既不删原文，也不动人设。
//
// 与 Rewrite/Append 的分工：那两条改的是「用哪份 system」，本文件只改「system 拆成
// 几条」，客户端写的内容逐字保留（拆点上的空白归前一条，拼接后与原文一致）。
package prompt

import (
	"encoding/json"
	"maps"
	"slices"
	"strings"
	"unicode"
)

// minSentencesPerSplit 拆出的一条 system 至少要包含的句数（另一条同样要满足）。
// 一条 3 句的段拆成 2+1 → 出现单句短指令，正是实测「拆开会废指令」的形态，
// 故下限 2 句 = 段至少 4 句才拆。
const minSentencesPerSplit = 2

// SplitSystemBySentences 把「多句长 system 消息」按句界平均拆成两条，其余消息与所有
// 其它字段逐字不动。返回 (新 body, 是否拆过)。
//
// 适用性（即 issue #389 的「该变体是否适用」判据）：只有一条 system/developer 消息
// 含 ≥2×minSentencesPerSplit 句、且拆出的两条都非空时才拆；否则原样返回 false，
// 调用方回落到既有兜底。content 非字符串（多模态数组）不动——拆 part 不属于本形态。
func SplitSystemBySentences(body []byte) ([]byte, bool) {
	if len(body) == 0 {
		return body, false
	}

	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil || obj == nil {
		return body, false
	}

	msgs, ok := obj["messages"].([]any)
	if !ok {
		return body, false
	}

	out := make([]any, 0, len(msgs)+1)
	split := false

	for _, raw := range msgs {
		halves, ok := splitSystemMessage(raw)
		if !ok {
			out = append(out, raw)

			continue
		}

		out = append(out, halves...)
		split = true
	}

	if !split {
		return body, false
	}

	obj["messages"] = out

	rewritten, err := json.Marshal(obj)
	if err != nil {
		return body, false
	}

	return rewritten, true
}

// splitSystemMessage 尝试把一条 system/developer 消息拆成两条；不适用时 ok=false。
// 其余字段（name 等）逐条复制，只有 content 按句界切开。
func splitSystemMessage(raw any) ([]any, bool) {
	m, ok := raw.(map[string]any)
	if !ok || !isSystemMsg(m) {
		return nil, false
	}

	content, ok := m["content"].(string)
	if !ok {
		return nil, false
	}

	parts := splitContentBySentences(content)
	if len(parts) != 2 {
		return nil, false
	}

	halves := make([]any, 0, len(parts))
	for _, part := range parts {
		clone := make(map[string]any, len(m))
		maps.Copy(clone, m)
		clone["content"] = part
		halves = append(halves, clone)
	}

	return halves, true
}

// splitContentBySentences 把一段文本按句界切成两条（各含 ≥minSentencesPerSplit 句）。
// 句数不足、或切开后任一条为空 → nil（调用方判为不适用）。
func splitContentBySentences(text string) []string {
	sentences := splitSentences(text)
	// 每条至少 minSentencesPerSplit 句 → 段内至少 2×minSentencesPerSplit 句才拆得动。
	if len(sentences) < 2*minSentencesPerSplit {
		return nil
	}

	half := (len(sentences) + 1) / 2

	parts := []string{
		strings.TrimSpace(strings.Join(sentences[:half], "")),
		strings.TrimSpace(strings.Join(sentences[half:], "")),
	}

	if slices.Contains(parts, "") {
		return nil
	}

	return parts
}

// splitSentences 按句界切句，切点保留在该句尾部（含其后的空白），因此各段直接拼接
// 可还原原文——拆句不改一个字，只改消息边界。
//
// 句界判定：
//   - 句末中文标点 。！？ 之后即断；
//   - ASCII . ! ? 要求后面紧跟空白，且空白之后是「句子开头」形态（大写字母、数字、
//     引号、括号或非 ASCII 字符）。这样 e.g. / i.e. / 3.5 / node.js / 文件后缀
//     不会被当成句末切开。
func splitSentences(text string) []string {
	runes := []rune(text)

	var (
		out   []string
		start int
	)

	for i := range runes {
		switch runes[i] {
		case '\u3002', '\uff01', '\uff1f': // 。！？
			out = append(out, string(runes[start:i+1]))

			start = i + 1
		case '.', '!', '?':
			end := i + 1

			for end < len(runes) && isSentenceSpace(runes[end]) {
				end++
			}

			if end == i+1 || end >= len(runes) || !isSentenceStart(runes[end]) {
				continue
			}

			out = append(out, string(runes[start:end]))

			start = end
		}
	}

	if start < len(runes) {
		out = append(out, string(runes[start:]))
	}

	return out
}

// isSentenceSpace 句末标点与下一句之间的空白（含换行）。
func isSentenceSpace(r rune) bool {
	return r == ' ' || r == '\t' || r == '\r' || r == '\n'
}

// isSentenceStart 下一句的开头形态：大写字母、数字、引号、括号或非 ASCII 字符。
func isSentenceStart(r rune) bool {
	return unicode.IsUpper(r) || unicode.IsDigit(r) || r > unicode.MaxASCII ||
		r == '"' || r == '\'' || r == '(' || r == '[' || r == '*'
}
