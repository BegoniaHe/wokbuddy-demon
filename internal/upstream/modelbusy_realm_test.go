package upstream

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
)

// globalBusyAccount global 域账号（上游没部署 /v2/chat/busy，一律 404）。
func globalBusyAccount() *auth.Auth {
	return &auth.Auth{
		UID: "u-gl", AccessToken: "at-gl", ExpiresAt: 9999999999, Domain: "www.workbuddy.ai",
	}
}

// TestModelBusyRemembersMissingRealm 404 是「该域没有这条观测」的结构性事实：记住后
// 本进程不再重复请求，且不产生错误（issue #401）。
func TestModelBusyRemembersMissingRealm(t *testing.T) {
	t.Parallel()

	calls := 0
	client := testClient(func(r *http.Request) (*http.Response, error) {
		calls++

		if r.URL.Path != "/v2/chat/busy" {
			t.Fatalf("path=%s", r.URL.Path)
		}

		return jsonResp(http.StatusNotFound, `{"error_msg":"404 Route Not Found"}`), nil
	})

	for attempt := range 2 {
		levels, err := client.ModelBusy(context.Background(), globalBusyAccount(), []string{"glm-5.2"})
		if err != nil {
			t.Fatalf("attempt %d: err=%v want nil（404 视为该域没有观测）", attempt, err)
		}

		if len(levels) != 0 {
			t.Fatalf("attempt %d: levels=%v want 空表", attempt, levels)
		}
	}

	if calls != 1 {
		t.Fatalf("calls=%d want 1（第二次应短路，不再打上游）", calls)
	}

	if !client.modelBusyRealmMissing("global") {
		t.Fatal("记忆未落到 global 桶")
	}
}

// TestModelBusyRealmMemoryIsolated 记忆按域分桶：global 记住 404 不影响 CN 观测。
func TestModelBusyRealmMemoryIsolated(t *testing.T) {
	t.Parallel()

	cnCalls := 0
	client := testClient(func(r *http.Request) (*http.Response, error) {
		if strings.Contains(r.Header.Get("Authorization"), "at-gl") {
			return jsonResp(http.StatusNotFound, `{"error_msg":"404 Route Not Found"}`), nil
		}

		cnCalls++

		return jsonResp(http.StatusOK, `{"code":0,"data":{"glm-5.2":"busy"}}`), nil
	})

	if _, err := client.ModelBusy(context.Background(), globalBusyAccount(), []string{"glm-5.2"}); err != nil {
		t.Fatalf("global: %v", err)
	}

	levels, err := client.ModelBusy(context.Background(), &auth.Auth{UID: "u-cn", AccessToken: "at"}, []string{"glm-5.2"})
	if err != nil {
		t.Fatalf("cn: %v", err)
	}

	if cnCalls != 1 || levels["glm-5.2"] != "busy" {
		t.Fatalf("cnCalls=%d levels=%v want 1/busy（global 的记忆不应波及 CN）", cnCalls, levels)
	}
}

// TestModelBusyTransientErrorNotRemembered 5xx / 超时是偶发失败，不能记忆：否则一次
// 抖动就会让该域整轮观测静默消失（issue #401）。
func TestModelBusyTransientErrorNotRemembered(t *testing.T) {
	t.Parallel()

	calls := 0
	client := testClient(func(*http.Request) (*http.Response, error) {
		calls++

		return jsonResp(http.StatusInternalServerError, `{"code":500,"msg":"boom"}`), nil
	})

	for attempt := range 2 {
		if _, err := client.ModelBusy(context.Background(), globalBusyAccount(), []string{"glm-5.2"}); err == nil {
			t.Fatalf("attempt %d: 500 应报错", attempt)
		}
	}

	if calls != 2 {
		t.Fatalf("calls=%d want 2（偶发失败不记忆）", calls)
	}

	if client.modelBusyRealmMissing("global") {
		t.Fatal("5xx 不应置位记忆")
	}
}

// TestModelBusyRealmKey 无账号不落桶；空串永不命中记忆。
func TestModelBusyRealmKey(t *testing.T) {
	t.Parallel()

	if realm := modelBusyRealm(nil); realm != "" {
		t.Fatalf("nil 账号 realm=%q want 空串", realm)
	}

	client := testClient(func(*http.Request) (*http.Response, error) {
		return jsonResp(http.StatusNotFound, `{"error_msg":"404 Route Not Found"}`), nil
	})

	if client.modelBusyRealmMissing("") {
		t.Fatal("空 realm 不应命中记忆")
	}

	// 无账号时按空 realm 处理：既不被记忆短路，也不写入记忆。
	if _, err := client.ModelBusy(context.Background(), nil, []string{"glm-5.2"}); err != nil {
		t.Fatalf("nil 账号: %v", err)
	}

	if client.modelBusyRealmMissing("cn") || client.modelBusyRealmMissing("global") {
		t.Fatal("nil 账号不应写入任何域桶")
	}
}
