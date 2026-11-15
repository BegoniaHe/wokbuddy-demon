package panel

import (
	"net/url"
	"regexp"
	"strings"
	"testing"
)

// TestLoginEndpoints realm→端点映射：global 三端点全在 workbuddy.ai 域，
// cn/空/非法值 → workbuddy.cn（官方主域）。
func TestLoginEndpoints(t *testing.T) {
	t.Parallel()

	cases := []struct {
		realm        string
		wantBase     string
		wantPlatform string
		wantOrigin   string
	}{
		{"global", "https://www.workbuddy.ai", "workbuddy-ai", "https://www.workbuddy.ai"},
		{"cn", "https://www.workbuddy.cn", "workbuddy", "https://www.codebuddy.cn"},
		{"", "https://www.workbuddy.cn", "workbuddy", "https://www.codebuddy.cn"},
		{"weird", "https://www.workbuddy.cn", "workbuddy", "https://www.codebuddy.cn"},
	}
	for _, item := range cases {
		t.Run(item.realm, func(t *testing.T) {
			t.Parallel()

			endpoint := loginCandidatesWithIdentity(item.realm, defaultLoginIdentity(item.realm))[0]

			parsed, err := url.Parse(endpoint.state)
			if err != nil {
				t.Fatalf("realm=%q state=%q: %v", item.realm, endpoint.state, err)
			}

			if got := parsed.Scheme + "://" + parsed.Host + parsed.Path; got != item.wantBase+"/v2/plugin/auth/state" {
				t.Errorf("realm=%q state=%q want base %q", item.realm, endpoint.state, item.wantBase)
			}

			if got := parsed.Query().Get("platform"); got != item.wantPlatform {
				t.Errorf("realm=%q platform=%q want %q", item.realm, got, item.wantPlatform)
			}

			tok, acct, origin := endpoint.token, endpoint.account, endpoint.origin
			if origin != item.wantOrigin {
				t.Errorf("realm=%q origin=%q want %q", item.realm, origin, item.wantOrigin)
			}
			// token/account 端点必与 state 同 base。
			if tok[:len(item.wantBase)] != item.wantBase || acct[:len(item.wantBase)] != item.wantBase {
				t.Errorf("realm=%q token/account endpoints should be %s: %q %q", item.realm, item.wantBase, tok, acct)
			}
		})
	}
}

// TestLoginStateURLJourneyParams 授权链接补齐 version 与 loginSessionId（issue #534）：
// version 来自部署 profile（空则不补），loginSessionId 只在 CN 站（platform=workbuddy）。
func TestLoginStateURLJourneyParams(t *testing.T) {
	t.Parallel()

	panel := loginTestPanel(t)

	cnState := loginCandidatesWithIdentity("cn", panel.loginIdentityFor("cn"))[0].state
	cnQuery := mustStateQuery(t, cnState)
	assertJourneyParam(t, cnQuery, "version", "9.9.9")
	assertUUIDv4(t, cnQuery.Get("loginSessionId"))

	globalState := loginCandidatesWithIdentity("global", panel.loginIdentityFor("global"))[0].state
	globalQuery := mustStateQuery(t, globalState)
	assertJourneyParam(t, globalQuery, "version", "9.9.10")

	if got := globalQuery.Get("loginSessionId"); got != "" {
		t.Errorf("global 链接不得带 loginSessionId（官方只在 CN 站追加），got %q", got)
	}

	// 无部署 profile 的缺省身份不补 version，但 CN 仍带现场生成的 loginSessionId。
	bareQuery := mustStateQuery(t, loginStateURL(upstreamBaseCN, defaultLoginIdentity("cn")))
	if got := bareQuery.Get("version"); got != "" {
		t.Errorf("无 profile 时不得补 version，got %q", got)
	}

	assertUUIDv4(t, bareQuery.Get("loginSessionId"))
}

// mustStateQuery 解析 auth/state 的查询参数。
func mustStateQuery(t *testing.T, raw string) url.Values {
	t.Helper()

	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse state url %q: %v", raw, err)
	}

	return parsed.Query()
}

// assertJourneyParam 断言查询参数取值。
func assertJourneyParam(t *testing.T, query url.Values, key, want string) {
	t.Helper()

	if got := query.Get(key); got != want {
		t.Errorf("%s=%q want %q", key, got, want)
	}
}

// uuidV4Pattern UUIDv4 字面量（version=4、variant=10xx）。
var uuidV4Pattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

// assertUUIDv4 断言 loginSessionId 是 UUIDv4。
func assertUUIDv4(t *testing.T, got string) {
	t.Helper()

	if !uuidV4Pattern.MatchString(got) {
		t.Errorf("loginSessionId=%q 不是 UUIDv4", got)
	}
}

// TestLoginCandidatesCNFallback CN origin 顺序与 chat/refresh 一致：主域后回落旧域。
func TestLoginCandidatesCNFallback(t *testing.T) {
	t.Parallel()

	candidates := loginCandidatesWithIdentity("cn", defaultLoginIdentity("cn"))

	want := []string{
		"https://www.workbuddy.cn",
		"https://copilot.tencent.com",
		"https://wb.tencentbuddy.com",
	}
	if len(candidates) != len(want) {
		t.Fatalf("candidates=%d want %d", len(candidates), len(want))
	}

	for i := range want {
		if candidates[i].base != want[i] {
			t.Errorf("candidate[%d]=%q want %q", i, candidates[i].base, want[i])
		}
	}

	preferred := loginCandidatesPreferWithIdentity("cn", "https://copilot.tencent.com", defaultLoginIdentity("cn"))
	if preferred[0].base != "https://copilot.tencent.com" {
		t.Errorf("preferred base=%q", preferred[0].base)
	}
}

// TestLoginCandidatesUseIdentity profile 的 loginUA / loginPlatform 必须进入
// auth/state 的 platform 参数与请求 UA，而不是固定旧 CLI 身份。
func TestLoginCandidatesUseIdentity(t *testing.T) {
	t.Parallel()

	identity := loginIdentity{
		userAgent: "WorkBuddy/9.9.9 WorkBuddy/9.9.9 CLI/9.9.9",
		platform:  "workbuddy-custom",
	}

	endpoint := loginCandidatesWithIdentity("cn", identity)[0]
	if endpoint.state != "https://www.workbuddy.cn/v2/plugin/auth/state?platform=workbuddy-custom" {
		t.Errorf("state=%q", endpoint.state)
	}

	if endpoint.userAgent != identity.userAgent {
		t.Errorf("ua=%q want %q", endpoint.userAgent, identity.userAgent)
	}

	escaped := loginCandidatesWithIdentity("cn", loginIdentity{userAgent: "ua", platform: "a&b"})[0]
	if !strings.Contains(escaped.state, "platform=a%26b") {
		t.Errorf("platform 未转义: %q", escaped.state)
	}
}
