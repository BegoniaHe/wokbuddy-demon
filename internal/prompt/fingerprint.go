// fingerprint.go 入站 system 文本指纹（issue #591）。
//
// 用途：上游内容拦截的降级判据要绑定「触发源」而不是进程全局。触发源 = 账号 UID +
// 本指纹；指纹只留 sha256 前缀，不把提示词原文写进 key、日志或面板。
//
// 口径：messages 里所有 role 为 system/developer 且 content 是字符串的消息，按消息
// 顺序拼接后哈希。不用「开头连续块」——上游按 system 连续原文匹配，中途插入的
// system 段同样会撞，隔离判据要覆盖全部 system 文本。
package prompt

import (
	"crypto/sha256"
	"encoding/hex"
	"unicode/utf8"
)

// systemFingerprintBytes 指纹取 sha256 的前若干字节：8 字节（16 位 hex）在单实例
// 的账号 × 人设规模下足够区分，日志行也短。
const systemFingerprintBytes = 8

// SystemFingerprint 返回入站 system/developer 文本的稳定指纹；入站无 system 文本、
// body 不可解析或非法 UTF-8 时返回空串（调用方按「无 system」分桶）。
//
// 空串与真实的「system 内容为空串」不区分：两者对上游都是同一份「没有有效 system
// 文本」，分桶相同不影响隔离语义。
func SystemFingerprint(body []byte) string {
	if len(body) == 0 || !utf8.Valid(body) {
		return ""
	}

	obj, err := decodeJSONObject(body)
	if err != nil {
		return ""
	}

	msgs, ok := obj["messages"].([]any)
	if !ok {
		return ""
	}

	digest := sha256.New()

	blocks := 0

	for _, raw := range msgs {
		m, ok := raw.(map[string]any)
		if !ok || !isSystemMsg(m) {
			continue
		}

		content, ok := m["content"].(string)
		if !ok {
			// 多模态 part 数组：不当字符串参与哈希（不 marshal 整棵子树，省一次
			// 全量序列化），该块对指纹不贡献。
			continue
		}
		// 每条之间插 NUL 分隔，避免 ["ab","c"] 与 ["a","bc"] 撞同一指纹。
		digest.Write([]byte(content))
		digest.Write([]byte{0})

		blocks++
	}

	if blocks == 0 {
		return ""
	}

	sum := digest.Sum(nil)

	return hex.EncodeToString(sum[:systemFingerprintBytes])
}
