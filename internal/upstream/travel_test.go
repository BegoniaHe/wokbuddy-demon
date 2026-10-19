package upstream

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"testing"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
)

var (
	errTravelPathFailed     = errors.New("travelPath failed")
	errMissingAuthorization = errors.New("missing Authorization")
	errMissingXUserID       = errors.New("missing X-User-Id")
	errWantGET              = errors.New("want GET")
	errWantPOST2            = errors.New("want POST")
	errBoom                 = errors.New("boom")
)

// travelPath 断言请求打到 growth 域的正确路径（/v2 优先，旧无前缀路径为 fallback）。
func travelPath(r *http.Request, want string) error {
	if r.URL.Path != want && r.URL.Path != "/v2"+want {
		return fmt.Errorf("%s: %w", "wrong path: "+r.URL.Path, errTravelPathFailed)
	}

	if r.Header.Get("Authorization") != "Bearer at" {
		return errMissingAuthorization
	}

	if r.Header.Get("X-User-Id") != "u1" {
		return errMissingXUserID
	}

	return nil
}

func TestTravelStatusParsesFields(t *testing.T) {
	t.Parallel()

	client := testClient(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodGet {
			return nil, errWantGET
		}

		if err := travelPath(r, "/activity/growth/buddy/travel/status"); err != nil {
			return nil, err
		}

		return jsonResp(200, `{"code":0,"msg":"ok","data":{"state":"arrived","daily_limit_reached":true,"record_id":42,"reward_credit":7}}`), nil
	})

	travelState, err := client.TravelStatus(&auth.Auth{AccessToken: "at", UID: "u1"})
	if err != nil {
		t.Fatalf("travel status: %v", err)
	}

	if travelState.State != "arrived" || !travelState.DailyLimitReached || travelState.RecordID != 42 || travelState.RewardCredit != 7 {
		t.Errorf("state=%+v", travelState)
	}
}

// TestTravelStatusBusinessError 上游 400 且 body 仍是 {code,msg,data} 信封时，
// 应被 doJSON 归一为 *Error（带 HTTP 状态码），而不是解析失败。
func TestTravelStatusBusinessError(t *testing.T) {
	t.Parallel()

	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(400, `{"code":400,"msg":"no active buddy","data":null}`), nil
	})
	_, err := c.TravelStatus(&auth.Auth{AccessToken: "at", UID: "u1"})

	var error *Error
	if !errors.As(err, &error) {
		t.Fatalf("want *Error, got %T %v", err, err)
	}

	if error.Status != 400 || error.Kind != ErrClient {
		t.Errorf("status=%d kind=%v", error.Status, error.Kind)
	}
}

func TestTravelDepartSendsLocationID(t *testing.T) {
	t.Parallel()

	var got []byte

	client := testClient(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodPost {
			return nil, errWantPOST2
		}

		if err := travelPath(r, "/activity/growth/buddy/travel/depart"); err != nil {
			return nil, err
		}

		got, _ = io.ReadAll(r.Body)

		return jsonResp(200, `{"code":0,"msg":"ok","data":{}}`), nil
	})
	if err := client.TravelDepart(&auth.Auth{AccessToken: "at", UID: "u1"}, 4); err != nil {
		t.Fatalf("depart: %v", err)
	}

	var body map[string]any
	if err := json.Unmarshal(got, &body); err != nil {
		t.Fatalf("depart body: %v (%s)", err, got)
	}

	if n, _ := body["location_id"].(float64); int(n) != 4 {
		t.Errorf("location_id=%v want 4 (body=%s)", body["location_id"], got)
	}
}

func TestTravelClaimReturnsReward(t *testing.T) {
	t.Parallel()

	var got []byte

	client := testClient(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodPost {
			return nil, errWantPOST2
		}

		if err := travelPath(r, "/activity/growth/buddy/travel/claim"); err != nil {
			return nil, err
		}

		got, _ = io.ReadAll(r.Body)

		return jsonResp(200, `{"code":0,"msg":"ok","data":{"reward_credit":9}}`), nil
	})

	reward, err := client.TravelClaim(&auth.Auth{AccessToken: "at", UID: "u1"}, 42)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}

	if reward != 9 {
		t.Errorf("reward=%d want 9", reward)
	}

	if !bytes.Contains(got, []byte(`"record_id":42`)) {
		t.Errorf("claim body missing record_id: %s", got)
	}
}

// TestBuddyInfoNullMeansNoBuddy data.buddy 为 null → 返回 nil 表示无猫。
func TestBuddyInfoNullMeansNoBuddy(t *testing.T) {
	t.Parallel()

	client := testClient(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodGet {
			return nil, errWantGET
		}

		if err := travelPath(r, "/activity/growth/buddy/info"); err != nil {
			return nil, err
		}

		return jsonResp(200, `{"code":0,"msg":"ok","data":{"buddy":null}}`), nil
	})

	buddy, err := client.BuddyInfo(&auth.Auth{AccessToken: "at", UID: "u1"})
	if err != nil {
		t.Fatalf("buddy info: %v", err)
	}

	if buddy != nil {
		t.Errorf("buddy=%+v want nil (无猫)", buddy)
	}
}

// TestBuddyInfoEmptyObjectMeansNoBuddy data.buddy 为空对象时同样视为无猫
// （id=0 不是合法档案），避免调度器把它当成有猫继续查询/派出。
func TestBuddyInfoEmptyObjectMeansNoBuddy(t *testing.T) {
	t.Parallel()

	client := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(200, `{"code":0,"msg":"ok","data":{"buddy":{}}}`), nil
	})

	buddy, err := client.BuddyInfo(&auth.Auth{AccessToken: "at", UID: "u1"})
	if err != nil {
		t.Fatalf("buddy info: %v", err)
	}

	if buddy != nil {
		t.Errorf("buddy=%+v want nil (无猫)", buddy)
	}
}

func TestBuddyInfoPresent(t *testing.T) {
	t.Parallel()

	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(200, `{"code":0,"data":{"buddy":{"id":7,"name":"档案喵 R"}}}`), nil
	})

	buddy, err := c.BuddyInfo(&auth.Auth{AccessToken: "at", UID: "u1"})
	if err != nil || buddy == nil {
		t.Fatalf("buddy=%+v err=%v", buddy, err)
	}

	if buddy.Name != "档案喵 R" || buddy.ID != 7 {
		t.Errorf("buddy=%+v", buddy)
	}
}

func TestBuddyAgreementIdempotent(t *testing.T) {
	t.Parallel()

	var got []byte

	client := testClient(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodPost {
			return nil, errWantPOST2
		}

		if err := travelPath(r, "/activity/growth/buddy/agreement"); err != nil {
			return nil, err
		}

		got, _ = io.ReadAll(r.Body)

		return jsonResp(200, `{"code":0,"msg":"ok","data":{"agreed":true}}`), nil
	})
	if err := client.BuddyAgreement(&auth.Auth{AccessToken: "at", UID: "u1"}); err != nil {
		t.Fatalf("agreement: %v", err)
	}

	if !bytes.Contains(got, []byte(`"agree":true`)) {
		t.Errorf("agreement body=%s", got)
	}
}

func TestBuddyFirstPath(t *testing.T) {
	t.Parallel()

	var got []byte

	client := testClient(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodPost {
			return nil, errWantPOST2
		}

		if err := travelPath(r, "/activity/growth/buddy/first"); err != nil {
			return nil, err
		}

		got, _ = io.ReadAll(r.Body)

		return jsonResp(200, `{"code":0,"msg":"ok","data":{"buddy":{"id":1}}}`), nil
	})
	if err := client.BuddyFirst(&auth.Auth{AccessToken: "at", UID: "u1"}); err != nil {
		t.Fatalf("first: %v", err)
	}

	if string(bytes.TrimSpace(got)) != "{}" {
		t.Errorf("first body=%q want {}", got)
	}
}

// TestIsBuddyTaskIncomplete conversation 门槛未达标：HTTP 400 + first_buddy 关键词。
func TestIsBuddyTaskIncomplete(t *testing.T) {
	t.Parallel()

	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(400, `{"code":400,"msg":"first_buddy task not completed yet"}`), nil
	})

	err := c.BuddyFirst(&auth.Auth{AccessToken: "at", UID: "u1"})
	if err == nil {
		t.Fatal("want error")
	}

	if !IsBuddyTaskIncomplete(err) {
		t.Errorf("err=%v should be classified as 门槛未达标", err)
	}
}

func TestIsBuddyTaskIncompleteNegative(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"普通错误", errBoom, false},
		{"400 其他业务错误", &Error{Kind: ErrClient, Status: 400, Msg: "no active buddy"}, false},
		{"500 含关键词也不认", &Error{Kind: ErrServer, Status: 500, Msg: "first_buddy task not completed yet"}, false},
		{"401 会话失效", &Error{Kind: ErrSessionDead, Status: 401, Msg: "Offline user session not found"}, false},
	}
	for _, c := range cases {
		if got := IsBuddyTaskIncomplete(c.err); got != c.want {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}

// TestTravel401ClassifiedSessionDead 401 交由调用方跳过本轮（巡检不强刷 token）。
func TestTravel401ClassifiedSessionDead(t *testing.T) {
	t.Parallel()

	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(401, `{"code":12153,"msg":"Offline user session not found"}`), nil
	})
	_, err := c.TravelStatus(&auth.Auth{AccessToken: "at", UID: "u1"})

	var ue *Error
	if !errors.As(err, &ue) || ue.Kind != ErrSessionDead {
		t.Fatalf("err=%v want ErrSessionDead", err)
	}
}

// TestTravelClaimEmptyBodyFallback 带 record_id 收到参数错误后，空 body 重试一次；
// 两种请求都携带 X-Product-Code。
func TestTravelClaimEmptyBodyFallback(t *testing.T) {
	t.Parallel()

	var calls int

	client := testClient(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("X-Product-Code") != "workbuddy" {
			t.Errorf("X-Product-Code=%q want workbuddy", r.Header.Get("X-Product-Code"))
		}

		calls++

		var body []byte
		if r.Body != nil {
			body, _ = io.ReadAll(r.Body)
		}

		if calls == 1 {
			if !bytes.Contains(body, []byte(`"record_id":42`)) {
				t.Errorf("first body=%s want record_id", body)
			}

			return jsonResp(400, `{"code":400,"msg":"parameter error"}`), nil
		}

		if len(body) != 0 {
			t.Errorf("fallback body=%s want empty", body)
		}

		return jsonResp(200, `{"code":0,"data":{"reward_credit":5}}`), nil
	})

	reward, err := client.TravelClaim(&auth.Auth{AccessToken: "at", UID: "u1"}, 42)
	if err != nil || reward != 5 {
		t.Fatalf("reward=%d err=%v", reward, err)
	}

	if calls != 2 {
		t.Errorf("calls=%d want 2", calls)
	}
}

// TestGrowthUsesV2First Bearer growth 通道 /v2 路径优先。
func TestGrowthUsesV2First(t *testing.T) {
	t.Parallel()

	var paths []string

	client := testClient(func(r *http.Request) (*http.Response, error) {
		paths = append(paths, r.URL.Path)
		if r.URL.Path == "/v2/activity/growth/streak" {
			return jsonResp(200, `{"code":0,"data":{"streak":{"days":3}}}`), nil
		}

		return jsonResp(404, `{"code":404,"msg":"not found"}`), nil
	})

	days, err := client.GrowthStreak(&auth.Auth{AccessToken: "at", UID: "u1"})
	if err != nil || days != 3 {
		t.Fatalf("days=%d err=%v", days, err)
	}

	if len(paths) != 1 || paths[0] != "/v2/activity/growth/streak" {
		t.Errorf("paths=%v want only /v2 first", paths)
	}
}

// TestTravelClaimReturnsLastError 空 body 重试失败时返回最后一次错误分类，
// 不被更早的参数错误覆盖。
func TestTravelClaimReturnsLastError(t *testing.T) {
	t.Parallel()

	var calls int

	client := testClient(func(r *http.Request) (*http.Response, error) {
		calls++
		if calls == 1 {
			return jsonResp(400, `{"code":400,"msg":"parameter error"}`), nil
		}

		return jsonResp(500, `{"code":500,"msg":"boom"}`), nil
	})

	_, err := client.TravelClaim(&auth.Auth{AccessToken: "at", UID: "u1"}, 42)

	var upstreamErr *Error
	if !errors.As(err, &upstreamErr) || upstreamErr.Kind != ErrServer {
		t.Errorf("err=%v want ErrServer (last attempt)", err)
	}

	if calls != 2 {
		t.Errorf("calls=%d want 2", calls)
	}
}
