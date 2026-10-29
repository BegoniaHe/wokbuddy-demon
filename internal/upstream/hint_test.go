package upstream

import (
	"strings"
	"testing"
)

// 11133 的两条 hint 分支：工具 schema 校验要说得清「不是请求体语法」，其余 11133
// 保持既有中性文案（不编造根因）。
func TestGatewayHintModelParamInvalidToolSchema(t *testing.T) {
	t.Parallel()

	toolBody := `{"code":11133,"msg":"Invalid request parameters","extError":{"code":"400001",` +
		`"message":"the request parameters were rejected by the model provider"}}`

	hint := GatewayHint(ErrBadParams, toolBody, HintContext{Model: "kimi-k3.1-pro", HasTools: true})
	if !strings.Contains(hint, "tool definitions") || !strings.Contains(hint, "not request body syntax") {
		t.Fatalf("工具 schema 形态的 hint 未说清根因: %q", hint)
	}

	// param 指向工具参数的形态同样命中（上游丢 extError 文案时）。
	paramBody := `{"code":11133,"msg":"Invalid request parameters","extError":{"param":"tools[0].function.parameters"}}`
	if got := GatewayHint(ErrBadParams, paramBody, HintContext{HasTools: true}); !strings.Contains(got, "tool definitions") {
		t.Fatalf("param 指向工具 schema 时 hint 未命中: %q", got)
	}

	// 请求没声明 tools 时不能按「供应商侧拒收」文案指向 tools：那句文案对任何参数错
	// 都通用（实测纯文本请求撞 11133 也是它），指去改 tools 只会误导。
	if got := GatewayHint(ErrBadParams, toolBody, HintContext{}); strings.Contains(got, "tool definitions") {
		t.Fatalf("未声明 tools 的请求被指去改 tools: %q", got)
	}

	// 未声明 tools 但 param 明确指向工具参数：仍是工具 schema 问题，照旧命中。
	if got := GatewayHint(ErrBadParams, paramBody, HintContext{}); !strings.Contains(got, "tool definitions") {
		t.Fatalf("param 明确指向工具参数时 hint 未命中: %q", got)
	}

	// 普通 11133（缺 messages / 通用参数错）保持既有中性文案，不被工具说法误伤。
	for _, body := range []string{
		`{"code":11133,"msg":"messages is required","extError":{"param":"messages"}}`,
		`{"code":11133,"msg":"model failed to process request"}`,
	} {
		got := GatewayHint(ErrBadParams, body, HintContext{})
		if strings.Contains(got, "tool definitions") {
			t.Fatalf("普通 11133 被误判成工具 schema: %q (body %s)", got, body)
		}

		if !strings.Contains(got, "request parameters were rejected by the model provider") {
			t.Fatalf("普通 11133 的中性 hint 被改动: %q", got)
		}
	}
}
