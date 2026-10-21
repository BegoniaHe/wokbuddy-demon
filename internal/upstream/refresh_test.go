package upstream

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
)

func TestRefreshTokenSingleFlight(t *testing.T) {
	t.Parallel()

	var hits atomic.Int64

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := hits.Add(1)

		time.Sleep(80 * time.Millisecond) // 拉宽并发窗口
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"code":0,"data":{"accessToken":"at-%d","refreshToken":"rt-%d","expiresIn":3600}}`, n, n)
	}))
	defer srv.Close()

	client := New()
	client.ChatBaseCN = srv.URL

	accounts := make([]*auth.Auth, 8)
	for i := range accounts {
		accounts[i] = &auth.Auth{UID: "u1", AccessToken: "old-at", RefreshToken: "old-rt", ExpiresAt: 1}
	}

	var wg sync.WaitGroup
	for _, a := range accounts {
		wg.Go(func() {
			if err := client.RefreshToken(a); err != nil {
				t.Errorf("refresh: %v", err)
			}
		})
	}

	wg.Wait()

	if got := hits.Load(); got != 1 {
		t.Fatalf("upstream refresh hits=%d want 1", got)
	}

	for i, a := range accounts {
		if a.RefreshTokenValue() != "rt-1" {
			t.Errorf("account %d refreshToken=%q want rt-1", i, a.RefreshTokenValue())
		}
	}
}

func TestRefreshTokenConcurrentLoserReusesSuccess(t *testing.T) {
	t.Parallel()

	var hits atomic.Int64

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := hits.Add(1)

		time.Sleep(100 * time.Millisecond)

		if n == 1 {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"code":0,"data":{"accessToken":"new-at","refreshToken":"new-rt","expiresIn":3600}}`))

			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"code":12153,"msg":"Offline user session not found"}`))
	}))
	defer srv.Close()

	client := New()
	client.ChatBaseCN = srv.URL

	accountA := &auth.Auth{UID: "u1", AccessToken: "old-at", RefreshToken: "old-rt", ExpiresAt: 1}
	accountB := &auth.Auth{UID: "u1", AccessToken: "old-at", RefreshToken: "old-rt", ExpiresAt: 1}

	errs := make([]error, 2)

	var wg sync.WaitGroup
	for i, a := range []*auth.Auth{accountA, accountB} {
		wg.Go(func() {
			errs[i] = client.RefreshToken(a)
		})

		time.Sleep(20 * time.Millisecond) // 确保第二个进入在途窗口
	}

	wg.Wait()

	if errs[0] != nil || errs[1] != nil {
		t.Fatalf("concurrent refresh should share success: %v / %v", errs[0], errs[1])
	}

	if got := hits.Load(); got != 1 {
		t.Fatalf("upstream refresh hits=%d want 1", got)
	}

	if accountB.RefreshTokenValue() != "new-rt" {
		t.Fatalf("waiter refreshToken=%q want new-rt", accountB.RefreshTokenValue())
	}
}
