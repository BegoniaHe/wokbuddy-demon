package upstream

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"regexp"
	"testing"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
)

var (
	uuidV4HeaderPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	uuidV7HeaderPattern = regexp.MustCompile(`^[0-9a-f]{12}7[0-9a-f]{3}[89ab][0-9a-f]{15}$`)
)

// TestConversationHeaderFallback 无客户端 conversationId 时，出站头使用
// ConversationHeaderID 兜底，并同步派生 Acp-Connection-Id。
func TestConversationHeaderFallback(t *testing.T) {
	t.Parallel()

	client := New()

	req, err := http.NewRequest(http.MethodPost, "https://example.invalid/v2/chat/completions", http.NoBody)
	if err != nil {
		t.Fatal(err)
	}

	client.ChatHeaders(req, &auth.Auth{UID: "u1", AccessToken: "at"}, "", ChatMeta{
		ConversationHeaderID:  "turn-id",
		ConversationRequestID: "turn-id",
	})

	if got := req.Header.Get("X-Conversation-Id"); got != "turn-id" {
		t.Fatalf("X-Conversation-Id=%q want turn-id", got)
	}

	if got := req.Header.Get("Acp-Connection-Id"); got == "" {
		t.Fatal("Acp-Connection-Id missing")
	}
}

// TestConversationHeaderAlwaysEmitted 零值 meta（搜索/夜猫子/模型探测等直接
// ChatHeaders 的路径）也必须带 X-Conversation-Id：上游渠道校验要求该头恒在。
// 会话 ID 兜底为 UUIDv4，请求/消息 ID 为 UUIDv7 32 hex。
func TestConversationHeaderAlwaysEmitted(t *testing.T) {
	t.Parallel()

	client := New()

	req, err := http.NewRequest(http.MethodPost, "https://example.invalid/v2/chat/completions", http.NoBody)
	if err != nil {
		t.Fatal(err)
	}

	client.ChatHeaders(req, &auth.Auth{UID: "u1", AccessToken: "at"}, "", ChatMeta{})

	if got := req.Header.Get("X-Conversation-Id"); !uuidV4HeaderPattern.MatchString(got) {
		t.Fatalf("X-Conversation-Id=%q want UUIDv4", got)
	}

	for _, name := range []string{"X-Conversation-Request-Id", "X-Conversation-Message-Id", "X-Request-ID"} {
		if got := req.Header.Get(name); !uuidV7HeaderPattern.MatchString(got) {
			t.Fatalf("%s=%q want UUIDv7 32 hex", name, got)
		}
	}

	if got := req.Header.Get("Acp-Connection-Id"); !uuidV4HeaderPattern.MatchString(got) {
		t.Fatalf("Acp-Connection-Id=%q want UUIDv4", got)
	}
}

// TestAccountStableHeadersUUIDShape 设备/会话头按 uid 稳定派生且为连字符 UUIDv4。
func TestAccountStableHeadersUUIDShape(t *testing.T) {
	t.Parallel()

	client := New()
	a := &auth.Auth{UID: "u-stable", AccessToken: "at"}

	read := func() (string, string) {
		req, _ := http.NewRequest(http.MethodPost, "https://example.invalid/v2/chat/completions", http.NoBody)
		client.CommonHeaders(req, a)

		return req.Header.Get("X-Machine-Id"), req.Header.Get("X-Session-Id")
	}

	machineA, sessionA := read()

	machineB, sessionB := read()
	if machineA != machineB || sessionA != sessionB {
		t.Fatalf("同 uid 派生应稳定: machine %q/%q session %q/%q", machineA, machineB, sessionA, sessionB)
	}

	if !uuidV4HeaderPattern.MatchString(machineA) || !uuidV4HeaderPattern.MatchString(sessionA) {
		t.Fatalf("设备/会话头应为 UUIDv4: machine=%q session=%q", machineA, sessionA)
	}
}

// TestConversationHeaderPrefersInbound 客户端显式 conversationId 优先于兜底值。
func TestConversationHeaderPrefersInbound(t *testing.T) {
	t.Parallel()

	client := New()

	req, err := http.NewRequest(http.MethodPost, "https://example.invalid/v2/chat/completions", http.NoBody)
	if err != nil {
		t.Fatal(err)
	}

	client.ChatHeaders(req, &auth.Auth{UID: "u1", AccessToken: "at"}, "", ChatMeta{
		ConversationID:       "client-conv",
		ConversationHeaderID: "fallback",
	})

	if got := req.Header.Get("X-Conversation-Id"); got != "client-conv" {
		t.Fatalf("X-Conversation-Id=%q want client-conv", got)
	}
}

// TestIdentityHeadersEnterprise 身份头只按企业身份注入：个人号即使残留
// token_source 也不发 X-Auth-Method / X-Userinfo（issue #248）。
//
//nolint:cyclop // 表驱动四象限。
func TestIdentityHeadersEnterprise(t *testing.T) {
	t.Parallel()

	makeAuth := func(enterprise, source string) *auth.Auth {
		payload := base64.RawURLEncoding.EncodeToString([]byte(`{"token_source":"` + source + `"}`))
		return &auth.Auth{UID: "u1", AccessToken: "x." + payload + ".sig", EnterpriseID: enterprise}
	}

	cases := []struct {
		name       string
		account    *auth.Auth
		wantMethod string
		wantInfo   bool
	}{
		{"企业号带来源", makeAuth("ent-1", "enterprise_switch"), "enterprise_switch", true},
		{"企业号缺来源", makeAuth("ent-1", ""), "", true},
		{"个人号无来源", makeAuth("", ""), "", false},
		{"个人号残留来源", makeAuth("", "enterprise_switch"), "", false},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			client := New()
			req, _ := http.NewRequest(http.MethodPost, "https://example.invalid/v2/chat/completions", http.NoBody)
			client.ChatHeaders(req, testCase.account, "", ChatMeta{})

			if got := req.Header.Get("X-Auth-Method"); got != testCase.wantMethod {
				t.Fatalf("X-Auth-Method=%q want %q", got, testCase.wantMethod)
			}

			info := req.Header.Get("X-Userinfo")
			if !testCase.wantInfo {
				if info != "" {
					t.Fatalf("personal account must not send X-Userinfo: %s", info)
				}

				return
			}

			raw, err := base64.StdEncoding.DecodeString(info)
			if err != nil {
				t.Fatalf("X-Userinfo base64: %v", err)
			}

			var payload map[string]any
			if err := json.Unmarshal(raw, &payload); err != nil {
				t.Fatalf("X-Userinfo json: %v", err)
			}

			if payload["uin"] != "u1" || payload["owner_uin"] != "ent-1" {
				t.Fatalf("X-Userinfo=%v", payload)
			}

			if testCase.wantMethod != "" && payload["token_source"] != testCase.wantMethod {
				t.Fatalf("token_source=%v want %s", payload["token_source"], testCase.wantMethod)
			}
		})
	}
}

// TestChatHeadersNoAcceptLanguage chat 路径不发 Accept-Language；billing 仍按 realm 带。
func TestChatHeadersNoAcceptLanguage(t *testing.T) {
	t.Parallel()

	client := New()
	acct := &auth.Auth{UID: "u1", AccessToken: "at"}

	req, _ := http.NewRequest(http.MethodPost, "https://example.invalid/v2/chat/completions", http.NoBody)
	client.ChatHeaders(req, acct, "", ChatMeta{})

	if got := req.Header.Get("Accept-Language"); got != "" {
		t.Fatalf("chat Accept-Language=%q want empty", got)
	}

	billing, _ := http.NewRequest(http.MethodPost, "https://example.invalid/billing", http.NoBody)
	client.BillingHeaders(billing, acct)

	if got := billing.Header.Get("Accept-Language"); got != "zh-CN" {
		t.Fatalf("billing Accept-Language=%q want zh-CN", got)
	}
}
