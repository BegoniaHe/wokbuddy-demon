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

// TestGatewayHintToolDefinitionRejection 工具定义侧的请求级拒收（11129 工具定义不合规 /
// 11152 工具名称不规范）：只在请求确实声明了 tools 时给「检查 tools」的指向；11101
// 请求体畸形不给（否则客户端会去翻自己的 schema）。
func TestGatewayHintToolDefinitionRejection(t *testing.T) {
	t.Parallel()

	bodies := []struct {
		name string
		body string
	}{
		{"11129 业务码", `{"code":11129,"msg":"Invalid tool definition. Start a new task and retry"}`},
		{"11152 业务码", `{"code":11152,"msg":"Invalid tool names. Start a new task and retry"}`},
		{"11152 中文文案", `{"code":11152,"msg":"工具名称不规范，请新建任务重试"}`},
		{"11129 数字 code 无空格", `{"code":11129,"msg":"bad tool"}`},
	}

	for _, item := range bodies {
		kind := Classify(400, item.body)
		if kind != ErrBadParams {
			t.Fatalf("%s: 分类变了: %v", item.name, kind)
		}

		hint := GatewayHint(kind, item.body, HintContext{HasTools: true})
		if !strings.Contains(hint, "tools[].function.parameters") {
			t.Fatalf("%s: 未声明 tools 指向: %q", item.name, hint)
		}

		// 没声明 tools 的请求不能按它指去改 tools。
		if got := GatewayHint(kind, item.body, HintContext{}); got != "" {
			t.Fatalf("%s: 未声明 tools 却给了 hint: %q", item.name, got)
		}
	}

	// 11101 请求体畸形：即使声明了 tools 也不指向 schema。
	badParams := `{"code":11101,"msg":"Unmarshal chat params failed with error: unexpected EOF"}`
	if got := GatewayHint(ErrBadParams, badParams, HintContext{HasTools: true}); got != "" {
		t.Fatalf("11101 被指去改 tools: %q", got)
	}

	// 上游丢业务码只留文案（官方 i18n 原文）时同样命中，但仍以 hasTools 为前提。
	textOnly := `{"msg":"Invalid tool definition. Start a new task and retry"}`
	if got := GatewayHint(ErrBadParams, textOnly, HintContext{HasTools: true}); !strings.Contains(got, "tools[].function.parameters") {
		t.Fatalf("文案形态未命中: %q", got)
	}

	if got := GatewayHint(ErrBadParams, textOnly, HintContext{}); got != "" {
		t.Fatalf("文案形态在未声明 tools 时给了 hint: %q", got)
	}
}

// TestBusinessFrameError 钉住顶层业务错误信封的判定口径：code 字符串/数字均可，
// 0 与空串不是错误，error/choices 帧归各自的既有分支处理。
func TestBusinessFrameError(t *testing.T) {
	t.Parallel()

	for _, want := range []struct {
		name string
		body string
		code string
		msg  string
		ok   bool
	}{
		{"数字 code", `{"code":10001,"msg":"quota exceeded"}`, "10001", "quota exceeded", true},
		{"字符串 code", `{"code":" 10001 ","msg":"quota exceeded"}`, "10001", "quota exceeded", true},
		{"message 兜底", `{"code":10001,"message":"quota exceeded"}`, "10001", "quota exceeded", true},
		{"零 code", `{"code":0,"msg":"ok"}`, "", "", false},
		{"零字符串 code", `{"code":"0","msg":"ok"}`, "", "", false},
		{"空 code", `{"code":"","msg":"ok"}`, "", "", false},
		{"缺 code", `{"msg":"ok"}`, "", "", false},
		{"缺 msg", `{"code":10001}`, "", "", false},
		{"error 帧不在此列", `{"code":10001,"msg":"x","error":{"code":1}}`, "", "", false},
		{"choices 帧不在此列", `{"code":10001,"msg":"x","choices":[]}`, "", "", false},
		{"非 JSON", `not json`, "", "", false},
		{"顶层 null", `null`, "", "", false},
	} {
		t.Run(want.name, func(t *testing.T) {
			t.Parallel()

			code, msg, ok := BusinessFrameError(want.body)
			if code != want.code || msg != want.msg || ok != want.ok {
				t.Fatalf("got (%q,%q,%v), want (%q,%q,%v)", code, msg, ok, want.code, want.msg, want.ok)
			}
		})
	}
}
