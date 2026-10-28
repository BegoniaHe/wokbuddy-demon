package upstream

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
)

// uaCaptureTransport 记录出站请求的 User-Agent。
type uaCaptureTransport struct {
	ua *string
}

func (t uaCaptureTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	*t.ua = r.Header.Get("User-Agent")
	return jsonResp(200, `{"code":0}`), nil
}

// TestUserAgentDefaultEmptyKeepsClientUA 默认（UserAgent/client_name 空）行为：
// chat 与 /v2/report 都发三段式桌面 UA（真机同形）；billing meter 面另有
// profile 抓包串，由 TestBillingMeterUA 覆盖。
// 显式 client_name="SaaS" 才还原旧行为：不写 UA 键，回落 Go 默认 UA。
func TestUserAgentDefaultEmptyKeepsClientUA(t *testing.T) {
	t.Parallel()

	for _, testCase := range []struct {
		name   string
		call   func(c *Client) error
		wantUA string
	}{
		{
			name: "chat",
			call: func(c *Client) error {
				rc, status, _, err := c.ChatStream(&auth.Auth{AccessToken: "at", UID: "u1"}, []byte(`{"model":"glm-5.2","messages":[]}`), "", ChatMeta{})
				if status != 200 {
					t.Fatalf("chat status=%d", status)
				}

				if rc != nil {
					rc.Close()
				}

				return err
			},
			wantUA: defaultUAString,
		},
		{
			name: "billing_report",
			call: func(c *Client) error {
				return c.ReportChatActivity(&auth.Auth{AccessToken: "at", UID: "u1"}, "cid", "")
			},
			wantUA: defaultUAString,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			var str string

			client := &Client{
				HTTP:          &http.Client{Transport: uaCaptureTransport{ua: &str}},
				ChatHTTP:      &http.Client{Transport: uaCaptureTransport{ua: &str}},
				ChatBaseCN:    "https://chat.example",
				BillingBaseCN: "https://billing.example",
				Profile:       testProfileSet(t),
			}
			if err := testCase.call(client); err != nil {
				t.Fatalf("call: %v", err)
			}

			if str != testCase.wantUA {
				t.Errorf("UA = %q want %q", str, testCase.wantUA)
			}
		})
	}
}

// TestUserAgentOverrideAllOutbound 显式设置后 chat/billing/refresh 全路径覆盖。
// 用 env 别名直接验证 fields 传输到 headers 的行为。
func TestUserAgentOverrideAllOutbound(t *testing.T) {
	t.Parallel()

	account := &auth.Auth{AccessToken: "at", UID: "u1", RefreshToken: "rt"}
	str := "WorkBuddy/9.9.9"
	client := &Client{
		HTTP: &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
			if got := r.Header.Get("User-Agent"); got != str {
				t.Errorf("UA = %q want %q (path=%s)", got, str, r.URL.Path)
			}

			return jsonResp(200, `{"code":0}`), nil
		})},
		ChatHTTP: &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
			if got := r.Header.Get("User-Agent"); got != str {
				t.Errorf("Chat UA = %q want %q", got, str)
			}

			return jsonResp(200, `{"code":0}`), nil
		})},
		ChatBaseCN:    "https://chat.example",
		BillingBaseCN: "https://billing.example",
		UserAgent:     str,
	}
	// chat
	if rc, status, _, err := client.ChatStream(account, []byte(`{"model":"deepseek-v4-flash","messages":[]}`), "", ChatMeta{}); status != 200 || err != nil {
		t.Errorf("chat: status=%d err=%v", status, err)
	} else if rc != nil {
		rc.Close()
	}
	// refresh（RefreshHeaders→CommonHeaders）
	client.HTTP.Transport = rtFunc(func(r *http.Request) (*http.Response, error) {
		if got := r.Header.Get("User-Agent"); got != str {
			t.Errorf("Refresh UA = %q want %q", got, str)
		}

		return jsonResp(200, `{"code":0,"data":{"accessToken":"nat","refreshToken":"nrt"}}`), nil
	})
	if err := client.RefreshToken(account); err != nil {
		t.Errorf("refresh: %v", err)
	}
}

// TestUserAgentOverrideBilling 余额/签到类 billing 请求同样覆盖。
func TestUserAgentOverrideBilling(t *testing.T) {
	t.Parallel()

	account := &auth.Auth{AccessToken: "at", UID: "u1"}

	client := &Client{
		HTTP: &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
			if got := r.Header.Get("User-Agent"); got != "CustomAgent/1" {
				t.Errorf("Billing UA = %q want CustomAgent/1", got)
			}

			return jsonResp(200, `{"code":0,"data":{"response":{"data":{"accounts":[{"PackageName":"x","CycleCapacitySize":100,"CycleCapacityUsed":0}]}}}}`), nil
		})},
		ChatBaseCN:    "https://chat.example",
		BillingBaseCN: "https://billing.example",
		UserAgent:     "CustomAgent/1",
	}
	if _, _, err := client.UserResource(account); err != nil {
		t.Errorf("userResource: %v", err)
	}
}

// TestFetchModelsUsesConfiguredUA FetchModels 手工 Set UA 也走覆盖。
func TestFetchModelsUsesConfiguredUA(t *testing.T) {
	t.Parallel()

	account := &auth.Auth{AccessToken: "at", UID: "u1"}

	client := &Client{
		HTTP: &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
			switch {
			case strings.HasSuffix(r.URL.Path, "/console/enterprises/personal/models"):
				if got := r.Header.Get("User-Agent"); got != "FetchAgent/2" {
					t.Errorf("personal/models UA = %q want FetchAgent/2", got)
				}
			case strings.HasSuffix(r.URL.Path, "/v3/config"):
				if got := r.Header.Get("User-Agent"); got != codeBuddyIDEUA && !strings.Contains(got, "WorkBuddy/") {
					t.Errorf("v3/config UA = %q want IDE or desktop UA", got)
				}

				return jsonResp(200, `{"code":0,"data":{"models":[{"id":"glm-5.2","name":"GLM","maxInputTokens":131072,"maxOutputTokens":8192,"reasoning":{"effort":"high","supportedEfforts":[]},"disabled":false}]}}`), nil
			default:
				t.Errorf("path=%s", r.URL.Path)
			}

			return jsonResp(200, `{"code":0,"data":{"models":[{"id":"glm-5.2","name":"GLM","maxInputTokens":131072,"maxOutputTokens":8192,"reasoning":{"effort":"high","supportedEfforts":[]},"disabled":false}],"agents":[{"name":"cli","models":["glm-5.2"]}]}}`), nil
		})},
		ChatBaseCN:    "https://chat.example",
		BillingBaseCN: "https://billing.example",
		UserAgent:     "FetchAgent/2",
	}
	if _, err := client.FetchModels(account); err != nil {
		t.Errorf("fetchModels: %v", err)
	}
}

// --- A 段：UA 对齐官方 WorkBuddy 三段式 ---

const (
	defaultUAString      = "WorkBuddy/9.9.9 WorkBuddy/9.9.9 CLI/8.8.8"
	explicitString       = "MyCustomAgent/3.1"
	clientVerUAString    = "WorkBuddy/6.0.0 WorkBuddy/6.0.0 CLI/8.8.8"
	billingUAAgentString = "BillingAgent/1"
)

// TestUserAgentDefaultWorkBuddyShape 默认（无任何配置）聊天/刷新出站 UA =
// 官方 WorkBuddy 三段式，旧值 `CLI/2.63.2 CodeBuddy/2.63.2` 已被对齐替换。
func TestUserAgentDefaultWorkBuddyShape(t *testing.T) {
	t.Parallel()

	account := &auth.Auth{AccessToken: "at", UID: "u1"}
	client := &Client{
		HTTP:          &http.Client{Transport: uaCaptureTransport{ua: new(string)}},
		ChatHTTP:      &http.Client{Transport: uaCaptureTransport{ua: new(string)}},
		ChatBaseCN:    "https://chat.example",
		BillingBaseCN: "https://billing.example",
		Profile:       testProfileSet(t),
	}

	rc, status, _, err := client.ChatStream(account, []byte(`{"model":"deepseek-v4-flash","messages":[]}`), "", ChatMeta{})
	if status != 200 || err != nil {
		t.Fatalf("chat: status=%d err=%v", status, err)
	}

	if ua := client.chatLastUA(); ua != defaultUAString {
		t.Errorf("chat UA = %q want %q", ua, defaultUAString)
	}

	if rc != nil {
		rc.Close()
	}
}

// TestUserAgentGlobalChatBrand global 的 chat 品牌段取 CLI product.json 的
// applicationName（workbuddy-ai）：默认走 profile 字面量，显式版本覆盖时按 realm 重拼。
func TestUserAgentGlobalChatBrand(t *testing.T) {
	t.Parallel()

	global := &auth.Auth{UID: "u1", Domain: "www.workbuddy.ai"}
	global.BackfillRealm()

	for _, testCase := range []struct {
		name          string
		clientVersion string
		cliVersion    string
		want          string
	}{
		{
			name: "默认 profile 字面量",
			want: "WorkBuddy/9.9.10 workbuddy-ai/9.9.10 CLI/8.8.9",
		},
		{
			name:          "client_version 覆盖",
			clientVersion: "6.0.0",
			want:          "WorkBuddy/6.0.0 workbuddy-ai/6.0.0 CLI/8.8.9",
		},
		{
			name:       "仅 cli_version 覆盖",
			cliVersion: "7.7.7",
			want:       "WorkBuddy/9.9.10 workbuddy-ai/9.9.10 CLI/7.7.7",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			client := newProfileClient(t)
			client.ClientVersion = testCase.clientVersion
			client.CliVersion = testCase.cliVersion

			if got := client.defaultWorkBuddyUAFor(global); got != testCase.want {
				t.Errorf("global chat UA = %q want %q", got, testCase.want)
			}
		})
	}
}

// chatLastUA 从最近一次聊天请求捕获 UA（当前测试 Client 的 ChatHTTP transport 记录）。
func (c *Client) chatLastUA() string {
	if t, ok := c.ChatHTTP.Transport.(uaCaptureTransport); ok && t.ua != nil {
		return *t.ua
	}

	return ""
}

// TestUserAgentExplicitOverride config user_agent 非空时以用户显式值为准（兼容旧覆盖逻辑）。
func TestUserAgentExplicitOverride(t *testing.T) {
	t.Parallel()

	account := &auth.Auth{AccessToken: "at", UID: "u1", RefreshToken: "rt"}

	client := &Client{
		HTTP: &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
			if got := r.Header.Get("User-Agent"); got != explicitString {
				t.Errorf("UA = %q want %q (path=%s)", got, explicitString, r.URL.Path)
			}

			return jsonResp(200, `{"code":0,"data":{"accessToken":"nat","refreshToken":"nrt"}}`), nil
		})},
		ChatHTTP:      &http.Client{Transport: uaCaptureTransport{ua: new(string)}},
		ChatBaseCN:    "https://chat.example",
		BillingBaseCN: "https://billing.example",
		UserAgent:     explicitString,
	}
	if rc, status, _, err := client.ChatStream(account, []byte(`{"model":"deepseek-v4-flash","messages":[]}`), "", ChatMeta{}); status != 200 || err != nil {
		t.Errorf("chat: status=%d err=%v", status, err)
	} else if rc != nil {
		rc.Close()
	}

	if got := client.chatLastUA(); got != explicitString {
		t.Errorf("chat UA = %q want %q", got, explicitString)
	}

	if err := client.RefreshToken(account); err != nil {
		t.Errorf("refresh: %v", err)
	}
}

// TestUserAgentClientVersionOverride config client_version 生效：UA 的 WorkBuddy 段跟随
// 且成对相同（platform 段 = applicationName 段），CLI 段保持默认。
func TestUserAgentClientVersionOverride(t *testing.T) {
	t.Parallel()

	account := &auth.Auth{AccessToken: "at", UID: "u1"}
	client := &Client{
		HTTP:          &http.Client{Transport: uaCaptureTransport{ua: new(string)}},
		ChatHTTP:      &http.Client{Transport: uaCaptureTransport{ua: new(string)}},
		ChatBaseCN:    "https://chat.example",
		BillingBaseCN: "https://billing.example",
		ClientVersion: "6.0.0",
		Profile:       testProfileSet(t),
	}

	rc, status, _, err := client.ChatStream(account, []byte(`{"model":"deepseek-v4-flash","messages":[]}`), "", ChatMeta{})
	if status != 200 || err != nil {
		t.Fatalf("chat: status=%d err=%v", status, err)
	}

	if got := client.chatLastUA(); got != clientVerUAString {
		t.Errorf("UA = %q want %q", got, clientVerUAString)
	}

	if rc != nil {
		rc.Close()
	}
}

// TestBillingReportUA /v2/report 面 UA 是 desktop surface 的三段式（真机同形），
// 单段 `WorkBuddy/<ver>` 的旧口径不成立（#367）。显式 user_agent 优先；
// `client_name="SaaS"` 的免 UA 逃生门只作用于 billing meter 面，不影响 report。
func TestBillingReportUA(t *testing.T) {
	t.Parallel()

	account := &auth.Auth{AccessToken: "at", UID: "u1"}

	cases := []struct {
		name   string
		client *Client
		want   string
	}{
		{"profile_default", &Client{ClientName: "WorkBuddy", Profile: testProfileSet(t)}, defaultUAString},
		{"explicit_user_agent", &Client{ClientName: "WorkBuddy", UserAgent: billingUAAgentString}, billingUAAgentString},
		{"saas_keeps_desktop_ua", &Client{ClientName: "SaaS", Profile: testProfileSet(t)}, defaultUAString},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			var got, gotPath string

			client := testCase.client
			client.HTTP = &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
				got = r.Header.Get("User-Agent")
				gotPath = r.URL.Path

				return jsonResp(200, `{"code":0}`), nil
			})}
			client.ChatBaseCN = "https://chat.example"
			client.BillingBaseCN = "https://billing.example"

			if err := client.ReportChatActivity(account, "cid", ""); err != nil {
				t.Fatalf("report: %v", err)
			}

			if gotPath != reportPath {
				t.Errorf("path = %q want %q", gotPath, reportPath)
			}

			if got != testCase.want {
				t.Errorf("report UA = %q want %q", got, testCase.want)
			}
		})
	}
}

// billingMeterUAProfileFixture 测试 profile 冻结的 billing meter 抓包 UA（假值）。
const billingMeterUAProfileFixture = "axios/9.9.9"

// TestBillingMeterUAFromProfile billing meter 面 UA 取 profile 的抓包字段
// （官方该面没有桌面 UA 拦截器，UA 是 axios 默认串，#374）。
func TestBillingMeterUAFromProfile(t *testing.T) {
	t.Parallel()

	account := &auth.Auth{AccessToken: "at", UID: "u1"}

	var got string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("User-Agent")

		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"code":0,"data":{"response":{"data":{"accounts":[{"PackageName":"x","CycleCapacitySize":100,"CycleCapacityUsed":0}]}}}}`))
	}))
	defer srv.Close()

	client := &Client{
		HTTP:          srv.Client(),
		BillingBaseCN: srv.URL,
		ClientVersion: "6.0.0",
		Profile:       testProfileSet(t),
	}
	if _, _, err := client.UserResource(account); err != nil {
		t.Fatalf("userResource: %v", err)
	}

	if got != billingMeterUAProfileFixture {
		t.Errorf("billing meter UA = %q want %q（profile 抓包值）", got, billingMeterUAProfileFixture)
	}

	// 显式 user_agent 优先于 profile。
	client.UserAgent = "BillingAgent/1"

	if ua := client.billingMeterUA(account); ua != "BillingAgent/1" {
		t.Errorf("billingMeterUA() = %q want BillingAgent/1", ua)
	}

	client.UserAgent = ""
	client.ClientName = "SaaS"

	if ua := client.billingMeterUA(account); ua != "" {
		t.Errorf("billingMeterUA() SaaS = %q want empty", ua)
	}

	// client_name="SaaS" 是显式逃生门：不写 UA 键，回落 Go 默认 UA。
	if _, _, err := client.UserResource(account); err != nil {
		t.Fatalf("userResource SaaS: %v", err)
	}

	if got != "Go-http-client/1.1" {
		t.Errorf("SaaS billing meter UA = %q want %q", got, "Go-http-client/1.1")
	}
}

// TestBillingMeterUAWithoutCaptureFallsBackDesktop profile 未重抓（没有
// emit.billingMeterUA）时回落桌面三段式，行为与灰度前一致。
func TestBillingMeterUAWithoutCaptureFallsBackDesktop(t *testing.T) {
	t.Parallel()

	account := &auth.Auth{AccessToken: "at", UID: "u1"}

	client := &Client{Profile: testProfileSet(t)}
	client.Profile.ForRealm("cn").Emit.BillingMeterUA = ""

	if got, want := client.billingMeterUA(account), client.desktopUAFor(account); got != want {
		t.Errorf("billing meter UA = %q want %q（回落 desktopUA）", got, want)
	}
}

var _ = io.Discard
