package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
)

// responses_shadow_test.go 实现一个与客户端同口径的 Responses SSE 影子解析器：
// 按 event:/data: 行解析 JSON，用 output_index 维护槽位，只接受已登记槽位的增量，
// 在 done 事件校验最终参数，并要求流内恰好一个终止事件。测试辅助只存在于 _test.go。

type shadowEvent struct {
	name string
	data map[string]any
}

type shadowSlot struct {
	kind       string // message / function / custom
	text       strings.Builder
	args       strings.Builder
	done       bool
	finalArgs  string
	finalInput string
}

type shadowConsumer struct {
	slots     map[int]*shadowSlot
	terminal  string
	errorSeen bool
	failed    bool
}

func shadowParseEvents(t *testing.T, raw string) []shadowEvent {
	t.Helper()

	var (
		events    []shadowEvent
		name      string
		dataLines []string
	)

	flush := func() {
		if name == "" && len(dataLines) == 0 {
			return
		}

		payload := strings.Join(dataLines, "\n")

		var data map[string]any
		if err := json.Unmarshal([]byte(payload), &data); err != nil {
			t.Fatalf("解析 SSE data 失败: %v\npayload=%s", err, payload)
		}

		events = append(events, shadowEvent{name: name, data: data})
		name = ""
		dataLines = nil
	}

	for line := range strings.SplitSeq(raw, "\n") {
		line = strings.TrimRight(line, "\r")
		switch {
		case line == "":
			flush()
		case strings.HasPrefix(line, "event: "):
			name = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			dataLines = append(dataLines, strings.TrimPrefix(line, "data: "))
		}
	}

	flush()

	return events
}

func shadowDeltaName(name string) bool {
	switch name {
	case "response.output_text.delta", "response.function_call_arguments.delta",
		"response.custom_tool_call_input.delta":
		return true
	default:
		return false
	}
}

func shadowTerminalName(name string) bool {
	switch name {
	case "response.completed", "response.incomplete", "response.failed":
		return true
	default:
		return false
	}
}

//
//nolint:err113 // 测试解析器用动态错误描述具体违例。
func (c *shadowConsumer) slot(index int, kind string) (*shadowSlot, error) {
	slot := c.slots[index]
	if slot == nil {
		return nil, fmt.Errorf("output_index=%d 的增量早于 output_item.added", index)
	}

	if slot.kind != kind {
		return nil, fmt.Errorf("output_index=%d 类型不匹配: slot=%s event=%s", index, slot.kind, kind)
	}

	return slot, nil
}

//
//nolint:err113,cyclop // 测试解析器按事件类型平铺断言，动态错误用于定位违例。
func (c *shadowConsumer) consume(event shadowEvent) error {
	if c.terminal != "" {
		return fmt.Errorf("终止事件 %s 之后仍出现 %s", c.terminal, event.name)
	}

	index := intValue(event.data["output_index"])

	switch event.name {
	case "error":
		c.errorSeen = true
		return nil
	case "response.output_item.added":
		item, _ := event.data["item"].(map[string]any)

		kind := stringValue(item["type"])
		if kind == "" {
			return fmt.Errorf("output_item.added 缺少 item.type (index=%d)", index)
		}

		switch kind {
		case "function_call":
			kind = "function"
		case "custom_tool_call":
			kind = "custom"
		}

		if c.slots[index] != nil {
			return fmt.Errorf("output_index=%d 重复 added", index)
		}

		c.slots[index] = &shadowSlot{kind: kind}

		return nil
	case "response.output_text.delta":
		slot, err := c.slot(index, "message")
		if err != nil {
			return err
		}

		slot.text.WriteString(stringValue(event.data["delta"]))

		return nil
	case "response.function_call_arguments.delta":
		slot, err := c.slot(index, "function")
		if err != nil {
			return err
		}

		slot.args.WriteString(stringValue(event.data["delta"]))

		return nil
	case "response.custom_tool_call_input.delta":
		slot, err := c.slot(index, "custom")
		if err != nil {
			return err
		}

		slot.args.WriteString(stringValue(event.data["delta"]))

		return nil
	case "response.function_call_arguments.done":
		slot, err := c.slot(index, "function")
		if err != nil {
			return err
		}

		got, want := stringValue(event.data["arguments"]), slot.args.String()
		if got != want {
			return fmt.Errorf("function 参数重建不一致 index=%d got=%q want=%q", index, got, want)
		}

		slot.done, slot.finalArgs = true, got

		return nil
	case "response.custom_tool_call_input.done":
		slot, err := c.slot(index, "custom")
		if err != nil {
			return err
		}

		got := stringValue(event.data["input"])
		if printed := slot.args.String(); printed != "" && printed != got {
			return fmt.Errorf("custom input 重建不一致 index=%d got=%q want=%q", index, got, printed)
		}

		slot.done, slot.finalInput = true, got

		return nil
	case "response.output_item.done":
		item, _ := event.data["item"].(map[string]any)

		slot := c.slots[index]
		if slot == nil {
			return fmt.Errorf("output_index=%d 的 done 早于 added", index)
		}

		switch slot.kind {
		case "message":
			var text strings.Builder

			parts, _ := item["content"].([]any)
			for _, part := range parts {
				if m, ok := part.(map[string]any); ok && m["type"] == typeOutputText {
					text.WriteString(stringValue(m["text"]))
				}
			}

			if text.String() != slot.text.String() {
				return fmt.Errorf("message 文本重建不一致 index=%d got=%q want=%q",
					index, text.String(), slot.text.String())
			}
		case "function":
			if stringValue(item["arguments"]) != slot.args.String() {
				return fmt.Errorf("function item 参数不一致 index=%d", index)
			}
		case "custom":
			if stringValue(item["input"]) != slot.finalInput {
				return fmt.Errorf("custom item input 不一致 index=%d", index)
			}
		}

		return nil
	case "response.completed", "response.incomplete", "response.failed":
		c.terminal = event.name
		c.failed = event.name == "response.failed"

		return nil
	default:
		return nil
	}
}

//
//nolint:err113 // 测试解析器用动态错误描述具体违例。
func (c *shadowConsumer) finish() error {
	if c.terminal == "" {
		return errors.New("流缺少终止事件")
	}

	if c.failed && !c.errorSeen {
		return errors.New("response.failed 之前缺少 error 事件")
	}

	if c.errorSeen && c.terminal == "response.completed" {
		return errors.New("错误路径不得以 response.completed 终止")
	}

	return nil
}

func runShadowWriter(t *testing.T, chunks string, customTools map[string]bool) ([]shadowEvent, *shadowConsumer) {
	t.Helper()

	rec := httptest.NewRecorder()
	w := newResponsesStreamWriter(rec, "glm-5.2", "sys", customTools, nil)

	if _, err := w.Write([]byte(chunks)); err != nil {
		t.Fatalf("writer: %v", err)
	}

	events := shadowParseEvents(t, rec.Body.String())

	consumer := &shadowConsumer{slots: map[int]*shadowSlot{}}
	for _, event := range events {
		if err := consumer.consume(event); err != nil {
			t.Fatalf("影子解析失败: %v\nstream=\n%s", err, rec.Body)
		}
	}

	if err := consumer.finish(); err != nil {
		t.Fatalf("流终止校验失败: %v\nstream=\n%s", err, rec.Body)
	}

	return events, consumer
}

func TestResponsesShadowText(t *testing.T) {
	t.Parallel()

	chunks := `data: {"id":"c1","choices":[{"index":0,"delta":{"content":"he"}}]}` + "\n\n" +
		`data: {"id":"c1","choices":[{"index":0,"delta":{"content":"llo"}}]}` + "\n\n" +
		`data: {"id":"c1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n" +
		"data: [DONE]\n\n"

	_, consumer := runShadowWriter(t, chunks, nil)

	slot := consumer.slots[0]
	if slot == nil || slot.kind != "message" || slot.text.String() != "hello" {
		t.Fatalf("文本重建=%+v", slot)
	}

	if consumer.terminal != "response.completed" {
		t.Fatalf("terminal=%q", consumer.terminal)
	}
}

func TestResponsesShadowParallelTools(t *testing.T) {
	t.Parallel()

	chunks := `data: {"id":"c1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_a","function":{"name":"alpha","arguments":"{\"x\":"}}]}}]}` + "\n\n" +
		`data: {"id":"c1","choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"id":"call_b","function":{"name":"beta","arguments":"{\"y\":"}}]}}]}` + "\n\n" +
		`data: {"id":"c1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"1}"}}]}}]}` + "\n\n" +
		`data: {"id":"c1","choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"function":{"arguments":"2}"}}]}}]}` + "\n\n" +
		`data: {"id":"c1","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}` + "\n\n" +
		"data: [DONE]\n\n"

	_, consumer := runShadowWriter(t, chunks, nil)

	if len(consumer.slots) != 2 {
		t.Fatalf("slots=%d want 2", len(consumer.slots))
	}

	if got := consumer.slots[0]; got == nil || got.kind != "function" || got.finalArgs != `{"x":1}` || !got.done {
		t.Fatalf("slot0=%+v", got)
	}

	if got := consumer.slots[1]; got == nil || got.kind != "function" || got.finalArgs != `{"y":2}` || !got.done {
		t.Fatalf("slot1=%+v", got)
	}
}

func TestResponsesShadowTextAndTools(t *testing.T) {
	t.Parallel()

	chunks := `data: {"id":"c1","choices":[{"index":0,"delta":{"content":"hi"}}]}` + "\n\n" +
		`data: {"id":"c1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"lookup","arguments":"{\"q\":"}}]}}]}` + "\n\n" +
		`data: {"id":"c1","choices":[{"index":0,"delta":{"content":" there"}}]}` + "\n\n" +
		`data: {"id":"c1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"1}"}}]}}]}` + "\n\n" +
		`data: {"id":"c1","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}` + "\n\n" +
		"data: [DONE]\n\n"

	_, consumer := runShadowWriter(t, chunks, nil)

	if got := consumer.slots[0]; got == nil || got.kind != "message" || got.text.String() != "hi there" {
		t.Fatalf("message slot=%+v", got)
	}

	if got := consumer.slots[1]; got == nil || got.kind != "function" || got.finalArgs != `{"q":1}` {
		t.Fatalf("tool slot=%+v", got)
	}
}

func TestResponsesShadowCustomTool(t *testing.T) {
	t.Parallel()

	chunks := `data: {"id":"c1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"apply_patch","arguments":"{\"input\":\"*** Begin Patch\"}"}}]}}]}` + "\n\n" +
		`data: {"id":"c1","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}` + "\n\n" +
		"data: [DONE]\n\n"

	_, consumer := runShadowWriter(t, chunks, map[string]bool{"apply_patch": true})

	slot := consumer.slots[0]
	if slot == nil || slot.kind != "custom" || slot.finalInput != "*** Begin Patch" || !slot.done {
		t.Fatalf("custom slot=%+v", slot)
	}
}

// TestResponsesShadowUpstreamError response.failed 是终止态：已输出内容保留，
// 失败后的增量与 completed 都被忽略，且流内只有一个终止事件。
//
//nolint:cyclop // 逐事件断言，分支用于定位违例。
func TestResponsesShadowUpstreamError(t *testing.T) {
	t.Parallel()

	chunks := `data: {"id":"c1","choices":[{"index":0,"delta":{"content":"part"}}]}` + "\n\n" +
		`data: {"error":{"message":"boom","code":"upstream_error"}}` + "\n\n" +
		`data: {"id":"c1","choices":[{"index":0,"delta":{"content":"after"}}]}` + "\n\n" +
		`data: {"id":"c1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n" +
		"data: [DONE]\n\n"

	events, consumer := runShadowWriter(t, chunks, nil)

	if got := consumer.slots[0]; got == nil || got.text.String() != "part" {
		t.Fatalf("已输出内容应保留: %+v", got)
	}

	if consumer.terminal != "response.failed" {
		t.Fatalf("terminal=%q", consumer.terminal)
	}

	if !consumer.errorSeen {
		t.Fatal("failed 前应有 error")
	}

	terminals := 0

	for _, event := range events {
		if shadowTerminalName(event.name) {
			terminals++
		}

		if event.name == "response.output_text.delta" && stringValue(event.data["delta"]) == "after" {
			t.Fatal("response.failed 后仍输出了增量")
		}

		if event.name == "response.completed" {
			t.Fatal("错误路径不得出现 response.completed")
		}
	}

	if terminals != 1 {
		t.Fatalf("终止事件数量=%d want 1", terminals)
	}
}

// TestResponsesShadowRejectsMalformed 合成异常 SSE，证明解析器能检测回归。
func TestResponsesShadowRejectsMalformed(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		sse  string
	}{
		{
			name: "delta 早于 added",
			sse: `event: response.output_text.delta
data: {"type":"response.output_text.delta","output_index":0,"delta":"x"}

event: response.completed
data: {"type":"response.completed","response":{}}

`,
		},
		{
			name: "未知 output_index",
			sse: `event: response.output_item.added
data: {"type":"response.output_item.added","output_index":0,"item":{"type":"message"}}

event: response.output_text.delta
data: {"type":"response.output_text.delta","output_index":1,"delta":"x"}

event: response.completed
data: {"type":"response.completed","response":{}}

`,
		},
		{
			name: "缺少终止事件",
			sse: `event: response.output_item.added
data: {"type":"response.output_item.added","output_index":0,"item":{"type":"message"}}

event: response.output_text.delta
data: {"type":"response.output_text.delta","output_index":0,"delta":"x"}

`,
		},
		{
			name: "error 后补 completed",
			sse: `event: error
data: {"type":"error","message":"boom"}

event: response.completed
data: {"type":"response.completed","response":{}}

`,
		},
		{
			name: "failed 后第二个终止事件",
			sse: `event: error
data: {"type":"error","message":"boom"}

event: response.failed
data: {"type":"response.failed","response":{}}

event: response.completed
data: {"type":"response.completed","response":{}}

`,
		},
		{
			name: "delta 类型不匹配",
			sse: `event: response.output_item.added
data: {"type":"response.output_item.added","output_index":0,"item":{"type":"message"}}

event: response.function_call_arguments.delta
data: {"type":"response.function_call_arguments.delta","output_index":0,"delta":"x"}

event: response.completed
data: {"type":"response.completed","response":{}}

`,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			events := shadowParseEvents(t, testCase.sse)
			consumer := &shadowConsumer{slots: map[int]*shadowSlot{}}

			var err error

			for _, event := range events {
				if err = consumer.consume(event); err != nil {
					break
				}
			}

			if err == nil {
				err = consumer.finish()
			}

			if err == nil {
				t.Fatalf("异常流未被检测: %+v", events)
			}
		})
	}
}
