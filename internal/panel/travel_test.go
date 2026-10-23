package panel

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
	"github.com/BegoniaHe/wokbuddy-demon/internal/pool"
	"github.com/BegoniaHe/wokbuddy-demon/internal/scheduler"
)

// TestAccountTravelNotImplemented 无 scheduler 时 501（issue #257）。
func TestAccountTravelNotImplemented(t *testing.T) {
	t.Parallel()

	panel := New(&Config{Pool: pool.New("")})
	req := httptest.NewRequest(http.MethodPost, "/panel/api/accounts/u1/travel", strings.NewReader(""))
	req.SetPathValue("uid", "u1")

	rec := httptest.NewRecorder()

	panel.accountTravel(rec, req)

	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
}

// TestAccountTravelNotFound 账号不存在时 404，而不是静默 200。
func TestAccountTravelNotFound(t *testing.T) {
	t.Parallel()

	state := pool.New("")
	state.Add(&auth.Auth{UID: "u1", AccessToken: "at"})

	panel := New(&Config{Pool: state, Scheduler: scheduler.New(&scheduler.Config{Pool: state})})
	req := httptest.NewRequest(http.MethodPost, "/panel/api/accounts/nope/travel", strings.NewReader(""))
	req.SetPathValue("uid", "nope")

	rec := httptest.NewRecorder()

	panel.accountTravel(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
}
