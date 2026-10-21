package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
	"github.com/BegoniaHe/wokbuddy-demon/internal/credithist"
	"github.com/BegoniaHe/wokbuddy-demon/internal/pool"
)

// TestCreditHistoryHandler 账本接口：limit 钳制、uid 过滤、昵称从池快照 join。
func TestCreditHistoryHandler(t *testing.T) {
	t.Parallel()

	ledger := credithist.Open("", 100)
	ledger.Observe("u1", 100, "")
	ledger.Observe("u1", 90, "checkin")
	ledger.Observe("u2", 50, "")
	ledger.Observe("u2", 60, "task")

	state := pool.New("")
	state.Add(&auth.Auth{UID: "u1", AccessToken: "at-1", Nickname: "主力号"})
	state.Add(&auth.Auth{UID: "u2", AccessToken: "at-2", Nickname: "备用号"})

	panel := New(&Config{Pool: state, CreditHistory: ledger})

	do := func(query string) []map[string]any {
		rec := httptest.NewRecorder()
		panel.ServeHTTP(rec, authed(http.MethodGet, "/panel/api/credit_history"+query, ""))

		if rec.Code != http.StatusOK {
			t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
		}

		var resp struct {
			Entries []map[string]any `json:"entries"`
		}

		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}

		return resp.Entries
	}

	all := do("")
	if len(all) != 2 {
		t.Fatalf("entries=%d want 2", len(all))
	}

	for _, entry := range all {
		uid, _ := entry["uid"].(string)
		nickname, _ := entry["nickname"].(string)

		want := map[string]string{"u1": "主力号", "u2": "备用号"}[uid]
		if nickname != want {
			t.Errorf("uid=%s nickname=%q want %q", uid, nickname, want)
		}
	}

	if got := do("?uid=u1"); len(got) != 1 {
		t.Errorf("uid 过滤 entries=%d want 1", len(got))
	}

	// limit 钳制：请求 99999 也只回既有条数，不报错。
	if got := do("?limit=99999"); len(got) != 2 {
		t.Errorf("limit 钳制 entries=%d want 2", len(got))
	}
}

// TestCreditHistoryHandlerDisabled 账本未注入 → 501（不 panic）。
func TestCreditHistoryHandlerDisabled(t *testing.T) {
	t.Parallel()

	panel := New(&Config{Pool: pool.New("")})

	rec := httptest.NewRecorder()
	panel.ServeHTTP(rec, authed(http.MethodGet, "/panel/api/credit_history", ""))

	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("code=%d want 501", rec.Code)
	}
}
