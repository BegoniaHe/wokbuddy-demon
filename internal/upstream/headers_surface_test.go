package upstream

import (
	"net/http"
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
