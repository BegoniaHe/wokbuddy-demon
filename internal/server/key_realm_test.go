package server

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/BegoniaHe/wokbuddy-demon/internal/apikey"
	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
)

// TestKeyRealmBindingRoutes 命名密钥绑定 global 后，裸模型请求只走 global 账号。
func TestKeyRealmBindingRoutes(t *testing.T) {
	t.Parallel()

	store := apikey.New("")

	rec, raw, err := store.Create("global-client")
	if err != nil {
		t.Fatal(err)
	}

	if ok, err := store.SetRealm(rec.ID, "global"); !ok || err != nil {
		t.Fatalf("set realm: ok=%v err=%v", ok, err)
	}

	var seen string

	client := newFakeUpstream(t, func(authz string) (int, string, bool) {
		seen = authz
		return 200, sseOK, true
	})
	p := testPoolWith(
		&auth.Auth{UID: "cn1", AccessToken: "cn-token", ExpiresAt: 9999999999},
		&auth.Auth{UID: "g1", AccessToken: "global-token", Domain: "workbuddy.ai", ExpiresAt: 9999999999},
	)
	handler := NewHandler(&Config{Pool: p, Upstream: client, Keys: store})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		bytes.NewReader([]byte(`{"model":"glm-5.2","messages":[]}`)))
	req.Header.Set("Authorization", "Bearer "+raw)

	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, req)

	if rec2.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec2.Code, rec2.Body)
	}

	if !strings.Contains(seen, "global-token") {
		t.Errorf("upstream auth=%q want global account", seen)
	}
}

// TestKeyRealmConflictRejected 绑定与显式前缀冲突时 400，且不打上游。
func TestKeyRealmConflictRejected(t *testing.T) {
	t.Parallel()

	store := apikey.New("")

	rec, raw, err := store.Create("global-client")
	if err != nil {
		t.Fatal(err)
	}

	if ok, err := store.SetRealm(rec.ID, "global"); !ok || err != nil {
		t.Fatalf("set realm: ok=%v err=%v", ok, err)
	}

	var calls int

	up := newFakeUpstream(t, func(string) (int, string, bool) {
		calls++
		return 200, sseOK, true
	})
	p := testPoolWith(&auth.Auth{UID: "cn1", AccessToken: "cn-token", ExpiresAt: 9999999999})
	handler := NewHandler(&Config{Pool: p, Upstream: up, Keys: store})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		bytes.NewReader([]byte(`{"model":"cn:glm-5.2","messages":[]}`)))
	req.Header.Set("Authorization", "Bearer "+raw)

	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, req)

	if rec2.Code != http.StatusBadRequest {
		t.Fatalf("code=%d body=%s want 400", rec2.Code, rec2.Body)
	}

	if !strings.Contains(rec2.Body.String(), "realm_binding_conflict") {
		t.Errorf("missing conflict code: %s", rec2.Body)
	}

	if calls != 0 {
		t.Errorf("upstream calls=%d want 0", calls)
	}
}

// TestKeyRealmUnboundFollowsModel 未绑定 key 保持既有前缀语义。
func TestKeyRealmUnboundFollowsModel(t *testing.T) {
	t.Parallel()

	store := apikey.New("")

	_, raw, err := store.Create("plain")
	if err != nil {
		t.Fatal(err)
	}

	var seen string

	client := newFakeUpstream(t, func(authz string) (int, string, bool) {
		seen = authz
		return 200, sseOK, true
	})
	p := testPoolWith(
		&auth.Auth{UID: "cn1", AccessToken: "cn-token", ExpiresAt: 9999999999},
		&auth.Auth{UID: "g1", AccessToken: "global-token", Domain: "workbuddy.ai", ExpiresAt: 9999999999},
	)
	handler := NewHandler(&Config{Pool: p, Upstream: client, Keys: store})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		bytes.NewReader([]byte(`{"model":"cn:glm-5.2","messages":[]}`)))
	req.Header.Set("Authorization", "Bearer "+raw)

	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, req)

	if rec2.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec2.Code, rec2.Body)
	}

	if !strings.Contains(seen, "cn-token") {
		t.Errorf("upstream auth=%q want cn account", seen)
	}
}
