package session

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"regexp"
	"strings"
	"testing"
	"time"
)

// imagePartSig 计算 part 原文摘要（sha256 前 8 hex），供期望值精确构造——
// 签名算法变更时测试期望值随此 helper 单点同步。
func imagePartSig(t *testing.T, part string) string {
	t.Helper()

	sum := sha256.Sum256([]byte(part))

	return hex.EncodeToString(sum[:4])
}

// TestExtractSessionConversationID 覆盖 conversationId 提取的 snake/camel/缺失三态：
//   - metadata.conversation_id / metadata.conversationId → 取值
//   - 顶层 conversation_id / conversationId → 取值（snake 优先于 camel，与 ExtractSession 会话键同口径）
//   - 缺失 / 只有 user_id → ""（会话头族语义只认对话 ID，绝不回落 user_id）
func TestExtractSessionConversationID(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		body string
		want string
	}{
		{"metadata snake_case", `{"metadata":{"conversation_id":"conv-1"}}`, "conv-1"},
		{"metadata camelCase", `{"metadata":{"conversationId":"conv-2"}}`, "conv-2"},
		{"top-level snake_case", `{"conversation_id":"conv-3"}`, "conv-3"},
		{"top-level camelCase", `{"conversationId":"conv-4"}`, "conv-4"},
		{"snake wins over camel", `{"conversation_id":"conv-s","conversationId":"conv-c"}`, "conv-s"},
		{"missing", `{"model":"glm-5.2"}`, ""},
		{"empty body", ``, ""},
		{"broken json", `{broken`, ""},
		{"metadata user_id only", `{"metadata":{"user_id":"u1"}}`, ""},
		{"top-level user_id only", `{"user_id":"u1"}`, ""},
		{"empty string value", `{"conversationId":""}`, ""},
		{"non-string value", `{"conversationId":123}`, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()

			if _, got := ExtractSession([]byte(c.body)); got != c.want {
				t.Errorf("ExtractSession(%q) conversationID = %q want %q", c.body, got, c.want)
			}
		})
	}
}

// uuidV7HexPattern 官方 X-Conversation-Request-ID 校验正则（32 位小写 hex、version=7、variant=10xx）。
var uuidV7HexPattern = regexp.MustCompile(`^[0-9a-f]{12}7[0-9a-f]{3}[89ab][0-9a-f]{15}$`)

// uuidV4Pattern 标准 UUIDv4（8-4-4-4-12）。
var uuidV4Pattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

// TestNewUUIDv7HexFormat 64 次采样均匹配官方正则，且两次调用不相等。
func TestNewUUIDv7HexFormat(t *testing.T) {
	t.Parallel()

	prev := NewUUIDv7Hex()
	for range 64 {
		id := NewUUIDv7Hex()
		if !uuidV7HexPattern.MatchString(id) {
			t.Fatalf("NewUUIDv7Hex() = %q 不匹配官方正则", id)
		}

		if id == prev {
			t.Fatalf("连续两次 UUIDv7 相同: %q", id)
		}

		prev = id
	}
}

// TestNewUUIDv7HexTimestamp 前 48 位必须是当前 Unix 毫秒（允许调用前后 1s 抖动）。
func TestNewUUIDv7HexTimestamp(t *testing.T) {
	t.Parallel()

	before := time.Now().UnixMilli()

	id := NewUUIDv7Hex()

	after := time.Now().UnixMilli()

	raw, err := hex.DecodeString(id)
	if err != nil {
		t.Fatalf("DecodeString(%q): %v", id, err)
	}

	var high [8]byte
	copy(high[2:], raw[:6])

	got := int64(binary.BigEndian.Uint64(high[:]))
	if got < before || got > after {
		t.Fatalf("UUIDv7 时间戳=%d 不在 [%d,%d]（前 48 位未写入当前毫秒）", got, before, after)
	}
}

// TestNewUUIDv4Format 64 次采样均匹配标准 UUIDv4。
func TestNewUUIDv4Format(t *testing.T) {
	t.Parallel()

	for range 64 {
		id := NewUUIDv4()
		if !uuidV4Pattern.MatchString(id) {
			t.Fatalf("NewUUIDv4() = %q 不匹配 UUIDv4", id)
		}
	}
}

// TestNewMessageIDFormat 32 位 hex 且不为空、两次调用大概率不同（随机性冒烟）。
func TestNewMessageIDFormat(t *testing.T) {
	t.Parallel()

	for range 50 {
		id := NewMessageID()
		if len(id) != 32 {
			t.Fatalf("NewMessageID() = %q len=%d want 32", id, len(id))
		}

		for _, ch := range id {
			if !strings.ContainsRune("0123456789abcdef", ch) {
				t.Fatalf("NewMessageID() = %q has non-hex char %q", id, ch)
			}
		}
	}
}

// TestRequestIDForKeyStability 同 key 恒稳定、异 key 各不同、空 key 每次新值。
func TestRequestIDForKeyStability(t *testing.T) {
	t.Parallel()
	// 预热清空包级缓存，避免其他测试污染 key（测试隔离）。
	k1a := RequestIDForKey("conv-a")

	k1b := RequestIDForKey("conv-a")
	if k1a != k1b {
		t.Errorf("same key should be stable: %q vs %q", k1a, k1b)
	}

	str2 := RequestIDForKey("conv-b")
	if k1a == str2 {
		t.Errorf("different keys should differ: %q", k1a)
	}
	// 空 key：每次调用生成新值（无会话则无"会话内稳定"语义）。
	str := RequestIDForKey("")

	e2 := RequestIDForKey("")
	if str == e2 {
		t.Errorf("empty key should yield fresh values each call: %q", str)
	}
	// 稳定值自身也须是 UUIDv7 形态的 32 hex（可作 B3 TraceId 直接使用）。
	for _, id := range []string{k1a, str2, str} {
		if !uuidV7HexPattern.MatchString(id) {
			t.Errorf("RequestIDForKey value %q 不匹配 UUIDv7", id)
		}
	}
}

// TestTurnKeyExtraction 轮级兜底键的提取：取**最后一条** user 消息的「序号+文本」，
// 轮内追加 assistant/tool 消息不改变键；无 user / 无文本 / 坏 JSON 一律空串。
func TestTurnKeyExtraction(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		body string
		want string
	}{
		{"single user", `{"messages":[{"role":"user","content":"你好"}]}`, "u0:你好"},
		{"last user wins", `{"messages":[{"role":"user","content":"第一问"},{"role":"assistant","content":"答"},{"role":"user","content":"第二问"}]}`, "u2:第二问"},
		// agent 多步：轮内追加 assistant/tool 消息，末条 user 位置与内容不变 → 同键。
		{"agent step keeps same key", `{"messages":[{"role":"user","content":"任务"},{"role":"assistant","tool_calls":[{"id":"c1"}]},{"role":"tool","content":"结果"}]}`, "u0:任务"},
		{"multimodal parts text joined", `{"messages":[{"role":"user","content":[{"type":"text","text":"看图"},{"type":"image_url","image_url":{"url":"data:x"}}]}]}`, "u0:看图\n[image_url:" + imagePartSig(t, `{"type":"image_url","image_url":{"url":"data:x"}}`) + "]"},
		{"no user message", `{"messages":[{"role":"system","content":"sys"}]}`, ""},
		{"empty messages", `{"messages":[]}`, ""},
		{"messages key absent", `{"model":"glm-5.2"}`, ""},
		{"broken json", `{broken`, ""},
		{"empty body", ``, ""},
		{"empty content", `{"messages":[{"role":"user","content":""}]}`, ""},
		{"null content", `{"messages":[{"role":"user","content":null}]}`, ""},
		// 纯图片 content 按内容签名派生非空轮级键（G1 修复，原为 ""）；键含 image part 摘要。
		{"image only content", `{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"x"}}]}]}`, "u0:[image_url:" + imagePartSig(t, `{"type":"image_url","image_url":{"url":"x"}}`) + "]"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()

			if got := TurnKey([]byte(c.body)); got != c.want {
				t.Errorf("TurnKey(%q) = %q want %q", c.body, got, c.want)
			}
		})
	}
}

// TestTurnRequestIDDerivation 轮级 ID 是纯派生：同键恒同值、异键异值、空键每次新值、
// 形态恒 32 hex。
func TestTurnRequestIDDerivation(t *testing.T) {
	t.Parallel()

	str := TurnRequestID("u0:同一个问题")

	a2 := TurnRequestID("u0:同一个问题")
	if str != a2 {
		t.Errorf("same turn key should derive same id: %q vs %q", str, a2)
	}

	if b := TurnRequestID("u0:另一个问题"); b == str {
		t.Errorf("different turn keys should derive different ids: %q", b)
	}
	// 同文本但序号不同（不同轮里内容相同的提问）也要分开。
	if c := TurnRequestID("u2:同一个问题"); c == str {
		t.Errorf("same text at different position should differ: %q", c)
	}
	// 空键：无轮可聚合 → 每次新值（保持原有请求级独立行为）。
	if e1, e2 := TurnRequestID(""), TurnRequestID(""); e1 == e2 {
		t.Errorf("empty turn key should yield fresh values each call: %q", e1)
	}

	for _, id := range []string{str, TurnRequestID(""), TurnRequestID("x")} {
		if len(id) != 32 {
			t.Errorf("TurnRequestID value %q len=%d want 32", id, len(id))
		}
	}
}

// uuidV7HyphenTestPattern 带连字符 UUIDv7：第 13 位为 7、变体位 10xx。
var uuidV7HyphenTestPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

// TestNewUUIDv7Format 带连字符 UUIDv7：形态、版本位、变体位，且两次调用不相等。
func TestNewUUIDv7Format(t *testing.T) {
	t.Parallel()

	prev := NewUUIDv7()

	for range 32 {
		id := NewUUIDv7()
		if !uuidV7HyphenTestPattern.MatchString(id) {
			t.Fatalf("NewUUIDv7() = %q 不匹配带连字符 UUIDv7", id)
		}

		if id == prev {
			t.Fatalf("NewUUIDv7() 连续两次相同: %q", id)
		}

		prev = id
	}
}

// TestNewUUIDv7Timestamp 前 48 bit 必须是当前 Unix 毫秒（允许调用前后 1s 抖动）。
func TestNewUUIDv7Timestamp(t *testing.T) {
	t.Parallel()

	before := time.Now().UnixMilli()

	id := NewUUIDv7()

	after := time.Now().UnixMilli()

	if got := uuidTimestampMillis(t, id); got < before || got > after {
		t.Fatalf("UUIDv7 时间戳=%d 不在 [%d,%d]", got, before, after)
	}
}

// TestUUIDv7AtStableAndPlausible 同种子 + 同基准恒同值；异种子互异；时间戳前缀
// 落在「基准时刻前 1 小时」窗口内（可信时间）。
func TestUUIDv7AtStableAndPlausible(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 10, 28, 10, 0, 0, 0, time.UTC)
	seed := "wb2a:acp:0f0e0d0c-0b0a-4908-8706-050403020100"

	first := UUIDv7At(seed, base)
	second := UUIDv7At(seed, base)

	if first != second {
		t.Fatalf("同种子派生不一致: %q != %q", first, second)
	}

	if !uuidV7HyphenTestPattern.MatchString(first) {
		t.Fatalf("UUIDv7At() = %q 不匹配带连字符 UUIDv7", first)
	}

	if UUIDv7At("wb2a:acp:other", base) == first {
		t.Fatal("不同种子派生出相同值")
	}

	got := uuidTimestampMillis(t, first)
	lower := base.Add(-time.Hour).UnixMilli()

	if got < lower || got > base.UnixMilli() {
		t.Fatalf("派生时间戳=%d 不在 [%d,%d]（基准前 1 小时窗口）", got, lower, base.UnixMilli())
	}
}

// uuidTimestampMillis 解析带连字符 UUIDv7 前 48 bit 的毫秒时间戳。
func uuidTimestampMillis(t *testing.T, id string) int64 {
	t.Helper()

	raw, err := hex.DecodeString(strings.ReplaceAll(id, "-", ""))
	if err != nil {
		t.Fatalf("DecodeString(%q): %v", id, err)
	}

	var high [8]byte
	copy(high[2:], raw[:6])

	return int64(binary.BigEndian.Uint64(high[:]))
}
