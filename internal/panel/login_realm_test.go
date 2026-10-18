package panel

import (
	"testing"
)

// TestLoginEndpoints realm→端点映射：global 三端点全在 workbuddy.ai 域，
// cn/空/非法值 → copilot.tencent.com（零回归兜底）。
func TestLoginEndpoints(t *testing.T) {
	t.Parallel()

	cases := []struct {
		realm      string
		wantState  string
		wantOrigin string
	}{
		{"global", "https://www.workbuddy.ai/v2/plugin/auth/state?platform=workbuddy-ai", "https://www.workbuddy.ai"},
		{"cn", "https://copilot.tencent.com/v2/plugin/auth/state?platform=workbuddy", "https://www.codebuddy.cn"},
		{"", "https://copilot.tencent.com/v2/plugin/auth/state?platform=workbuddy", "https://www.codebuddy.cn"},
		{"weird", "https://copilot.tencent.com/v2/plugin/auth/state?platform=workbuddy", "https://www.codebuddy.cn"},
	}
	for _, item := range cases {
		t.Run(item.realm, func(t *testing.T) {
			t.Parallel()

			st, tok, acct, origin := loginEndpoints(item.realm)
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
				if tok[:len("https://copilot.tencent.com")] != "https://copilot.tencent.com" ||
					acct[:len("https://copilot.tencent.com")] != "https://copilot.tencent.com" {
					t.Errorf("cn token/account endpoints should be copilot.tencent.com: %q %q", tok, acct)
				}
			}
		})
	}
}
