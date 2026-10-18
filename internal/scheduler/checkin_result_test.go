// checkin_result_test.go 钉住签到结果分类在调度侧的消费：活动未开启不标记已签、
// 不打原始错误码（issue #27）。
package scheduler

import (
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
	"github.com/BegoniaHe/wokbuddy-demon/internal/pool"
	"github.com/BegoniaHe/wokbuddy-demon/internal/upstream"
)

func TestCheckinOneInactiveNotMarkedDone(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/daily-checkin") {
			w.Write([]byte(`{"code":14001,"msg":"活动未开启"}`))
			return
		}

		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer srv.Close()

	state := pool.New("")
	account := &auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999}
	state.Add(account)

	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	scheduler := New(&Config{Pool: state, Upstream: up})

	var buf strings.Builder

	scheduler.logger = log.New(&buf, "", 0)

	scheduler.checkinOne("u1", "nick", account)

	st, _ := state.Status("u1")
	if st.CheckinDone {
		t.Error("活动未开启不应标记已签")
	}

	got := buf.String()
	if !strings.Contains(got, "活动未开启") {
		t.Errorf("日志应含分类文案，got %q", got)
	}

	if strings.Contains(got, "14001") {
		t.Errorf("活动未开启不应打原始错误码，got %q", got)
	}
}

// TestCheckinOneAlreadyMarkedDone 幂等拒绝仍标记已签（回归保护）。
func TestCheckinOneAlreadyMarkedDone(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"code":14001,"msg":"今日已签到"}`))
	}))
	defer srv.Close()

	state := pool.New("")
	account := &auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999}
	state.Add(account)

	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	scheduler := New(&Config{Pool: state, Upstream: up})

	scheduler.logger = log.New(io.Discard, "", 0)

	scheduler.checkinOne("u1", "nick", account)

	st, _ := state.Status("u1")
	if !st.CheckinDone {
		t.Error("今日已签到应标记已签")
	}
}
