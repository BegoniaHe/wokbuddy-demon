package upstream

import (
	"testing"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
)

// TestGlobalBasesFollowAccountDomain 国际域两套主机名并存（官方 CLI 新登录
// codebuddy.ai、旧桌面端 workbuddy.ai），token 只在签发域可用：chat/billing/
// web 与 refresh 共用的 chatOrigins 必须按 auth.domain 选主机。
func TestGlobalBasesFollowAccountDomain(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		domain   string
		explicit string
		want     string
	}{
		{"官方国际站裸域", "codebuddy.ai", "", officialGlobalBase},
		{"官方国际站子域", "www.codebuddy.ai", "", officialGlobalBase},
		{"官方国际站大小写空白", "  WWW.CodeBuddy.AI  ", "", officialGlobalBase},
		{"旧桌面端", "www.workbuddy.ai", "", defaultGlobalBase},
		{"缺省 domain 回落旧桌面端", "", "global", defaultGlobalBase},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			t.Parallel()

			client := testClient(nil)
			client.GlobalEnabled = true

			account := &auth.Auth{Domain: item.domain}
			if item.explicit != "" {
				if _, err := auth.BackfillRealmFor(account, item.explicit); err != nil {
					t.Fatal(err)
				}
			}

			if got := client.chatBase(account); got != item.want {
				t.Errorf("chatBase=%q want %q", got, item.want)
			}

			if got := client.billingBase(account); got != item.want {
				t.Errorf("billingBase=%q want %q", got, item.want)
			}

			if got := client.webBase(account); got != item.want {
				t.Errorf("webBase=%q want %q", got, item.want)
			}

			origins := client.chatOrigins(account)
			if len(origins) != 1 || origins[0] != item.want {
				t.Errorf("chatOrigins=%v want [%q]", origins, item.want)
			}
		})
	}
}

// TestGlobalBaseConfigOverrideWins 显式 global.chat_base / billing_base 仍是
// 最高优先级：按 domain 选主机只在未配置时生效。
func TestGlobalBaseConfigOverrideWins(t *testing.T) {
	t.Parallel()

	client := testClient(nil)
	client.GlobalEnabled = true
	client.ChatBaseGlobal = "https://chat-proxy.example"
	client.BillingBaseGlobal = "https://billing-proxy.example"

	account := &auth.Auth{Domain: "www.codebuddy.ai"}

	if got := client.chatBase(account); got != "https://chat-proxy.example" {
		t.Errorf("chatBase=%q want config override", got)
	}

	if got := client.billingBase(account); got != "https://billing-proxy.example" {
		t.Errorf("billingBase=%q want config override", got)
	}
}
