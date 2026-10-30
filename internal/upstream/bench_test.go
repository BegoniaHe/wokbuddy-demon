package upstream

import (
	"encoding/json"
	"strings"
	"testing"
)

// buildLargeBody 构造接近真实量级的 chat body（约 124KB：tools + 长 system + 20 轮消息）。
func buildLargeBody(b *testing.B) []byte {
	b.Helper()

	msgs := make([]map[string]any, 0, 20)

	msgs = append(msgs, map[string]any{"role": "system", "content": strings.Repeat("系统提示。", 1200)})
	for range 19 {
		msgs = append(msgs, map[string]any{"role": "user", "content": strings.Repeat("用户提问内容。", 200)})
	}

	tools := make([]map[string]any, 0, 8)
	for i := range 8 {
		tools = append(tools, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        "tool_" + string(rune('a'+i)),
				"description": strings.Repeat("工具描述。", 100),
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"q": map[string]any{"type": "string", "description": strings.Repeat("参数说明。", 100)},
					},
				},
			},
		})
	}

	raw, err := json.Marshal(map[string]any{"model": "glm-5.2", "messages": msgs, "tools": tools})
	if err != nil {
		b.Fatal(err)
	}

	return raw
}

// BenchmarkRewriteChatBody 单轮解码改写 124KB 真实体（20 轮消息 + 8 个工具）：
// 守住「一次解码、一次编码」——每多一轮整解整编，这里会成倍变慢。
func BenchmarkRewriteChatBody(b *testing.B) {
	body := buildLargeBody(b)

	rewrite := chatBodyRewrite{
		leadingSystem: true, injectCacheKey: true, uid: "user-abc-123", conversationID: "conv-bench",
	}

	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	b.ResetTimer()

	for range b.N {
		_ = rewriteChatBody(body, &rewrite)
	}
}
