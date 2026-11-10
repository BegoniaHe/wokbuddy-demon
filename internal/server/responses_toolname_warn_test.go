// responses_toolname_warn_test.go 钉住 responses 工具调用缺名的可观测性（issue #489）：
// 报错/丢弃前必须有可归因的 WARN，且错误语义不变。
package server

import (
	"strings"
	"testing"
)

// wantLogFragments 断言捕获到的日志包含全部片段。
func wantLogFragments(t *testing.T, out string, fragments ...string) {
	t.Helper()

	for _, fragment := range fragments {
		if !strings.Contains(out, fragment) {
			t.Errorf("log missing %q\nlog:\n%s", fragment, out)
		}
	}
}

// TestResponsesFunctionCallMissingNameWarns 回放的 function_call 缺 name：400 语义
// 不变，同时留下带模型/下标/call_id 的 WARN。
//
//nolint:paralleltest // 该用例改写进程级日志出口，必须串行执行
func TestResponsesFunctionCallMissingNameWarns(t *testing.T) {
	logs := captureLog(t)

	body := []byte(`{"model":"glm-5.2","input":[{"type":"function_call","call_id":"call_1","arguments":"{}"}]}`)

	if _, _, _, _, err := responsesToOpenAIWithClient(body, nil, nil); err == nil {
		t.Fatal("缺函数名的 function_call 必须保持 400 语义")
	}

	wantLogFragments(t, logs.String(),
		"responses tool call missing function name", "model=glm-5.2", "index=0", "call_1")
}

// TestResponsesCustomToolCallMissingNameWarns custom_tool_call 缺 name 同样告警。
//
//nolint:paralleltest // 该用例改写进程级日志出口，必须串行执行
func TestResponsesCustomToolCallMissingNameWarns(t *testing.T) {
	logs := captureLog(t)

	body := []byte(`{"model":"glm-5.2","input":[{"type":"custom_tool_call","id":"ct_1","input":"patch"}]}`)

	if _, _, _, _, err := responsesToOpenAIWithClient(body, nil, nil); err == nil {
		t.Fatal("缺函数名的 custom_tool_call 必须保持 400 语义")
	}

	wantLogFragments(t, logs.String(), "responses tool call missing function name", "ct_1")
}

// TestResponsesCustomToolDeclarationWithoutNameWarns 缺名的 custom 工具声明被丢弃时
// 留 WARN（否则模型看不到该工具，排障无从归因）。
//
//nolint:paralleltest // 该用例改写进程级日志出口，必须串行执行
func TestResponsesCustomToolDeclarationWithoutNameWarns(t *testing.T) {
	logs := captureLog(t)

	body := []byte(`{"model":"glm-5.2","tools":[{"type":"custom"}]}`)
	if got := responsesCustomTools(body); len(got) != 0 {
		t.Fatalf("无名 custom 工具不应进入名单: %v", got)
	}

	wantLogFragments(t, logs.String(), "custom tool without name")
}
