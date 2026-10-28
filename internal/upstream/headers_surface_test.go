package upstream

import (
	"encoding/binary"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
)

// assertHeaderKeys 断言请求头键集**精确相等**：多发与漏发都失败。
// 出站指纹按「键集不多不少」验收，只断言存在会放过多发头。
func assertHeaderKeys(t *testing.T, req *http.Request, want ...string) {
	t.Helper()

	got := make([]string, 0, len(req.Header))
	for name := range req.Header {
		got = append(got, strings.ToLower(name))
	}

	slices.Sort(got)

	wanted := make([]string, 0, len(want))
	for _, name := range want {
		wanted = append(wanted, strings.ToLower(name))
	}

	slices.Sort(wanted)

	if !slices.Equal(got, wanted) {
		t.Fatalf("头键集不符\n got=%v\nwant=%v", got, wanted)
	}
}

// surfaceGoldenAccount 黄金头集用例的固定账号：CN 个人号（有 domain，避免
// X-No-* 声明头干扰键集比对）；带 device token，用于钉住「只有 chat 面发该头」。
func surfaceGoldenAccount() *auth.Auth {
	return &auth.Auth{
		UID: "u1", AccessToken: "at", RefreshToken: "rt",
		Domain: "www.workbuddy.cn", DeviceToken: "dev-token-1",
	}
}

// TestCodebuddyRequestOnlyOnChatFamily X-Codebuddy-Request 只出现在 chat 头族
// （chat 与 agenttool 四路，agenttool 无真机证据沿 chat 头族一起发）；billing /
// report / refresh / v3-config / growth 一律不发（#368：真机 14 条非 chat 业务请求
// 0 命中，7 条 chat 全带）。
func TestCodebuddyRequestOnlyOnChatFamily(t *testing.T) {
	t.Parallel()

	client := newProfileClient(t)
	account := surfaceGoldenAccount()

	chatFamily := []struct {
		name    string
		surface surface
	}{
		{"chat", surfaceChat},
		{"search", surfaceSearch},
		{"webfetch", surfaceWebFetch},
		{"agentic_search", surfaceAgenticSearch},
		{"images", surfaceImages},
	}

	for _, testCase := range chatFamily {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			req, err := http.NewRequest(http.MethodPost, "https://example.invalid/x", http.NoBody)
			if err != nil {
				t.Fatal(err)
			}

			client.applyHeaders(testCase.surface, req, account, "", ChatMeta{})

			if got := req.Header.Get("X-Codebuddy-Request"); got != "1" {
				t.Errorf("%s X-Codebuddy-Request=%q want 1", testCase.name, got)
			}
		})
	}

	light := []struct {
		name string
		call func(req *http.Request)
	}{
		{"billing", func(req *http.Request) { client.BillingHeaders(req, account) }},
		{"busy", func(req *http.Request) { client.ModelBusyHeaders(req, account) }},
		{"report", func(req *http.Request) { client.ReportHeaders(req, account) }},
		{"refresh", func(req *http.Request) { client.RefreshHeaders(req, account) }},
		{"v3config", func(req *http.Request) { client.V3ConfigHeaders(req, account, "") }},
		{"growth", func(req *http.Request) { client.GrowthHeaders(req, account) }},
	}

	for _, testCase := range light {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			req, err := http.NewRequest(http.MethodPost, "https://example.invalid/x", http.NoBody)
			if err != nil {
				t.Fatal(err)
			}

			testCase.call(req)

			if got := req.Header.Get("X-Codebuddy-Request"); got != "" {
				t.Errorf("%s X-Codebuddy-Request=%q want empty", testCase.name, got)
			}
		})
	}
}

// hex32 32 位小写 hex（无版本位约束的随机 ID 形态断言用）。
var hex32 = regexp.MustCompile(`^[0-9a-f]{32}$`)

// uuidV7Hex32 UUIDv7 形态的 32 hex（第 13 位为 7、变体位 10xx）。
var uuidV7Hex32 = regexp.MustCompile(`^[0-9a-f]{12}7[0-9a-f]{3}[89ab][0-9a-f]{15}$`)

// TestLightSurfaceGoldenHeaderSets 轻头族黄金头集：refresh / v3-config / billing
// 三路的键集**精确相等**（多发漏发都红），并断言 X-Request-ID 为 32 hex（#369）。
// 官方 HTTP 层给每条非 chat 业务请求补该头，chat 面的同名头是消息级 ID。
func TestLightSurfaceGoldenHeaderSets(t *testing.T) {
	t.Parallel()

	client := newProfileClient(t)
	account := surfaceGoldenAccount()

	cases := []struct {
		name string
		url  string
		call func(req *http.Request)
		keys []string
	}{
		{
			name: "refresh",
			url:  "https://www.workbuddy.cn/v2/plugin/auth/token/refresh",
			call: func(req *http.Request) { client.RefreshHeaders(req, account) },
			keys: []string{
				"Accept", "Authorization", "B3", "Content-Type", "Traceparent", "User-Agent",
				"X-Auth-Refresh-Source", "X-B3-Parentspanid", "X-B3-Sampled", "X-B3-Spanid",
				"X-B3-Traceid", "X-Domain", "X-No-Enterprise-Id", "X-Refresh-Token",
				"X-Request-Id", "X-Requested-With", "X-Trace-Id", "X-User-Id",
			},
		},
		{
			name: "v3config",
			url:  "https://www.workbuddy.cn/v3/config",
			call: func(req *http.Request) { client.V3ConfigHeaders(req, account, "") },
			keys: []string{
				"Accept", "Authorization", "User-Agent", "X-Domain",
				"X-No-Enterprise-Id", "X-Product", "X-Request-Id", "X-Requested-With",
				"X-User-Id",
			},
		},
		{
			name: "billing",
			url:  "https://www.workbuddy.cn/billing/meter/get-user-resource-summary",
			call: func(req *http.Request) { client.BillingHeaders(req, account) },
			keys: []string{
				"Accept", "Authorization", "Content-Type", "User-Agent", "X-Domain",
				"X-Request-Id", "X-User-Id",
			},
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			req, err := http.NewRequest(http.MethodPost, testCase.url, http.NoBody)
			if err != nil {
				t.Fatal(err)
			}

			testCase.call(req)

			assertHeaderKeys(t, req, testCase.keys...)

			if got := req.Header.Get("X-Request-ID"); !uuidV7Hex32.MatchString(got) {
				t.Errorf("X-Request-ID=%q want UUIDv7 32 hex", got)
			}

			if testCase.name == "v3config" {
				// /v3/config 的 Accept 是宽松值（官方 RestOperations 默认），别收窄。
				if got := req.Header.Get("Accept"); got != "application/json, text/plain, */*" {
					t.Errorf("v3config Accept=%q want 宽松值", got)
				}
			}
		})
	}
}

// TestModelBusyGoldenHeaderSet /v2/chat/busy 只有一个 8 轻头形态：真机（个人号）
// 去掉 Host / Connection / Content-Length 后恰好 8 个，多一个就是非官方指纹。
func TestModelBusyGoldenHeaderSet(t *testing.T) {
	t.Parallel()

	client := newProfileClient(t)
	account := surfaceGoldenAccount()

	req, err := http.NewRequest(http.MethodPost, "https://www.workbuddy.cn/v2/chat/busy", http.NoBody)
	if err != nil {
		t.Fatal(err)
	}

	client.ModelBusyHeaders(req, account)

	assertHeaderKeys(t, req,
		"Accept", "Content-Type", "User-Agent", "X-Request-Id",
		"X-Product", "Authorization", "X-User-Id", "X-Domain",
	)

	for name, want := range map[string]string{
		"Accept":        "application/json, text/plain, */*",
		"Content-Type":  "application/json",
		"X-Product":     "SaaS",
		"Authorization": "Bearer at",
		"X-User-Id":     "u1",
		"X-Domain":      "www.workbuddy.cn",
		"User-Agent":    "WorkBuddy/9.9.9 WorkBuddy/9.9.9 CLI/8.8.8",
	} {
		if got := req.Header.Get(name); got != want {
			t.Errorf("%s=%q want %q", name, got, want)
		}
	}

	if got := req.Header.Get("X-Request-ID"); !uuidV7Hex32.MatchString(got) {
		t.Errorf("X-Request-ID=%q want UUIDv7 32 hex", got)
	}

	// 会话头族 / 链路族 / 归属族一律不发。
	for _, name := range []string{
		"X-Conversation-Id", "X-Conversation-Request-Id", "X-Conversation-Message-Id",
		"X-Root-Request-Id", "Acp-Connection-Id", "X-Trace-Id", "Traceparent", "B3",
		"X-Agent-Runtime", "X-Agent-Purpose", "X-Agent-Type", "X-Ide-Name",
		"X-Private-Data", "X-Requested-With", "X-Codebuddy-Request",
		"X-Enterprise-Id", "X-Tenant-Id", "X-No-Enterprise-Id",
	} {
		if got := req.Header.Get(name); got != "" {
			t.Errorf("busy 不应发 %s=%q", name, got)
		}
	}
}

// TestModelBusyEnterpriseAccountBareToo 企业账号同样 8 轻头：真机样本是个人号，
// 企业号是否带 X-Enterprise-Id / X-Tenant-Id 无证据，按「不猜测」不发。
func TestModelBusyEnterpriseAccountBareToo(t *testing.T) {
	t.Parallel()

	client := newProfileClient(t)
	account := &auth.Auth{UID: "u-ent", AccessToken: "at", EnterpriseID: "ent-1", Domain: "www.workbuddy.cn"}

	req, err := http.NewRequest(http.MethodPost, "https://www.workbuddy.cn/v2/chat/busy", http.NoBody)
	if err != nil {
		t.Fatal(err)
	}

	client.ModelBusyHeaders(req, account)

	assertHeaderKeys(t, req,
		"Accept", "Content-Type", "User-Agent", "X-Request-Id",
		"X-Product", "Authorization", "X-User-Id", "X-Domain",
	)
}

// TestBillingMeterUAAllMeterPaths billing meter 三个真实端点
// （get-user-resource / summary / free-packages）共用 BillingHeaders，UA 恒为
// profile 冻结的抓包值——官方该面 UA 由 axios 默认值补上，不是桌面拦截器（#374）。
func TestBillingMeterUAAllMeterPaths(t *testing.T) {
	t.Parallel()

	client := newProfileClient(t)
	account := surfaceGoldenAccount()

	for _, path := range []string{
		"/billing/meter/get-user-resource",
		"/billing/meter/get-user-resource-summary",
		"/billing/meter/get-user-resource-free-packages",
		"/v2/billing/meter/checkin-activity-status",
	} {
		req, err := http.NewRequest(http.MethodPost, "https://www.workbuddy.cn"+path, http.NoBody)
		if err != nil {
			t.Fatal(err)
		}

		client.BillingHeaders(req, account)

		if got := req.Header.Get("User-Agent"); got != billingMeterUAProfileFixture {
			t.Errorf("%s User-Agent=%q want %q（profile 抓包值）", path, got, billingMeterUAProfileFixture)
		}
	}
}

// TestGrowthGoldenHeaderSet growth 面（/activity/growth/*）是 Node 原生 fetch：
// UA=node、accept-language=*、sec-fetch-mode=cors，没有 X-Request-ID / X-Product /
// X-Requested-With / X-Codebuddy-Request（#375）。真机 9 个头里 `accept-encoding`
// 由 Go transport 发送，不在 req.Header 里，故此处断言 8 项。
func TestGrowthGoldenHeaderSet(t *testing.T) {
	t.Parallel()

	client := newProfileClient(t)
	account := surfaceGoldenAccount()

	req, err := http.NewRequest(http.MethodGet, "https://www.workbuddy.cn/v2/activity/growth/buddy/info", http.NoBody)
	if err != nil {
		t.Fatal(err)
	}

	client.GrowthHeaders(req, account)

	assertHeaderKeys(t, req,
		"Accept", "Accept-Language", "Authorization", "Content-Type",
		"Sec-Fetch-Mode", "User-Agent", "X-Domain", "X-User-Id",
	)

	for name, want := range map[string]string{
		"Accept":          "application/json",
		"Accept-Language": "*",
		"Content-Type":    "application/json", // GET 也照发（真机如此）
		"Sec-Fetch-Mode":  "cors",
		"User-Agent":      "node",
		"X-User-Id":       "u1",
		"X-Domain":        "www.workbuddy.cn",
		"Authorization":   "Bearer at",
	} {
		if got := req.Header.Get(name); got != want {
			t.Errorf("%s=%q want %q", name, got, want)
		}
	}
}

// TestGrowthUAHonorsExplicitUserAgent growth 面默认 UA 是 node，但显式
// upstream.user_agent 的口径是「全部出站生效」，不能在该面被 node 静默吞掉。
func TestGrowthUAHonorsExplicitUserAgent(t *testing.T) {
	t.Parallel()

	client := newProfileClient(t)
	client.UserAgent = "custom/1.0"

	req, err := http.NewRequest(http.MethodGet, "https://www.workbuddy.cn/v2/activity/growth/buddy/info", http.NoBody)
	if err != nil {
		t.Fatal(err)
	}

	client.GrowthHeaders(req, surfaceGoldenAccount())

	if got := req.Header.Get("User-Agent"); got != "custom/1.0" {
		t.Errorf("User-Agent=%q want custom/1.0（显式配置优先于 node 默认）", got)
	}
}

// TestLightSurfaceXDomainFollowsEndpoint 轻头族无账号 domain 时，X-Domain 必须取
// 本次请求的 endpoint host，而不是 chat base：billing 面走 billingBase
// （codebuddy.cn），回落到 chat base 会把 workbuddy.cn 发给 billing 域。
func TestLightSurfaceXDomainFollowsEndpoint(t *testing.T) {
	t.Parallel()

	client := newProfileClient(t)
	client.ChatBaseCN = "https://www.workbuddy.cn"
	client.BillingBaseCN = "https://www.codebuddy.cn"

	account := &auth.Auth{UID: "u1", AccessToken: "at"}

	req, err := http.NewRequest(http.MethodPost,
		"https://www.codebuddy.cn/billing/meter/get-user-resource-summary", http.NoBody)
	if err != nil {
		t.Fatal(err)
	}

	client.BillingHeaders(req, account)

	if got := req.Header.Get("X-Domain"); got != "www.codebuddy.cn" {
		t.Errorf("X-Domain=%q want www.codebuddy.cn（endpoint authority，不是 chat base）", got)
	}
}

// TestGrowthPathsUseNodeFingerprint 三条已实测/已用路径（buddy/info、travel/status、
// streak）走同一 growth 头族；其余 /activity/growth/* 路径（tasks、heatmap、
// lottery、makeup-cards 等）是**同面假设**，无逐路径抓包证据，注释见 UPSTREAM.md。
func TestGrowthPathsUseNodeFingerprint(t *testing.T) {
	t.Parallel()

	paths := make(map[string]struct{})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("User-Agent"); got != "node" {
			t.Errorf("%s User-Agent=%q want node", r.URL.Path, got)
		}

		if got := r.Header.Get("X-Codebuddy-Request"); got != "" {
			t.Errorf("%s X-Codebuddy-Request=%q want empty", r.URL.Path, got)
		}

		if got := r.Header.Get("X-Request-ID"); got != "" {
			t.Errorf("%s X-Request-ID=%q want empty", r.URL.Path, got)
		}

		if got := r.Header.Get("Accept-Language"); got != "*" {
			t.Errorf("%s Accept-Language=%q want *", r.URL.Path, got)
		}

		paths[strings.TrimPrefix(r.URL.Path, "/v2")] = struct{}{}

		w.Write([]byte(`{"code":0,"data":{"buddy":{"id":1,"name":"x"},"state":"idle","streak":{"days":1},"tasks":[]}}`))
	}))
	defer srv.Close()

	client := &Client{HTTP: srv.Client(), ChatHTTP: srv.Client(), ChatBaseCN: srv.URL, Profile: testProfileSet(t)}
	account := surfaceGoldenAccount()

	if _, err := client.BuddyInfo(account); err != nil {
		t.Fatalf("BuddyInfo: %v", err)
	}

	if _, err := client.TravelStatus(account); err != nil {
		t.Fatalf("TravelStatus: %v", err)
	}

	if _, err := client.GrowthStreak(account); err != nil {
		t.Fatalf("GrowthStreak: %v", err)
	}

	for _, want := range []string{
		"/activity/growth/buddy/info",
		"/activity/growth/buddy/travel/status",
		"/activity/growth/streak",
	} {
		if _, ok := paths[want]; !ok {
			t.Errorf("未观察到 %s 请求，got=%v", want, paths)
		}
	}
}

// TestChatBaggage 断言 chat 出站的 W3C baggage 逐字形态：只有 4 条（buddy 两空值键
// + codebuddy.session_id + codebuddy.conversation_request_id），且两个 codebuddy 键
// 与同请求的 X-Conversation-Id / X-Conversation-Request-Id 逐项同值（#364 真机口径）。
// 真机没发 conversation_message_id / root_request_id，故这两条必须**不出现**。
func TestChatBaggage(t *testing.T) {
	t.Parallel()

	client := newProfileClient(t)
	account := surfaceGoldenAccount()

	req, err := http.NewRequest(http.MethodPost, "https://www.workbuddy.cn/v2/chat/completions", http.NoBody)
	if err != nil {
		t.Fatal(err)
	}

	client.ChatHeaders(req, account, "", ChatMeta{
		ConversationID:        "0f0e0d0c-0b0a-4908-8706-050403020100",
		ConversationRequestID: "01a17a9c4d2f7b3e8c5a6d4e2f1b0a93",
	})

	wantBaggage := "buddyId=,buddyName=," +
		"codebuddy.session_id=" + req.Header.Get("X-Conversation-Id") +
		",codebuddy.conversation_request_id=" + req.Header.Get("X-Conversation-Request-Id")

	if got := req.Header.Get("Baggage"); got != wantBaggage {
		t.Fatalf("baggage=%q want %q", got, wantBaggage)
	}

	if want, got := "0f0e0d0c-0b0a-4908-8706-050403020100", req.Header.Get("X-Conversation-Id"); got != want {
		t.Fatalf("X-Conversation-Id=%q want %q", got, want)
	}

	if want, got := "01a17a9c4d2f7b3e8c5a6d4e2f1b0a93", req.Header.Get("X-Conversation-Request-Id"); got != want {
		t.Fatalf("X-Conversation-Request-Id=%q want %q", got, want)
	}

	for _, forbidden := range []string{"conversation_message_id", "root_request_id", "tracestate"} {
		if strings.Contains(req.Header.Get("Baggage"), forbidden) {
			t.Errorf("baggage 不应包含 %q：%q", forbidden, req.Header.Get("Baggage"))
		}
	}

	// baggage 只在 chat 面：agenttool 四路与轻头族各面都不发。
	other, err := http.NewRequest(http.MethodPost, "https://www.workbuddy.cn/agenttool/v1/search", http.NoBody)
	if err != nil {
		t.Fatal(err)
	}

	client.applyHeaders(surfaceSearch, other, account, "", ChatMeta{})

	if got := other.Header.Get("Baggage"); got != "" {
		t.Errorf("search baggage=%q want empty", got)
	}
}

// TestAcpConnectionIDIsUUIDv7 acp-connection-id 是会话级带连字符 UUIDv7：
// 第 13 位为 7、变体位 10xx、同 conversationId 恒同值、时间戳前缀落在可信时间（#372）。
func TestAcpConnectionIDIsUUIDv7(t *testing.T) {
	t.Parallel()

	const convID = "0f0e0d0c-0b0a-4908-8706-050403020100"

	first := acpConnectionID(convID)

	if !uuidV7HyphenPattern.MatchString(first) {
		t.Fatalf("acp-connection-id=%q want 带连字符 UUIDv7", first)
	}

	if got := acpConnectionID(convID); got != first {
		t.Fatalf("同 convID 派生值不稳定: %q != %q", got, first)
	}

	if acpConnectionID("another-conversation") == first {
		t.Fatal("不同 convID 派生出相同 acp-connection-id")
	}

	// 时间戳前缀必须落在进程启动基准前 1 小时窗口内（可信时间，不是固定哨兵值）。
	raw, err := hex.DecodeString(strings.ReplaceAll(first, "-", ""))
	if err != nil {
		t.Fatalf("DecodeString(%q): %v", first, err)
	}

	var high [8]byte
	copy(high[2:], raw[:6])

	stamp := int64(binary.BigEndian.Uint64(high[:]))
	lower := acpConnectionEpoch.Add(-time.Hour).UnixMilli()

	if stamp < lower || stamp > acpConnectionEpoch.UnixMilli() {
		t.Fatalf("acp-connection-id 时间戳=%d 不在 [%d,%d]", stamp, lower, acpConnectionEpoch.UnixMilli())
	}
}

// TestChatGoldenHeaderSet chat 面黄金头集：键集**精确相等**（多发漏发都红），
// 并钉住真机取值——Accept=application/json（流式也不发 event-stream）、
// X-Agent-Runtime=local-agent、trace 三处同值且是随机 v4 形态、与
// X-Conversation-Request-ID（v7）解耦；会话 ID 兜底仍是 v4、acp id 是 v7。
func TestChatGoldenHeaderSet(t *testing.T) {
	t.Parallel()

	const (
		convID    = "0f0e0d0c-0b0a-4908-8706-050403020100"
		convReqID = "01a17a9c4d2f7b3e8c5a6d4e2f1b0a93"
	)

	client := newProfileClient(t)
	account := surfaceGoldenAccount()

	req, err := http.NewRequest(http.MethodPost, "https://www.workbuddy.cn/v2/chat/completions", http.NoBody)
	if err != nil {
		t.Fatal(err)
	}

	client.ChatHeaders(req, account, "", ChatMeta{
		ConversationID:        convID,
		ConversationRequestID: convReqID,
	})

	assertHeaderKeys(t, req,
		"Accept", "Acp-Connection-Id", "Authorization", "B3", "Baggage", "Content-Type",
		"Traceparent", "User-Agent", "X-Agent-Intent", "X-Agent-Purpose", "X-Agent-Runtime",
		"X-Agent-Type", "X-B3-Parentspanid", "X-B3-Sampled", "X-B3-Spanid", "X-B3-Traceid",
		"X-Codebuddy-Request", "X-Conversation-Id", "X-Conversation-Message-Id",
		"X-Conversation-Request-Id", "X-Domain", "X-Ide-Name", "X-Ide-Type", "X-Ide-Version",
		"X-No-Enterprise-Id", "X-Private-Data", "X-Product", "X-Request-Id",
		"X-Requested-With", "X-Root-Request-Id", "X-Trace-Id", "X-User-Id",
		"X-Device-Token",
	)

	if got := req.Header.Get("Accept"); got != "application/json" {
		t.Errorf("Accept=%q want application/json（流式也不发 event-stream）", got)
	}

	if got := req.Header.Get("X-Agent-Runtime"); got != "local-agent" {
		t.Errorf("X-Agent-Runtime=%q want local-agent", got)
	}

	assertTraceFamily(t, req, convReqID)

	// 会话 ID 兜底保持 v4（真机主请求就是 v4）；acp id 是带连字符 v7。
	if got := req.Header.Get("X-Conversation-Id"); got != convID {
		t.Errorf("X-Conversation-Id=%q want %q", got, convID)
	}

	if got := req.Header.Get("X-Conversation-Request-Id"); got != convReqID {
		t.Errorf("X-Conversation-Request-Id=%q want %q", got, convReqID)
	}

	// X-Request-ID 是消息级 ID，与 X-Conversation-Message-Id 同值且不等于轮级 ID。
	messageID := req.Header.Get("X-Conversation-Message-Id")
	if got := req.Header.Get("X-Request-ID"); got != messageID {
		t.Errorf("X-Request-ID=%q want =X-Conversation-Message-Id %q", got, messageID)
	}

	if !uuidV7Hex32.MatchString(messageID) {
		t.Errorf("X-Conversation-Message-Id=%q want UUIDv7 32 hex", messageID)
	}
}

// assertTraceFamily trace 三处（X-Trace-Id / X-B3-Traceid / traceparent）同值、
// 为随机 UUIDv4 形态 32 hex，且与轮级 X-Conversation-Request-ID 不同值。
func assertTraceFamily(t *testing.T, req *http.Request, convReqID string) {
	t.Helper()

	traceID := req.Header.Get("X-Trace-Id")
	if !uuidV432HeaderPattern.MatchString(traceID) {
		t.Errorf("X-Trace-Id=%q want 随机 UUIDv4 形态 32 hex", traceID)
	}

	if traceID == convReqID {
		t.Error("X-Trace-Id 不得复用 X-Conversation-Request-ID（两者形态可判别）")
	}

	if got := req.Header.Get("X-B3-Traceid"); got != traceID {
		t.Errorf("X-B3-Traceid=%q want 与 X-Trace-Id 同值 %q", got, traceID)
	}

	spanID := req.Header.Get("X-B3-Spanid")
	if got, want := req.Header.Get("Traceparent"), "00-"+traceID+"-"+spanID+"-01"; got != want {
		t.Errorf("Traceparent=%q want %q", got, want)
	}
}
