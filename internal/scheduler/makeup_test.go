package scheduler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
	"github.com/BegoniaHe/wokbuddy-demon/internal/cst"
	"github.com/BegoniaHe/wokbuddy-demon/internal/pool"
	"github.com/BegoniaHe/wokbuddy-demon/internal/upstream"
)

// TestMakeupRecentNearToFar 本月漏签由近及远补、受补签卡余额限制（issue #250）。
func TestMakeupRecentNearToFar(t *testing.T) {
	t.Parallel()

	now := cst.Now()
	yesterday := now.AddDate(0, 0, -1)
	dayBefore := now.AddDate(0, 0, -2)

	if dayBefore.Month() != now.Month() {
		t.Skip("month boundary: dynamic fixture would cross month")
	}

	var mu sync.Mutex

	var used []string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		switch {
		case strings.HasSuffix(r.URL.Path, "/activity/growth/heatmap"):
			_, _ = w.Write([]byte(`{"code":0,"data":{"cells":[` +
				`{"date":"` + cst.Day(dayBefore) + `","score":0},` +
				`{"date":"` + cst.Day(yesterday) + `","score":0},` +
				`{"date":"` + cst.Day(now) + `","score":0}]}}`))
		case strings.HasSuffix(r.URL.Path, "/activity/growth/makeup-cards/use"):
			var body struct {
				TargetDate string `json:"target_date"`
			}

			_ = json.NewDecoder(r.Body).Decode(&body)

			mu.Lock()

			used = append(used, body.TargetDate)

			mu.Unlock()

			_, _ = w.Write([]byte(`{"code":0,"data":{}}`))
		case strings.HasSuffix(r.URL.Path, "/activity/growth/streak"):
			_, _ = w.Write([]byte(`{"code":0,"data":{"makeup_cards":{"balance":1}}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	state := pool.New("")

	state.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})

	client := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	scheduler := New(&Config{Pool: state, Upstream: client})

	scheduler.makeupRecent(&auth.Auth{UID: "u1", AccessToken: "at"})

	mu.Lock()
	defer mu.Unlock()

	if len(used) != 1 || used[0] != cst.Day(yesterday) {
		t.Fatalf("used=%v want [%s]", used, cst.Day(yesterday))
	}
}
