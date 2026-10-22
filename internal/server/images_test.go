package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
	"github.com/BegoniaHe/wokbuddy-demon/internal/livecfg"
	"github.com/BegoniaHe/wokbuddy-demon/internal/pool"
	"github.com/BegoniaHe/wokbuddy-demon/internal/upstream"
)

func jsonResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func TestImagesDisabledReturns404(t *testing.T) {
	t.Parallel()

	handler := NewHandler(&Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: newFakeUpstream(t, func(string) (int, string, bool) { return 200, `{}`, false }),
		Live:     livecfg.New(livecfg.Snapshot{ImagesEndpoint: false}),
	})

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/images/generations",
		strings.NewReader(`{"model":"text-to-image","prompt":"cat"}`)))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
}

func TestImagesPassthroughAndUsage(t *testing.T) {
	t.Parallel()

	var gotPath, gotAccept string

	var gotBody []byte

	client := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			gotPath = r.URL.Path
			gotAccept = r.Header.Get("Accept")
			gotBody, _ = io.ReadAll(r.Body)

			return jsonResponse(http.StatusOK,
				`{"created":1,"data":[{"url":"https://img.example/a.png"}],"usage":{"prompt_tokens":3,"completion_tokens":4,"total_tokens":7}}`), nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}

	handler := NewHandler(&Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: client,
		Live:     livecfg.New(livecfg.Snapshot{ImagesEndpoint: true}),
	})

	body := `{"model":"text-to-image","prompt":"cat","n":1}`
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(body)))

	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}

	if gotPath != "/v2/images/generations" {
		t.Fatalf("path=%s", gotPath)
	}

	if gotAccept != "application/json" {
		t.Fatalf("accept=%q", gotAccept)
	}

	if string(gotBody) != body {
		t.Fatalf("body must be passed through untouched: %s", gotBody)
	}

	var parsed map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &parsed); err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(rec.Body.String(), `"url":"https://img.example/a.png"`) {
		t.Fatalf("response not passed through: %s", rec.Body)
	}
}

func TestImagesRealmDirectNoFallback(t *testing.T) {
	t.Parallel()

	client := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return jsonResponse(http.StatusOK, `{}`), nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}

	// 只有 global 号：裸模型默认 cn，必须 fail-fast 而不是回落到 global。
	state := pool.New("")

	global := &auth.Auth{UID: "g1", AccessToken: "at-global", ExpiresAt: 9999999999}
	if _, err := auth.BackfillRealmFor(global, "global"); err != nil {
		t.Fatal(err)
	}

	state.Add(global)

	handler := NewHandler(&Config{Pool: state, Upstream: client, Live: livecfg.New(livecfg.Snapshot{ImagesEndpoint: true})})

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/images/generations",
		strings.NewReader(`{"model":"text-to-image","prompt":"cat"}`)))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
}

// TestImagesRealmPrefixStripped 带 realm 前缀的模型只改 body.model，按前缀直路由。
func TestImagesRealmPrefixStripped(t *testing.T) {
	t.Parallel()

	var gotBody []byte

	client := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			gotBody, _ = io.ReadAll(r.Body)

			return jsonResponse(http.StatusOK, `{"created":1,"data":[]}`), nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}

	handler := NewHandler(&Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: client,
		Live:     livecfg.New(livecfg.Snapshot{ImagesEndpoint: true}),
	})

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/images/generations",
		strings.NewReader(`{"model":"cn:text-to-image","prompt":"cat"}`)))

	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}

	if !strings.Contains(string(gotBody), `"model":"text-to-image"`) || strings.Contains(string(gotBody), "cn:") {
		t.Fatalf("body=%s", gotBody)
	}
}
