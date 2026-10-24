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

// TestModelBusyParsesOfficialArray 官方数组响应用 busy_level，且不得把字段名当模型。
func TestModelBusyParsesOfficialArray(t *testing.T) {
	t.Parallel()

	client := testClient(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != "/v2/chat/busy" {
			t.Fatalf("path=%s", r.URL.Path)
		}

		return jsonResp(200, `{"code":0,"data":[{"model":"glm-5.2","busy_level":"congested"},{"model":"deepseek-v4.1-flash","level":"IDLE"}]}`), nil
	})

	levels, err := client.ModelBusy(context.Background(), &auth.Auth{UID: "u1", AccessToken: "at"}, []string{"glm-5.2", "deepseek-v4.1-flash"})
	if err != nil {
		t.Fatal(err)
	}

	if levels["glm-5.2"] != "congested" || levels["deepseek-v4.1-flash"] != "idle" {
		t.Fatalf("levels=%v", levels)
	}

	if _, ok := levels["busy_level"]; ok {
		t.Fatalf("busy_level must not be a model key: %v", levels)
	}
}

// TestModelBusyNestedContainers 数组里再套容器（数组 / 包装 map）仍要递归解析，
// 不能因为外层项没有 model/busy_level 就整段丢弃。
func TestModelBusyNestedContainers(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"数组套数组":  `{"code":0,"data":[[{"model":"glm-5.2","busy_level":"idle"}]]}`,
		"包装 map": `{"code":0,"data":[{"list":[{"model":"glm-5.2","busy_level":"idle"}]}]}`,
	}

	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if levels := parseModelBusy([]byte(body)); levels["glm-5.2"] != "idle" {
				t.Fatalf("levels=%v", levels)
			}
		})
	}
}

// TestModelBusyInvalidLevelsFallBackUnknown 官方缺失/非法 busy_level 不写入结果
// （调用方按 unknown 处理），旧 level 键仍兼容。
func TestModelBusyInvalidLevelsFallBackUnknown(t *testing.T) {
	t.Parallel()

	client := testClient(func(*http.Request) (*http.Response, error) {
		return jsonResp(200, `{"code":0,"data":[{"model":"glm-5.2","busy_level":"bogus"},{"model":"kimi","busy_level":"BUSY"}]}`), nil
	})

	levels, err := client.ModelBusy(context.Background(), &auth.Auth{UID: "u1", AccessToken: "at"}, []string{"glm-5.2", "kimi"})
	if err != nil {
		t.Fatal(err)
	}

	if _, ok := levels["glm-5.2"]; ok {
		t.Fatalf("invalid level must not be stored: %v", levels)
	}

	if levels["kimi"] != "busy" {
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
