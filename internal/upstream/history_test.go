package upstream

import (
	"encoding/json"
	"reflect"
	"testing"
)

// msg 构造一条测试消息：裁剪逻辑只看 role，tag 仅用于断言切片位置。
func msg(role, tag string) map[string]any {
	return map[string]any{"role": role, "content": tag, "tag": tag}
}

// tags 抽出每条的 tag，用于断言保留下来的消息序列。
func tags(msgs []any) []string {
	out := make([]string, 0, len(msgs))
	for _, m := range msgs {
		tag, _ := m.(map[string]any)["tag"].(string)
		out = append(out, tag)
	}

	return out
}

// toAny 把 []map[string]any 转成 payload 里的 []any 形态。
func toAny(msgs []map[string]any) []any {
	out := make([]any, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, m)
	}

	return out
}

func TestTrimHistoryKeepsHeadSystemAndTailTurns(t *testing.T) {
	t.Parallel()

	msgs := toAny([]map[string]any{
		msg("system", "sys"),
		msg("user", "u1"),
		msg("assistant", "a1"),
		msg("user", "u2"),
		msg("assistant", "a2"),
		msg("user", "u3"),
		msg("assistant", "a3"),
	})

	out, dropped := trimHistory(msgs, 2)
	if want := []string{"sys", "u2", "a2", "u3", "a3"}; !reflect.DeepEqual(tags(out), want) {
		t.Fatalf("keep=2 输出 = %v, want %v", tags(out), want)
	}

	if dropped != 2 {
		t.Fatalf("dropped = %d, want 2（u1 + a1）", dropped)
	}
}

func TestTrimHistoryNoopWhenNotEnoughTurns(t *testing.T) {
	t.Parallel()

	msgs := toAny([]map[string]any{
		msg("system", "sys"),
		msg("user", "u1"),
		msg("assistant", "a1"),
	})

	out, dropped := trimHistory(msgs, 3)
	if dropped != 0 {
		t.Fatalf("dropped = %d, want 0", dropped)
	}

	if len(out) != len(msgs) {
		t.Fatalf("长度 = %d, want %d（原样返回）", len(out), len(msgs))
	}

	// keep <= 0 = 关闭。
	out, dropped = trimHistory(msgs, 0)
	if dropped != 0 || len(out) != len(msgs) {
		t.Fatalf("keep=0：dropped=%d len=%d，want 0/%d", dropped, len(out), len(msgs))
	}
}

func TestTrimHistoryDropsNonContiguousSystemSegment(t *testing.T) {
	t.Parallel()

	// 中间夹着 system 的段必须整体丢弃：只保留「开头连续」的 system。
	msgs := toAny([]map[string]any{
		msg("system", "sys"),
		msg("user", "u1"),
		msg("system", "sys2"),
		msg("assistant", "a1"),
		msg("user", "u2"),
		msg("assistant", "a2"),
	})

	out, dropped := trimHistory(msgs, 1)
	if want := []string{"sys", "u2", "a2"}; !reflect.DeepEqual(tags(out), want) {
		t.Fatalf("输出 = %v, want %v", tags(out), want)
	}

	if dropped != 3 {
		t.Fatalf("dropped = %d, want 3（u1 / sys2 / a1）", dropped)
	}
}

func TestTrimHistoryDeveloperCountsAsSystem(t *testing.T) {
	t.Parallel()

	msgs := toAny([]map[string]any{
		msg("developer", "dev"),
		msg("system", "sys"),
		msg("user", "u1"),
		msg("assistant", "a1"),
		msg("user", "u2"),
	})

	out, dropped := trimHistory(msgs, 1)
	if want := []string{"dev", "sys", "u2"}; !reflect.DeepEqual(tags(out), want) {
		t.Fatalf("输出 = %v, want %v", tags(out), want)
	}

	if dropped != 2 {
		t.Fatalf("dropped = %d, want 2", dropped)
	}
}

func TestTrimHistoryToolAndAssistantNotCounted(t *testing.T) {
	t.Parallel()

	// tool / assistant 不参与轮次计数：keep=2 时两条 user 都保住，不触发裁剪。
	msgs := toAny([]map[string]any{
		msg("system", "sys"),
		msg("user", "u1"),
		msg("assistant", "a1"),
		msg("tool", "t1"),
		msg("tool", "t2"),
		msg("user", "u2"),
		msg("assistant", "a2"),
		msg("tool", "t3"),
	})

	out, dropped := trimHistory(msgs, 2)
	if want := []string{"sys", "u1", "a1", "t1", "t2", "u2", "a2", "t3"}; !reflect.DeepEqual(tags(out), want) {
		t.Fatalf("输出 = %v, want %v（未触发裁剪）", tags(out), want)
	}

	if dropped != 0 {
		t.Fatalf("dropped = %d, want 0", dropped)
	}

	// keep=1 落在倒数第 1 条 user（u2），起点前的 user / assistant / tool 全丢。
	out, dropped = trimHistory(msgs, 1)
	if want := []string{"sys", "u2", "a2", "t3"}; !reflect.DeepEqual(tags(out), want) {
		t.Fatalf("keep=1 输出 = %v, want %v", tags(out), want)
	}

	if dropped != 4 {
		t.Fatalf("dropped = %d, want 4", dropped)
	}
}

func TestTrimHistoryCleansOrphanToolResultsAtBoundary(t *testing.T) {
	t.Parallel()

	// 跨边界配对（极端形态：客户端把 tool 结果放在 user 之后）：带 tool_calls 的
	// assistant 落在被裁掉的一段，tool 结果留在尾部 —— 必须由 cleanupOrphanToolCalls 清掉。
	msgs := []any{
		map[string]any{"role": "system", "tag": "sys"},
		map[string]any{"role": "user", "tag": "u1"},
		map[string]any{
			"role":    "assistant",
			"tag":     "a1",
			"content": "",
			"tool_calls": []any{
				map[string]any{"id": "c1", "type": "function", "function": map[string]any{"name": "f", "arguments": "{}"}},
			},
		},
		map[string]any{"role": "user", "tag": "u2"},
		map[string]any{"role": "tool", "tool_call_id": "c1", "tag": "t1", "content": "ok"},
	}

	trimmed, dropped := trimHistory(msgs, 1)
	if dropped != 2 {
		t.Fatalf("dropped = %d, want 2（u1 + a1）", dropped)
	}

	if want := []string{"sys", "u2", "t1"}; !reflect.DeepEqual(tags(trimmed), want) {
		t.Fatalf("裁剪后 = %v, want %v", tags(trimmed), want)
	}

	cleaned, changed := cleanupOrphanToolCalls(trimmed)
	if !changed {
		t.Fatal("孤儿 tool 结果应被清除")
	}

	if want := []string{"sys", "u2"}; !reflect.DeepEqual(tags(cleaned), want) {
		t.Fatalf("清理后 = %v, want %v", tags(cleaned), want)
	}
}

func TestPrepareBodyTrimsHistory(t *testing.T) {
	t.Parallel()

	body := map[string]any{
		"model": "glm-5.2",
		"messages": []any{
			map[string]any{"role": "system", "content": "SYS"},
			map[string]any{"role": "user", "content": "u1"},
			map[string]any{"role": "assistant", "content": "a1"},
			map[string]any{"role": "user", "content": "u2"},
			map[string]any{"role": "assistant", "content": "a2"},
			map[string]any{"role": "user", "content": "u3"},
		},
	}

	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var got struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}

	out := PrepareBodyOptWithEffortsAndDefault(raw, false, nil, nil, 2)
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if len(got.Messages) != 4 {
		t.Fatalf("消息数 = %d, want 4（system + u2 + a2 + u3）", len(got.Messages))
	}

	if got.Messages[0].Role != "system" || got.Messages[0].Content != "SYS" {
		t.Fatalf("首条必须是原 system，得到 %+v", got.Messages[0])
	}

	if got.Messages[1].Content != "u2" {
		t.Fatalf("第二条 = %q, want u2", got.Messages[1].Content)
	}

	// keep=0 = 关闭：输出必须包含全部 6 条。
	out = PrepareBodyOptWithEffortsAndDefault(raw, false, nil, nil, 0)
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if len(got.Messages) != 6 {
		t.Fatalf("keep=0 消息数 = %d, want 6", len(got.Messages))
	}
}
