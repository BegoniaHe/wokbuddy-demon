// checkin_result_test.go 钉住签到结果分类：inactive / already / ok / failed 四类分离，
// 非 *Error（网络层）不误判为 inactive/already（issue #27）。
package upstream

import (
	"errors"
	"testing"
)

func TestClassifyCheckin(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want CheckinResult
	}{
		{"nil", nil, CheckinOK},
		{"already-已签到", &Error{Kind: ErrNone, Status: 200, Msg: "今日已签到"}, CheckinAlready},
		{"already-english", &Error{Kind: ErrNone, Status: 200, Msg: "already checked in"}, CheckinAlready},
		{"inactive-未开启", &Error{Kind: ErrNone, Status: 200, Msg: "活动未开启"}, CheckinInactive},
		{"inactive-未开放", &Error{Kind: ErrNone, Status: 200, Msg: "签到活动未开放"}, CheckinInactive},
		{"inactive-已结束", &Error{Kind: ErrNone, Status: 200, Msg: "活动已结束"}, CheckinInactive},
		{"inactive-已过期", &Error{Kind: ErrNone, Status: 200, Msg: "活动已过期"}, CheckinInactive},
		{"failed-业务错误", &Error{Kind: ErrNone, Status: 200, Msg: "请求参数错误"}, CheckinFailed},
		{"failed-5xx", &Error{Kind: ErrNone, Status: 500, Msg: "internal error"}, CheckinFailed},
		{"failed-网络层", errors.New("connection reset"), CheckinFailed},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ClassifyCheckin(c.err); got != c.want {
				t.Errorf("ClassifyCheckin(%v)=%v want %v", c.err, got, c.want)
			}
		})
	}
}

// TestIsCheckinInactiveNonError 非 *Error 永不判 inactive（避免把网络抖动当活动未开启）。
func TestIsCheckinInactiveNonError(t *testing.T) {
	if IsCheckinInactive(errors.New("活动未开启")) {
		t.Error("plain error 不应判 inactive（只认 *Error.Msg）")
	}
}
