package upstream

import (
	"net/http"
	"testing"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
)

// TestBalanceObserverNotifies UserResource 成功后触发余额观测回调。
func TestBalanceObserverNotifies(t *testing.T) {
	t.Parallel()

	client := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(200, `{"code":0,"data":{"Response":{"Data":{"Accounts":[
			{"CycleCapacitySize":1000,"CycleCapacityRemain":900,"CycleCapacityUsed":100}
		]}}}}`), nil
	})

	var gotUID string

	var gotRemain, gotTotal int64

	client.BalanceObserver = func(uid string, remain, total int64) {
		gotUID, gotRemain, gotTotal = uid, remain, total
	}

	if _, _, err := client.UserResource(&auth.Auth{AccessToken: "at", UID: "u1"}); err != nil {
		t.Fatalf("resource: %v", err)
	}

	if gotUID != "u1" || gotRemain != 900 || gotTotal != 1000 {
		t.Fatalf("observer got %s/%d/%d want u1/900/1000", gotUID, gotRemain, gotTotal)
	}
}

// TestBalanceObserverNotifiesSchedulerPath scheduler / 面板「检查」走的是
// UserResourceDetailedWithExpiry；回调必须挂在公共出口，否则账本永远只有登录基线。
func TestBalanceObserverNotifiesSchedulerPath(t *testing.T) {
	t.Parallel()

	client := testClient(func(*http.Request) (*http.Response, error) {
		return jsonResp(200, `{"code":0,"data":{"Response":{"Data":{"Accounts":[
			{"CycleCapacitySize":1000,"CycleCapacityRemain":800,"CycleCapacityUsed":200}
		]}}}}`), nil
	})

	var calls int

	client.BalanceObserver = func(uid string, remain, total int64) {
		calls++

		if uid != "u1" || remain != 800 || total != 1000 {
			t.Errorf("observer got %s/%d/%d want u1/800/1000", uid, remain, total)
		}
	}

	if _, _, _, _, _, err := client.UserResourceDetailedWithExpiry(&auth.Auth{AccessToken: "at", UID: "u1"}, 0); err != nil {
		t.Fatalf("resource: %v", err)
	}

	if calls != 1 {
		t.Fatalf("observer calls=%d want 1", calls)
	}
}

// TestBalanceObserverPanicIsolated 回调 panic 不得打断余额查询主路径。
func TestBalanceObserverPanicIsolated(t *testing.T) {
	t.Parallel()

	client := testClient(func(*http.Request) (*http.Response, error) {
		return jsonResp(200, `{"code":0,"data":{"Response":{"Data":{"Accounts":[
			{"CycleCapacitySize":10,"CycleCapacityRemain":10,"CycleCapacityUsed":0}
		]}}}}`), nil
	})
	client.BalanceObserver = func(string, int64, int64) { panic("boom") }

	remain, total, err := client.UserResource(&auth.Auth{AccessToken: "at", UID: "u1"})
	if err != nil {
		t.Fatalf("回调 panic 不应影响查询结果: %v", err)
	}

	if remain != 10 || total != 10 {
		t.Fatalf("remain/total=%d/%d want 10/10", remain, total)
	}
}
