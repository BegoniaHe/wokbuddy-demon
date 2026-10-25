package upstream

import (
	"net/http"
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

// TestGlobalRegisterBaseFollowsAccountDomain 注册激活链路与 billing 同口径：
// 显式 billing_base 优先；未配置时 codebuddy.ai 账号打官方国际站，其余回落旧桌面端。
func TestGlobalRegisterBaseFollowsAccountDomain(t *testing.T) {
	t.Parallel()

	client := testClient(nil)
	client.GlobalEnabled = true

	if got := client.globalRegisterBase(&auth.Auth{Domain: "www.codebuddy.ai"}); got != officialGlobalBase {
		t.Errorf("codebuddy.ai globalRegisterBase=%q want %q", got, officialGlobalBase)
	}

	if got := client.globalRegisterBase(&auth.Auth{Domain: "www.workbuddy.ai"}); got != defaultGlobalBase {
		t.Errorf("workbuddy.ai globalRegisterBase=%q want %q", got, defaultGlobalBase)
	}

	if got := client.globalRegisterBase(&auth.Auth{}); got != defaultGlobalBase {
		t.Errorf("empty domain globalRegisterBase=%q want %q", got, defaultGlobalBase)
	}

	client.BillingBaseGlobal = "https://billing-proxy.example"

	if got := client.globalRegisterBase(&auth.Auth{Domain: "www.codebuddy.ai"}); got != "https://billing-proxy.example" {
		t.Errorf("config override globalRegisterBase=%q want billing-proxy", got)
	}
}

// TestGlobalRegisterRequestUsesIssuingDomain 注册请求的主机与 Origin/Referer 必须
// 跟随签发域；否则 codebuddy.ai token 会被打到旧桌面端主机并 401。
func TestGlobalRegisterRequestUsesIssuingDomain(t *testing.T) {
	t.Parallel()

	var gotURL, gotOrigin, gotReferer string

	client := testClient(func(r *http.Request) (*http.Response, error) {
		gotURL = r.URL.String()
		gotOrigin = r.Header.Get("Origin")
		gotReferer = r.Header.Get("Referer")

		return jsonResp(http.StatusOK, `{"code":200,"msg":"register success"}`), nil
	})
	client.GlobalEnabled = true

	account := &auth.Auth{Domain: "www.codebuddy.ai", UID: "u1", AccessToken: "at"}

	activated, _, _, err := client.GlobalRegisterStatus(account)
	if err != nil {
		t.Fatalf("GlobalRegisterStatus: %v", err)
	}

	if !activated {
		t.Fatal("code=200 should report activated")
	}

	wantURL := officialGlobalBase + "/auth/realms/copilot/overseas/user/register?userId=u1"
	if gotURL != wantURL {
		t.Errorf("register URL=%q want %q", gotURL, wantURL)
	}

	if gotOrigin != officialGlobalBase {
		t.Errorf("Origin=%q want %q", gotOrigin, officialGlobalBase)
	}

	if gotReferer != officialGlobalBase+"/" {
		t.Errorf("Referer=%q want %q", gotReferer, officialGlobalBase+"/")
	}
}
