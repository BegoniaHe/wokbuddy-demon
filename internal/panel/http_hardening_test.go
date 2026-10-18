package panel

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPanelAPIResponsesAreNotCached(t *testing.T) {
	p := New(Config{Version: "test", APIKey: "secret"})
	for _, path := range []string{"/panel/api/audit", "/panel/api/config"} {
		method := http.MethodGet
		if strings.HasSuffix(path, "/config") {
			method = http.MethodPost
		}
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
		cc := rec.Header().Get("Cache-Control")
		if !strings.Contains(cc, "no-store") {
			t.Errorf("%s %s Cache-Control=%q want no-store", method, path, cc)
		}
		if rec.Header().Get("Pragma") != "no-cache" {
			t.Errorf("%s %s missing Pragma: no-cache", method, path)
		}
	}
}

func TestLimitPanelBodyRejectsOversize(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/panel/api/config",
		bytes.NewReader(make([]byte, panelAPIMaxBodyBytes+1)))
	limitPanelBody(httptest.NewRecorder(), req)
	if _, err := io.ReadAll(req.Body); err == nil {
		t.Fatal("oversize body should be rejected")
	}
}

func TestLimitPanelBodyAllowsNormal(t *testing.T) {
	payload := []byte(`{"ok":true}`)
	req := httptest.NewRequest(http.MethodPost, "/panel/api/config", bytes.NewReader(payload))
	limitPanelBody(httptest.NewRecorder(), req)
	got, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("body=%q want %q", got, payload)
	}
}
