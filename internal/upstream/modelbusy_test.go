package upstream

import (
	"context"
	"net/http"
	"testing"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
)

// TestModelBusyParsesEnvelope 兼容 {code,data} 与数组响应，按 50 分批（issue #258）。
func TestModelBusyParsesEnvelope(t *testing.T) {
	t.Parallel()

	calls := 0
	client := testClient(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != "/v2/chat/busy" {
			t.Fatalf("path=%s", r.URL.Path)
		}

		calls++

		return jsonResp(200, `{"code":0,"data":{"glm-5.2":"busy","deepseek-v4.1-flash":{"level":"idle"}}}`), nil
	})

	levels, err := client.ModelBusy(context.Background(), &auth.Auth{UID: "u1", AccessToken: "at"}, []string{"glm-5.2", "deepseek-v4.1-flash"})
	if err != nil {
		t.Fatal(err)
	}

	if calls != 1 {
		t.Fatalf("calls=%d", calls)
	}

	if levels["glm-5.2"] != "busy" || levels["deepseek-v4.1-flash"] != "idle" {
		t.Fatalf("levels=%v", levels)
	}
}

// TestModelBusyBatches50 55 个模型分 2 批。
func TestModelBusyBatches50(t *testing.T) {
	t.Parallel()

	calls := 0
	client := testClient(func(r *http.Request) (*http.Response, error) {
		calls++

		return jsonResp(200, `{"code":0,"data":[]}`), nil
	})

	models := make([]string, 55)
	for i := range models {
		models[i] = "m" + string(rune('a'+i%26))
	}

	if _, err := client.ModelBusy(context.Background(), &auth.Auth{UID: "u1", AccessToken: "at"}, models); err != nil {
		t.Fatal(err)
	}

	if calls != 2 {
		t.Fatalf("calls=%d want 2", calls)
	}
}
