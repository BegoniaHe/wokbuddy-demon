package panel

import (
	"strings"
	"testing"
)

// TestLoginEndpoints realm→端点映射：global 三端点全在 workbuddy.ai 域，
// cn/空/非法值 → workbuddy.cn（官方主域）。
func TestLoginEndpoints(t *testing.T) {
	t.Parallel()

	cases := []struct {
		realm      string
		wantState  string
		wantOrigin string
	}{
		{"global", "https://www.workbuddy.ai/v2/plugin/auth/state?platform=workbuddy-ai", "https://www.workbuddy.ai"},
		{"cn", "https://www.workbuddy.cn/v2/plugin/auth/state?platform=workbuddy", "https://www.codebuddy.cn"},
		{"", "https://www.workbuddy.cn/v2/plugin/auth/state?platform=workbuddy", "https://www.codebuddy.cn"},
		{"weird", "https://www.workbuddy.cn/v2/plugin/auth/state?platform=workbuddy", "https://www.codebuddy.cn"},
	}
	for _, item := range cases {
		t.Run(item.realm, func(t *testing.T) {
			t.Parallel()

			ep := loginCandidatesWithIdentity(item.realm, defaultLoginIdentity(item.realm))[0]

			st, tok, acct, origin := ep.state, ep.token, ep.account, ep.origin
			if st != item.wantState {
				t.Errorf("realm=%q state=%q want %q", item.realm, st, item.wantState)
			}

			if origin != item.wantOrigin {
				t.Errorf("realm=%q origin=%q want %q", item.realm, origin, item.wantOrigin)
			}
			// token/account 端点必与 state 同 base。
			if item.realm == "global" {
				if tok[:len("https://www.workbuddy.ai")] != "https://www.workbuddy.ai" ||
					acct[:len("https://www.workbuddy.ai")] != "https://www.workbuddy.ai" {
					t.Errorf("global token/account endpoints should be workbuddy.ai: %q %q", tok, acct)
				}
			} else {
				if tok[:len("https://www.workbuddy.cn")] != "https://www.workbuddy.cn" ||
					acct[:len("https://www.workbuddy.cn")] != "https://www.workbuddy.cn" {
					t.Errorf("cn token/account endpoints should be workbuddy.cn: %q %q", tok, acct)
				}
			}
		})
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
