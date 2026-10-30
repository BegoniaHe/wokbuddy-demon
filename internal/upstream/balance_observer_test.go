package upstream

import (
	"context"
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

	if _, _, err := client.UserResource(context.Background(), &auth.Auth{AccessToken: "at", UID: "u1"}); err != nil {
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

	if _, _, _, _, _, err := client.UserResourceDetailedWithExpiry(context.Background(), &auth.Auth{AccessToken: "at", UID: "u1"}, 0); err != nil {
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

	remain, total, err := client.UserResource(context.Background(), &auth.Auth{AccessToken: "at", UID: "u1"})
	if err != nil {
		t.Fatalf("回调 panic 不应影响查询结果: %v", err)
	}

	if remain != 10 || total != 10 {
		t.Fatalf("remain/total=%d/%d want 10/10", remain, total)
	}
}

// TestBalanceObserverNotifiesSummaryPath summary 是主余额路径：观测必须挂在它上面，
// 这样 issue #200 把 scheduler 改走 UserResourceSnapshot 后仍能记录周期刷新。
func TestBalanceObserverNotifiesSummaryPath(t *testing.T) {
	t.Parallel()

	client := testClient(func(*http.Request) (*http.Response, error) {
		return jsonResp(200, `{"code":0,"data":{"Packages":[{"CycleRemainCapacity":700,"CycleTotalCapacity":1000}]}}`), nil
	})

	var calls int

	client.BalanceObserver = func(uid string, remain, total int64) {
		calls++

		if uid != "u1" || remain != 700 || total != 1000 {
			t.Errorf("observer got %s/%d/%d want u1/700/1000", uid, remain, total)
		}
	}

	if _, err := client.fetchResourceSummary(context.Background(), &auth.Auth{AccessToken: "at", UID: "u1"}); err != nil {
		t.Fatalf("summary: %v", err)
	}

	if calls != 1 {
		t.Fatalf("observer calls=%d want 1", calls)
	}
}

// TestBalanceObserverSkipsEmptySummary summary 返回空 packages 时会回落旧端点：
// 此时不能按 0/0 记一笔假变动。
func TestBalanceObserverSkipsEmptySummary(t *testing.T) {
	t.Parallel()

	client := testClient(func(*http.Request) (*http.Response, error) {
		return jsonResp(200, `{"code":0,"data":{"packages":[]}}`), nil
	})

	var calls int

	client.BalanceObserver = func(string, int64, int64) { calls++ }

	if _, err := client.fetchResourceSummary(context.Background(), &auth.Auth{AccessToken: "at", UID: "u1"}); err != nil {
		t.Fatalf("summary: %v", err)
	}

	if calls != 0 {
		t.Fatalf("空 packages 不应触发观测，calls=%d", calls)
	}
}

// TestBalanceObserverNotifiesEnterprisePath 企业口径的观测回调挂在 enterpriseResource
// 本身：后续调用方（如 UserResourceSnapshot）直接走该路径时也能记账，不依赖包装层。
func TestBalanceObserverNotifiesEnterprisePath(t *testing.T) {
	t.Parallel()

	client := testClient(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/v2/billing/meter/get-enterprise-user-usage" {
			return jsonResp(200, `{"code":0,"data":{"limitNum":1000,"credit":250}}`), nil
		}

		return jsonResp(404, `{"code":404,"msg":"not found"}`), nil
	})

	var (
		calls     int
		gotRemain int64
		gotTotal  int64
	)

	client.BalanceObserver = func(uid string, remain, total int64) {
		calls++

		if uid != "e1" {
			t.Errorf("observer uid=%q want e1", uid)
		}

		gotRemain, gotTotal = remain, total
	}

	account := &auth.Auth{UID: "e1", AccessToken: "at", EnterpriseID: "ent-1"}
	if _, _, _, _, _, err := client.UserResourceDetailedWithExpiry(context.Background(), account, 0); err != nil {
		t.Fatalf("enterprise resource: %v", err)
	}

	if calls != 1 || gotRemain != 750 || gotTotal != 1000 {
		t.Fatalf("observer calls=%d remain/total=%d/%d want 1/750/1000", calls, gotRemain, gotTotal)
	}
}
