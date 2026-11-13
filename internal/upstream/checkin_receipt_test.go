// checkin_receipt_test.go 钉住签到回执形态（issue #511）：成功平铺回执带积分、
// 重复签到的 400 信封归 already、真失败文案不被「已签到」子串判成幂等。
package upstream

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
)

func checkinReceiptFromBody(status int, body string) CheckinReceipt {
	return checkinReceipt(status, []byte(body))
}

func TestCheckinReceiptSuccessFlat(t *testing.T) {
	t.Parallel()

	got := checkinReceiptFromBody(200, `{"credit":100,"streak_days":7,"is_streak_day":true}`)
	if got.State != CheckinStateChecked {
		t.Fatalf("state=%q want checked", got.State)
	}

	if !got.CreditKnown || got.Credit != 100 {
		t.Errorf("credit=%d known=%v want 100/true", got.Credit, got.CreditKnown)
	}

	if got.StreakDays != 7 || !got.IsStreakDay {
		t.Errorf("streak=%d/%v want 7/true", got.StreakDays, got.IsStreakDay)
	}
}

// TestCheckinReceiptSuccessWithoutCredit 回执缺 credit 时不得编造数字。
func TestCheckinReceiptSuccessWithoutCredit(t *testing.T) {
	t.Parallel()

	got := checkinReceiptFromBody(200, `{"streak_days":3}`)
	if got.State != CheckinStateChecked {
		t.Fatalf("state=%q want checked", got.State)
	}

	if got.CreditKnown {
		t.Errorf("credit=%d 不应标记 known", got.Credit)
	}
}

// TestCheckinReceiptAlreadyEnvelope 重复签到的 400 信封：already + 干净文案，
// 不带 requestId、不带原始 JSON。
func TestCheckinReceiptAlreadyEnvelope(t *testing.T) {
	t.Parallel()

	got := checkinReceiptFromBody(400,
		`{"code":10001,"msg":"今天已签到，请明天再来","requestId":"req-123"}`)
	if got.State != CheckinStateAlready {
		t.Fatalf("state=%q want already", got.State)
	}

	if strings.ContainsAny(got.Message, `{}`) || strings.Contains(got.Message, "requestId") ||
		strings.Contains(got.Message, "req-123") {
		t.Errorf("message=%q 不应回显上游原始信封", got.Message)
	}

	if got.CreditKnown {
		t.Error("幂等回执不应带积分")
	}
}

// TestCheckinReceiptInactiveCodes 官方四态表：1002/1003 是活动态非错误。
func TestCheckinReceiptInactiveCodes(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		body string
	}{
		{"not-eligible", `{"code":1002,"msg":"not eligible"}`},
		{"event-ended", `{"code":1003,"msg":"event ended"}`},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()

			if got := checkinReceiptFromBody(200, c.body); got.State != CheckinStateInactive {
				t.Errorf("state=%q want inactive", got.State)
			}
		})
	}
}

// TestCheckinReceiptSuccessTextSaysAlready code=0 但文案是「今天已签到」：按幂等处理，
// 不能因为成功码就报成新签到。
func TestCheckinReceiptSuccessTextSaysAlready(t *testing.T) {
	t.Parallel()

	got := checkinReceiptFromBody(200, `{"code":0,"msg":"今天已签到"}`)
	if got.State != CheckinStateAlready {
		t.Fatalf("state=%q want already", got.State)
	}
}

// TestCheckinReceiptFailureWithAlreadySubstring 真失败文案含「已签到」子串，
// 必须判 failed（issue #511 的核心误判）。
func TestCheckinReceiptFailureWithAlreadySubstring(t *testing.T) {
	t.Parallel()

	got := checkinReceiptFromBody(400, `{"code":11101,"msg":"签到失败，已签到天数不足 7 天"}`)
	if got.State != CheckinStateFailed {
		t.Fatalf("state=%q want failed", got.State)
	}

	if got.Message != "签到失败，已签到天数不足 7 天" {
		t.Errorf("message=%q want 上游原文（无信封）", got.Message)
	}
}

// TestCheckinReceiptServerErrorFallsBack 5xx 无法读到回执：failed 且不带原始 body。
func TestCheckinReceiptServerErrorFallsBack(t *testing.T) {
	t.Parallel()

	got := checkinReceiptFromBody(500, `{"code":10000,"msg":"API request failed"}`)
	if got.State != CheckinStateFailed {
		t.Fatalf("state=%q want failed", got.State)
	}
}

// TestCheckinReceiptUnparsableSuccessBody 2xx 但回执读不出来（空 body / WAF 或反代
// 的 HTML 页 / 半截 JSON / null）：必须判失败，不能当成新签到——否则会标记当日
// 已签、面板报「签到成功」，账号实际没签（issue #511）。
func TestCheckinReceiptUnparsableSuccessBody(t *testing.T) {
	t.Parallel()

	for _, body := range []string{"", "<html><body>blocked</body></html>", `{"credit":100`, "null", "[]"} {
		got := checkinReceiptFromBody(200, body)
		if got.State != CheckinStateFailed {
			t.Errorf("body=%q state=%q want failed", body, got.State)
		}

		if got.Message == "" {
			t.Errorf("body=%q 失败回执应带可读文案", body)
		}

		if got.CreditKnown {
			t.Errorf("body=%q 不应带积分", body)
		}
	}
}

// TestClassifyCheckinFailureBeatsAlready 分类序：失败硬词压过 already 关键词。
func TestClassifyCheckinFailureBeatsAlready(t *testing.T) {
	t.Parallel()

	msg := "签到失败，已签到天数不足 7 天"
	if got := ClassifyCheckin(&Error{Kind: ErrClient, Status: 400, Msg: msg}); got != CheckinFailed {
		t.Errorf("ClassifyCheckin(%q)=%v want failed", msg, got)
	}

	if got := ClassifyCheckin(&Error{Kind: ErrClient, Status: 400, Msg: `{"code":1001}`}); got != CheckinAlready {
		t.Errorf("code=1001 应判 already, got %v", got)
	}

	if got := ClassifyCheckin(&Error{Kind: ErrClient, Status: 400, Msg: `{"code":10001}`}); got != CheckinAlready {
		t.Errorf("code=10001 应判 already, got %v", got)
	}
}

// TestDailyCheckinReceipt429IsTransient 429「请求处理中」是瞬时态（issue #250）：
// 回执必须带 transient error，调度器才会把账号排进 10m/40m 补签；否则当天直接漏签。
func TestDailyCheckinReceipt429IsTransient(t *testing.T) {
	t.Parallel()

	up := testClient(func(*http.Request) (*http.Response, error) {
		return jsonResp(http.StatusTooManyRequests, `{"code":6004,"msg":"请求处理中，请稍后再试"}`), nil
	})
	up.RetryDelay = time.Millisecond

	got, err := up.DailyCheckinReceipt(context.Background(), &auth.Auth{UID: "u1", AccessToken: "at"})
	if err == nil {
		t.Fatalf("429 应带 transient error，receipt=%+v", got)
	}

	if !IsTransientBillingErr(err) {
		t.Errorf("429 应判瞬时（可补签）: %v", err)
	}

	if got.State != CheckinStateFailed {
		t.Errorf("state=%q want failed", got.State)
	}
}

// TestDailyCheckinReceiptReadsFlatReceipt 端到端：签到成功回执被读出，而不是丢弃。
func TestDailyCheckinReceiptReadsFlatReceipt(t *testing.T) {
	t.Parallel()

	up := testClient(func(*http.Request) (*http.Response, error) {
		return jsonResp(200, `{"credit":150,"streak_days":2,"is_streak_day":false}`), nil
	})

	got, err := up.DailyCheckinReceipt(context.Background(), &auth.Auth{UID: "u1", AccessToken: "at"})
	if err != nil {
		t.Fatalf("err=%v", err)
	}

	if got.State != CheckinStateChecked || !got.CreditKnown || got.Credit != 150 {
		t.Fatalf("receipt=%+v want checked/credit 150", got)
	}
}
