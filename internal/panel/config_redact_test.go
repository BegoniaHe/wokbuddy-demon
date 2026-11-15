// config_redact_test.go 钉住「配置回显不含代理凭据」：代理 URL 按协议支持
// user:pass，订阅 URL 常把 token 放在 path/query，与 api_key 同属凭证。
package panel

import (
	"net/http"
	"strings"
	"testing"

	"github.com/BegoniaHe/wokbuddy-demon/internal/pool"
)

func TestGetConfigRedactsProxyCredentials(t *testing.T) {
	t.Parallel()

	pnl := New(&Config{
		Pool:       pool.New(""),
		APIKey:     "data-key",
		ConfigPath: "config.json",
		LoadConfig: func() (any, error) {
			return map[string]any{
				"api_key": "data-key",
				"upstream": map[string]any{
					"proxies": []string{"socks5://alice:s3cret@10.0.0.1:1080"},
					"proxy_slots": []map[string]any{
						{"id": "cn", "url": "http://bob:hunter2@10.0.0.2:8080"},
					},
					"proxy_subscriptions": []string{"https://sub.example/api?token=tok-abc"},
				},
			}, nil
		},
	})

	rec := panelDo(pnl, http.MethodGet, "/panel/api/config", "data-key")
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}

	body := rec.Body.String()

	for _, secret := range []string{"s3cret", "hunter2", "alice", "bob", "tok-abc"} {
		if strings.Contains(body, secret) {
			t.Fatalf("回显泄露代理凭据 %q: %s", secret, body)
		}
	}

	for _, want := range []string{"socks5://10.0.0.1:1080", "http://10.0.0.2:8080", "https://sub.example"} {
		if !strings.Contains(body, want) {
			t.Fatalf("掩码形态缺失 %q: %s", want, body)
		}
	}
}
