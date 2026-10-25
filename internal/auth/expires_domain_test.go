package auth

import (
	"fmt"
	"testing"
	"time"
)

// TestParseExpiresAtMillis 嵌套形与扁平形的毫秒 expiresAt 统一归一为秒；
// 秒值原样保留，NeedsRefresh 按归一后的秒判断。
func TestParseExpiresAtMillis(t *testing.T) {
	t.Parallel()

	future := time.Now().Add(time.Hour)
	millis := future.UnixMilli()
	seconds := future.Unix()

	cases := []struct {
		name string
		raw  string
	}{
		{
			"嵌套形毫秒",
			fmt.Sprintf(`{"auth":{"accessToken":"at","refreshToken":"rt","expiresAt":%d,"domain":""},"account":{"uid":"u1"}}`, millis),
		},
		{
			"扁平形毫秒",
			fmt.Sprintf(`{"accessToken":"at","refreshToken":"rt","expiresAt":%d,"uid":"u2"}`, millis),
		},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			t.Parallel()

			auth, err := Parse([]byte(item.raw))
			if err != nil {
				t.Fatalf("parse: %v", err)
			}

			if auth.ExpiresAt != seconds {
				t.Fatalf("ExpiresAt=%d want %d（毫秒归一为秒）", auth.ExpiresAt, seconds)
			}

			if !auth.NeedsRefresh(2 * time.Hour) {
				t.Error("一小时内到期应 NeedsRefresh(2h)=true")
			}

			if auth.NeedsRefresh(30 * time.Minute) {
				t.Error("半小时内未到期应 NeedsRefresh(30m)=false")
			}
		})
	}

	// 秒值（当前量级）不得被归一化。
	parsed, err := Parse([]byte(fmt.Sprintf(
		`{"accessToken":"at","refreshToken":"rt","expiresAt":%d,"uid":"u3"}`, seconds,
	)))
	if err != nil {
		t.Fatalf("parse seconds: %v", err)
	}

	if parsed.ExpiresAt != seconds {
		t.Errorf("秒值被改写: got %d want %d", parsed.ExpiresAt, seconds)
	}
}

// TestCodebuddyAIDomainGlobal codebuddy.ai 裸域与任意子域判 global；
// codebuddy.cn 与空 domain 仍判 cn。
func TestCodebuddyAIDomainGlobal(t *testing.T) {
	t.Parallel()

	cases := []struct {
		domain string
		want   string
	}{
		{"www.codebuddy.ai", "global"},
		{"codebuddy.ai", "global"},
		{"sub.codebuddy.ai", "global"},
		{"  WWW.CodeBuddy.AI  ", "global"},
		{"www.codebuddy.cn", "cn"},
		{"codebuddy.cn", "cn"},
		{"", "cn"},
	}
	for _, item := range cases {
		if got := ResolveRealm("", item.domain); got != item.want {
			t.Errorf("ResolveRealm(%q)=%q want %q", item.domain, got, item.want)
		}

		auth := &Auth{Domain: item.domain}
		if _, got := auth.BackfillRealm(); got != item.want {
			t.Errorf("BackfillRealm(%q)=%q want %q", item.domain, got, item.want)
		}
	}
}

// TestCodebuddyAIIssuerGlobal iss 命中的 codebuddy.ai 家族同样判 global。
func TestCodebuddyAIIssuerGlobal(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		iss  string
		want string
	}{
		{"codebuddy.ai 裸域", "https://codebuddy.ai/realms/x", "global"},
		{"www.codebuddy.ai", "https://www.codebuddy.ai/auth/realms/copilot", "global"},
		{"codebuddy.cn", "https://www.codebuddy.cn/auth/realms/copilot", ""},
	}
	for _, item := range cases {
		if got := realmFromIssuer(jwtWithIss(item.iss)); got != item.want {
			t.Errorf("%s: realmFromIssuer=%q want %q", item.name, got, item.want)
		}
	}
}
