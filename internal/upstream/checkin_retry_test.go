// checkin_retry_test.go 钉住签到/余额维护类计费调用的瞬时错误有界重试：
// 上游 5xx（实测偶发 code 10000 / http 500）与网络抖动重试，业务错误（已签到/
// 参数错）不重试——重试只会原样再失败一次。
package upstream

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
)

var errWrongPath = errors.New("wrong path")

// shortBillingRetry 测试用重试间隔（生产 2s 会让单测秒级膨胀）。
func shortBillingRetry(t *testing.T) {
	t.Helper()
}

func TestDailyCheckinRetriesTransient500(t *testing.T) {
	t.Parallel()
	shortBillingRetry(t)

	var calls atomic.Int32

	client := testClient(func(r *http.Request) (*http.Response, error) {
		if !strings.HasSuffix(r.URL.Path, "/v2/billing/meter/daily-checkin") {
			return nil, errWrongPath
		}

		if calls.Add(1) == 1 {
			return jsonResp(500, `{"code":10000,"msg":"API request failed with status code: 500"}`), nil
		}

		return jsonResp(200, `{"code":0,"data":{}}`), nil
	})
	if err := client.DailyCheckin(context.Background(), &auth.Auth{AccessToken: "at"}); err != nil {
		t.Fatalf("首次 500 应重试成功，err=%v", err)
	}

	if n := calls.Load(); n != 2 {
		t.Fatalf("calls=%d want 2（1 次失败 + 1 次重试）", n)
	}
}

func TestDailyCheckinRetriesExhausted(t *testing.T) {
	t.Parallel()
	shortBillingRetry(t)

	var calls atomic.Int32

	c := testClient(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		return jsonResp(500, `{"code":10000,"msg":"API request failed with status code: 500"}`), nil
	})

	err := c.DailyCheckin(context.Background(), &auth.Auth{AccessToken: "at"})
	if err == nil {
		t.Fatal("持续 500 应返回错误")
	}

	if n := calls.Load(); n != 3 {
		t.Fatalf("calls=%d want 3（1 次 + 2 次重试封顶）", n)
	}
}

func TestDailyCheckinNoRetryOnBusinessError(t *testing.T) {
	t.Parallel()
	shortBillingRetry(t)

	var calls atomic.Int32

	client := testClient(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		// 「今天已签到」是业务幂等拒绝（code!=0），重试无意义。
		return jsonResp(200, `{"code":14001,"msg":"今日已签到"}`), nil
	})

	err := client.DailyCheckin(context.Background(), &auth.Auth{AccessToken: "at"})
	if err == nil || !IsAlreadyCheckin(err) {
		t.Fatalf("err=%v want already-checkin business error", err)
	}

	if n := calls.Load(); n != 1 {
		t.Fatalf("calls=%d want 1（业务错误不重试）", n)
	}
}

func TestDailyCheckinNoRetryOn4xx(t *testing.T) {
	t.Parallel()
	shortBillingRetry(t)

	var calls atomic.Int32

	c := testClient(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		return jsonResp(403, `{"code":11140,"msg":"request illegal"}`), nil
	})
	if err := c.DailyCheckin(context.Background(), &auth.Auth{AccessToken: "at"}); err == nil {
		t.Fatal("403 应返回错误")
	}

	if n := calls.Load(); n != 1 {
		t.Fatalf("calls=%d want 1（4xx 非瞬时，不重试）", n)
	}
}

func TestUserResourceDetailedRetriesTransient500(t *testing.T) {
	t.Parallel()
	shortBillingRetry(t)

	var calls atomic.Int32

	client := testClient(func(r *http.Request) (*http.Response, error) {
		if !strings.HasSuffix(r.URL.Path, "/billing/meter/get-user-resource") {
			return nil, errWrongPath
		}

		if calls.Add(1) == 1 {
			return jsonResp(500, `{"code":10000,"msg":"API request failed with status code: 500"}`), nil
		}

		return jsonResp(200, `{"code":0,"data":{"Response":{"Data":{"Accounts":[
			{"PackageName":"p","CapacitySize":100,"CapacityRemain":60}
		]}}}}`), nil
	})

	remain, _, _, _, _, err := client.UserResourceDetailedWithExpiry(context.Background(), &auth.Auth{AccessToken: "at"}, 0)
	if err != nil || remain != 60 {
		t.Fatalf("remain=%d err=%v, want 60 nil", remain, err)
	}

	if n := calls.Load(); n != 2 {
		t.Fatalf("calls=%d want 2（1 次失败 + 1 次重试）", n)
	}
}
