package scheduler

import (
	"context"
	"encoding/json"
	"fmt"
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

	scheduler.makeupRecent(context.Background(), &auth.Auth{UID: "u1", AccessToken: "at"})

	mu.Lock()
	defer mu.Unlock()

	if len(used) != 1 || used[0] != cst.Day(yesterday) {
		t.Fatalf("used=%v want [%s]", used, cst.Day(yesterday))
	}
}

// makeupFixture 可变漏签状态：use 成功后从 heatmap 移除，模拟下一轮真实上游。
type makeupFixture struct {
	mu      sync.Mutex
	missed  map[string]bool
	used    []string
	balance int
}

func newMakeupFixture(t *testing.T, balance int, missed ...string) (*makeupFixture, *httptest.Server) {
	t.Helper()

	fixture := &makeupFixture{missed: map[string]bool{}, used: nil, balance: balance}
	for _, day := range missed {
		fixture.missed[day] = true
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		switch {
		case strings.HasSuffix(r.URL.Path, "/activity/growth/heatmap"):
			fixture.mu.Lock()

			cells := make([]string, 0, len(fixture.missed))
			for day := range fixture.missed {
				cells = append(cells, `{"date":"`+day+`","score":0}`)
			}

			fixture.mu.Unlock()

			_, _ = w.Write([]byte(`{"code":0,"data":{"cells":[` + strings.Join(cells, ",") + `]}}`))
		case strings.HasSuffix(r.URL.Path, "/activity/growth/makeup-cards/use"):
			var body struct {
				TargetDate string `json:"target_date"`
			}

			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}

			fixture.mu.Lock()

			delete(fixture.missed, body.TargetDate)
			fixture.used = append(fixture.used, body.TargetDate)
			fixture.balance--

			fixture.mu.Unlock()

			_, _ = w.Write([]byte(`{"code":0,"data":{}}`))
		case strings.HasSuffix(r.URL.Path, "/activity/growth/streak"):
			fixture.mu.Lock()

			balance := fixture.balance

			fixture.mu.Unlock()

			_, _ = fmt.Fprintf(w, `{"code":0,"data":{"makeup_cards":{"balance":%d}}}`, balance)
		default:
			http.NotFound(w, r)
		}
	}))

	return fixture, srv
}

func (f *makeupFixture) snapshotUsed() []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]string(nil), f.used...)
}

// TestMakeupRecentPerRoundCap 每轮上限：cap=1 分两轮继续、cap=0 全补、
// cap 大于漏签数全部补完；补签顺序仍由近及远。
// TestUseMakeupCardsCancelLeavesCorrectRemainder：取消时报告的是尚未处理的漏签天数
// （由近及远遍历，下标 0..i 共 i+1 天），不能按 len-i 误报。
func TestUseMakeupCardsCancelLeavesCorrectRemainder(t *testing.T) {
	t.Parallel()

	scheduler := New(&Config{
		Pool:     pool.New(""),
		Upstream: &upstream.Client{HTTP: http.DefaultClient},
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	used, remaining, left := scheduler.useMakeupCards(
		ctx, &auth.Auth{UID: "u1", AccessToken: "at"}, []string{"2026-10-01", "2026-10-02", "2026-10-03"}, 5, 0,
	)

	if used != 0 || remaining != 5 {
		t.Fatalf("used=%d remaining=%d want 0/5（取消后不得用卡）", used, remaining)
	}

	if left != 3 {
		t.Fatalf("left=%d want 3（三天全未处理）", left)
	}
}

func TestMakeupRecentPerRoundCap(t *testing.T) {
	t.Parallel()

	now := cst.Now()
	yesterday := now.AddDate(0, 0, -1)
	dayBefore := now.AddDate(0, 0, -2)

	if dayBefore.Month() != now.Month() {
		t.Skip("month boundary: dynamic fixture would cross month")
	}

	days := []string{cst.Day(dayBefore), cst.Day(yesterday)}

	newScheduler := func(srv *httptest.Server) *Scheduler {
		client := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}

		return New(&Config{Pool: pool.New(""), Upstream: client})
	}

	t.Run("cap=1 分两轮", func(t *testing.T) {
		t.Parallel()

		fixture, srv := newMakeupFixture(t, 5, days...)
		defer srv.Close()

		scheduler := newScheduler(srv)
		scheduler.SetMakeupCardsPerRound(1)
		scheduler.makeupRecent(context.Background(), &auth.Auth{UID: "u1", AccessToken: "at"})

		if got := fixture.snapshotUsed(); len(got) != 1 || got[0] != days[1] {
			t.Fatalf("第一轮 used=%v want [%s]", got, days[1])
		}

		next := newScheduler(srv)
		next.SetMakeupCardsPerRound(1)
		next.makeupRecent(context.Background(), &auth.Auth{UID: "u1", AccessToken: "at"})

		if got := fixture.snapshotUsed(); len(got) != 2 || got[1] != days[0] {
			t.Fatalf("第二轮 used=%v want [%s %s]", got, days[1], days[0])
		}
	})

	t.Run("cap=0 全补", func(t *testing.T) {
		t.Parallel()

		fixture, srv := newMakeupFixture(t, 5, days...)
		defer srv.Close()

		scheduler := newScheduler(srv)
		scheduler.SetMakeupCardsPerRound(0)
		scheduler.makeupRecent(context.Background(), &auth.Auth{UID: "u1", AccessToken: "at"})

		if got := fixture.snapshotUsed(); len(got) != 2 || got[0] != days[1] || got[1] != days[0] {
			t.Fatalf("used=%v want [%s %s]", got, days[1], days[0])
		}
	})

	t.Run("cap 大于漏签数全补", func(t *testing.T) {
		t.Parallel()

		fixture, srv := newMakeupFixture(t, 5, days...)
		defer srv.Close()

		scheduler := newScheduler(srv)
		scheduler.SetMakeupCardsPerRound(10)
		scheduler.makeupRecent(context.Background(), &auth.Auth{UID: "u1", AccessToken: "at"})

		if got := fixture.snapshotUsed(); len(got) != 2 {
			t.Fatalf("used=%v want 2 天全补", got)
		}
	})
}
