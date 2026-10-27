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

// TestDesktopFingerprintHeadersAbsent 官方 CLI chat 与 daemon 网关都不发
// Origin / Referer / X-Machine-Id / X-Session-Id（issue #299）。CN/global 经
// CommonHeaders、ChatHeaders、RefreshHeaders 都不得注入。
func TestDesktopFingerprintHeadersAbsent(t *testing.T) {
	t.Parallel()

	client := New()

	cases := []struct {
		name    string
		account *auth.Auth
	}{
		{"CN 个人号", &auth.Auth{UID: "u-cn", AccessToken: "at"}},
		{"global 个人号", &auth.Auth{UID: "u-global", AccessToken: "at", Domain: "www.workbuddy.ai"}},
	}

	headerSets := []struct {
		name string
		call func(req *http.Request, account *auth.Auth)
	}{
		{"CommonHeaders", func(req *http.Request, account *auth.Auth) {
			client.CommonHeaders(req, account)
		}},
		{"ChatHeaders", func(req *http.Request, account *auth.Auth) {
			client.ChatHeaders(req, account, "", ChatMeta{})
		}},
		{"RefreshHeaders", func(req *http.Request, account *auth.Auth) {
			client.RefreshHeaders(req, account)
		}},
	}

	for _, accountCase := range cases {
		for _, headerCase := range headerSets {
			t.Run(accountCase.name+"/"+headerCase.name, func(t *testing.T) {
				t.Parallel()

				req, err := http.NewRequest(http.MethodPost, "https://example.invalid/v2/chat/completions", http.NoBody)
				if err != nil {
					t.Fatalf("new request: %v", err)
				}

				headerCase.call(req, accountCase.account)

				for _, name := range []string{"Origin", "Referer", "X-Machine-Id", "X-Session-Id"} {
					if got := req.Header.Get(name); got != "" {
						t.Errorf("%s=%q want empty", name, got)
					}
				}
			})
		}
	}
}

// TestChatHeadersDepartmentInfoExcludesNoDepartment 有 departmentFullName 时
// ChatHeaders 发 X-Department-Info 并移除 X-No-Department-Info；只调
// injectChatDomain 的 FetchModels 类端点仍保留「无部门」声明。
func TestChatHeadersDepartmentInfoExcludesNoDepartment(t *testing.T) {
	t.Parallel()

	account := &auth.Auth{UID: "u1", AccessToken: "at", EnterpriseID: "ent-1", DepartmentFullName: "研发部"}

	req, err := http.NewRequest(http.MethodPost, "https://example.invalid/v2/chat/completions", http.NoBody)
	if err != nil {
		t.Fatal(err)
	}

	New().ChatHeaders(req, account, "", ChatMeta{})

	if got := req.Header.Get("X-Department-Info"); got != "研发部" {
		t.Errorf("X-Department-Info=%q", got)
	}

	if got := req.Header.Get("X-No-Department-Info"); got != "" {
		t.Errorf("X-No-Department-Info=%q should be removed", got)
	}

	modelsReq, err := http.NewRequest(http.MethodPost, "https://example.invalid/v3/config", http.NoBody)
	if err != nil {
		t.Fatal(err)
	}

	injectChatDomain(modelsReq, account)

	if got := modelsReq.Header.Get("X-No-Department-Info"); got != "1" {
		t.Errorf("non-chat X-No-Department-Info=%q want 1", got)
	}

	if got := modelsReq.Header.Get("X-Department-Info"); got != "" {
		t.Errorf("non-chat X-Department-Info=%q want empty", got)
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

// TestIdentityHeaders 身份头按 enterpriseId / idSource 注入：企业号补齐
// X-Id-Source / X-Department-Info，静态 Key（enterpriseId 空、idSource 非空）
// 不再被跳过；个人号即使残留 token_source 也不发任何身份头（issue #248/#298）。
//
//nolint:cyclop // 表驱动多字段断言。
func TestIdentityHeaders(t *testing.T) {
	t.Parallel()

	makeAuth := func(enterprise, idSource, department, source string) *auth.Auth {
		payload := base64.RawURLEncoding.EncodeToString([]byte(`{"token_source":"` + source + `"}`))

		return &auth.Auth{
			UID:                "u1",
			AccessToken:        "x." + payload + ".sig",
			EnterpriseID:       enterprise,
			IDSource:           idSource,
			DepartmentFullName: department,
		}
	}

	cases := []struct {
		name           string
		account        *auth.Auth
		wantMethod     string
		wantIDSource   string
		wantDepartment string
		wantInfo       bool
		wantOwner      string
	}{
		{"企业号带来源与部门", makeAuth("ent-1", "static_key", "研发部", "enterprise_switch"), "enterprise_switch", "static_key", "研发部", true, "ent-1"},
		{"企业号缺来源", makeAuth("ent-1", "", "", ""), "", "", "", true, "ent-1"},
		{"静态 Key 仅 idSource", makeAuth("", "static_key", "", "static_key"), "static_key", "static_key", "", true, ""},
		{"个人号无来源", makeAuth("", "", "", ""), "", "", "", false, ""},
		{"个人号残留来源", makeAuth("", "", "", "enterprise_switch"), "", "", "", false, ""},
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

			if got := req.Header.Get("X-Id-Source"); got != testCase.wantIDSource {
				t.Fatalf("X-Id-Source=%q want %q", got, testCase.wantIDSource)
			}

			if got := req.Header.Get("X-Department-Info"); got != testCase.wantDepartment {
				t.Fatalf("X-Department-Info=%q want %q", got, testCase.wantDepartment)
			}

			if testCase.wantDepartment != "" && req.Header.Get("X-No-Department-Info") != "" {
				t.Fatal("X-No-Department-Info must not accompany X-Department-Info")
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

			if payload["uin"] != "u1" {
				t.Fatalf("X-Userinfo uin=%v", payload["uin"])
			}

			if got, ok := payload["owner_uin"]; testCase.wantOwner == "" && ok {
				t.Fatalf("owner_uin should be absent: %v", got)
			} else if testCase.wantOwner != "" && got != testCase.wantOwner {
				t.Fatalf("owner_uin=%v want %s", got, testCase.wantOwner)
			}

			if got, ok := payload["id_source"]; testCase.wantIDSource == "" && ok {
				t.Fatalf("id_source should be absent: %v", got)
			} else if testCase.wantIDSource != "" && got != testCase.wantIDSource {
				t.Fatalf("id_source=%v want %s", got, testCase.wantIDSource)
			}

			if got, ok := payload["token_source"]; testCase.wantMethod == "" && ok {
				t.Fatalf("token_source should be absent: %v", got)
			} else if testCase.wantMethod != "" && got != testCase.wantMethod {
				t.Fatalf("token_source=%v want %s", got, testCase.wantMethod)
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

// TestRefreshHeadersTrace refresh 也补 trace 五件套，且不复用会话四件套。
func TestRefreshHeadersTrace(t *testing.T) {
	t.Parallel()

	client := New()
	acct := &auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt"}

	req, _ := http.NewRequest(http.MethodPost, "https://example.invalid/v2/auth/refresh", http.NoBody)
	client.RefreshHeaders(req, acct)

	hex16 := regexp.MustCompile(`^[0-9a-f]{16}$`)

	traceID := req.Header.Get("X-Trace-Id")
	if !uuidV7HeaderPattern.MatchString(traceID) {
		t.Errorf("X-Trace-Id=%q want UUIDv7 32 hex", traceID)
	}

	b3Trace := req.Header.Get("X-B3-Traceid")
	if !uuidV7HeaderPattern.MatchString(b3Trace) {
		t.Errorf("X-B3-Traceid=%q want UUIDv7 32 hex", b3Trace)
	}

	spanID := req.Header.Get("X-B3-Spanid")
	parentSpanID := req.Header.Get("X-B3-Parentspanid")

	if !hex16.MatchString(spanID) || !hex16.MatchString(parentSpanID) {
		t.Errorf("span/parent=%q/%q want 16 hex", spanID, parentSpanID)
	}

	if got, want := req.Header.Get("Traceparent"), "00-"+b3Trace+"-"+spanID+"-01"; got != want {
		t.Errorf("Traceparent=%q want %q", got, want)
	}

	if got, want := req.Header.Get("B3"), b3Trace+"-"+spanID+"-1-"+parentSpanID; got != want {
		t.Errorf("B3=%q want %q", got, want)
	}

	if got := req.Header.Get("X-B3-Sampled"); got != "1" {
		t.Errorf("X-B3-Sampled=%q want 1", got)
	}

	for _, name := range []string{"X-Conversation-Id", "X-Conversation-Request-Id", "X-Conversation-Message-Id", "X-Request-ID"} {
		if got := req.Header.Get(name); got != "" {
			t.Errorf("refresh 不应发 %s=%q", name, got)
		}
	}
}

// TestRefreshDomain refresh 的 X-Domain：账号 domain 优先；global 缺省
// www.workbuddy.ai；CN 无 domain 回落到本次 refresh 请求的 endpoint host
// （含 cn.chat_base 覆盖与 chat 回落链里的备选域）。
func TestRefreshDomain(t *testing.T) {
	t.Parallel()

	globalNoDomain := &auth.Auth{UID: "u-global", AccessToken: "at", RefreshToken: "rt"}
	if _, err := auth.BackfillRealmFor(globalNoDomain, "global"); err != nil {
		t.Fatalf("BackfillRealmFor: %v", err)
	}

	cases := []struct {
		name    string
		reqURL  string
		account *auth.Auth
		want    string
	}{
		{
			name:    "CN 无 domain 回落默认 chat host",
			reqURL:  "https://www.workbuddy.cn/v2/plugin/auth/token/refresh",
			account: &auth.Auth{UID: "u-cn", AccessToken: "at", RefreshToken: "rt"},
			want:    "www.workbuddy.cn",
		},
		{
			name:    "CN chat_base 覆盖时跟随该 host",
			reqURL:  "https://cn.example.test/v2/plugin/auth/token/refresh",
			account: &auth.Auth{UID: "u-cn", AccessToken: "at", RefreshToken: "rt"},
			want:    "cn.example.test",
		},
		{
			name:    "CN chat 回落链备选域用实际 endpoint host",
			reqURL:  "https://copilot.tencent.com/v2/plugin/auth/token/refresh",
			account: &auth.Auth{UID: "u-cn", AccessToken: "at", RefreshToken: "rt"},
			want:    "copilot.tencent.com",
		},
		{
			name:    "CN 有 domain 时账号 domain 优先",
			reqURL:  "https://cn.example.test/v2/plugin/auth/token/refresh",
			account: &auth.Auth{UID: "u-cn", AccessToken: "at", RefreshToken: "rt", Domain: "copilot.tencent.com"},
			want:    "copilot.tencent.com",
		},
		{
			name:    "global 无 domain 仍固定 www.workbuddy.ai",
			reqURL:  "https://global.example.test/v2/plugin/auth/token/refresh",
			account: globalNoDomain,
			want:    "www.workbuddy.ai",
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			req, err := http.NewRequest(http.MethodPost, testCase.reqURL, http.NoBody)
			if err != nil {
				t.Fatal(err)
			}

			New().RefreshHeaders(req, testCase.account)

			if got := req.Header.Get("X-Domain"); got != testCase.want {
				t.Errorf("X-Domain=%q want %q", got, testCase.want)
			}
		})
	}
}

// TestPrivateDataHeaderPerRealm X-Private-Data 按账号身份分层：CN 5.9.3 官方
// applyPrivateDataHeaderPolicy 在个人号默认分支发 false、企业号发 true；
// global 基线仍是 5.8.4，旧逻辑发 true，等 global 升到 5.9.x 后按同一逻辑复核
// （issue #343）。
func TestPrivateDataHeaderPerRealm(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		account *auth.Auth
		want    string
	}{
		{"CN 个人号", &auth.Auth{UID: "u-cn", AccessToken: "at"}, "false"},
		{"CN 企业号", &auth.Auth{UID: "u-cn-ent", AccessToken: "at", EnterpriseID: "ent-1"}, "true"},
		{"global 个人号", &auth.Auth{UID: "u-gl", AccessToken: "at", Domain: "www.workbuddy.ai"}, "true"},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			req, err := http.NewRequest(http.MethodPost, "https://example.invalid/v2/chat/completions", http.NoBody)
			if err != nil {
				t.Fatal(err)
			}

			New().ChatHeaders(req, testCase.account, "", ChatMeta{})

			if got := req.Header.Get("X-Private-Data"); got != testCase.want {
				t.Fatalf("X-Private-Data=%q want %q", got, testCase.want)
			}
		})
	}
}

// TestAgentRuntimeHeaderAllSurfaces 官方 5.9.3 在 chat / images / search / webfetch
// 全发 X-Agent-Runtime，网关统一官方兜底值 other。ImagesHeaders / SearchHeaders
// 复用 ChatHeaders，自动继承该头（issue #345）。
func TestAgentRuntimeHeaderAllSurfaces(t *testing.T) {
	t.Parallel()

	client := New()
	account := &auth.Auth{UID: "u1", AccessToken: "at"}

	headerSets := []struct {
		name string
		call func(req *http.Request)
	}{
		{"chat", func(req *http.Request) { client.ChatHeaders(req, account, "", ChatMeta{}) }},
		{"images", func(req *http.Request) { client.ImagesHeaders(req, account, "", ChatMeta{}) }},
		{"search", func(req *http.Request) { client.SearchHeaders(req, account, "", ChatMeta{}) }},
	}

	for _, headerCase := range headerSets {
		t.Run(headerCase.name, func(t *testing.T) {
			t.Parallel()

			req, err := http.NewRequest(http.MethodPost, "https://example.invalid/x", http.NoBody)
			if err != nil {
				t.Fatal(err)
			}

			headerCase.call(req)

			if got := req.Header.Get("X-Agent-Runtime"); got != "other" {
				t.Fatalf("X-Agent-Runtime=%q want other", got)
			}
		})
	}
}
