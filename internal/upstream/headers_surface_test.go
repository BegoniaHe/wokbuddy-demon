package upstream

import (
	"net/http"
	"regexp"
	"slices"
	"strings"
	"testing"

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
// X-No-* 声明头干扰键集比对）。
func surfaceGoldenAccount() *auth.Auth {
	return &auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", Domain: "www.workbuddy.cn"}
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
				"Accept", "Authorization", "Content-Type", "User-Agent", "X-Domain",
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
		})
	}
}
