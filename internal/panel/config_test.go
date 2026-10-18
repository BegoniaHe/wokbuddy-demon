package panel

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRestoreConfigEndpoint(t *testing.T) {
	called := false
	p := New(Config{
		Version: "test",
		RestoreConfig: func() ([]string, error) {
			called = true
			return []string{"listen"}, nil
		},
	})
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/panel/api/config/restore", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if !called {
		t.Fatal("RestoreConfig not called")
	}
	if body := rec.Body.String(); body == "" || !containsAll(body, `"ok":true`, `"listen"`) {
		t.Fatalf("body=%s", body)
	}
}

func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			return false
		}
	}
	return true
}
