package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/BegoniaHe/wokbuddy-demon/internal/apikey"
)

// panelWithKeys 构建一个仅启用密钥存储的面板（其余依赖 nil，密钥接口不触碰）。
func panelWithKeys(store *apikey.Store) *Panel {
	return New(Config{Version: "test", APIKey: "test-key", Keys: store})
}

func authed(method, path, body string) *http.Request {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-key")
	return req
}

type keysResp struct {
	Keys   []apikey.Record `json:"keys"`
	Key    string          `json:"key"`
	Record apikey.Record   `json:"record"`
}

// do 发起一个带默认密钥的面板请求，返回响应体与状态码。
func (p *Panel) doKeys(method, path, body string) (int, string) {
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, authed(method, path, body))
	return rec.Code, rec.Body.String()
}

// createKey 通过接口签发一把密钥并返回明文与记录。
func createKey(t *testing.T, p *Panel, name string) (string, apikey.Record) {
	t.Helper()
	code, body := p.doKeys(http.MethodPost, "/panel/api/keys", `{"name":"`+name+`"}`)
	if code != http.StatusOK {
		t.Fatalf("create code=%d body=%s", code, body)
	}
	var created keysResp
	if err := json.Unmarshal([]byte(body), &created); err != nil {
		t.Fatalf("decode create: %v", err)
	}
	if created.Key == "" || created.Record.ID == "" {
		t.Fatalf("create missing key/record: %s", body)
	}
	if strings.Contains(body, "hash") {
		t.Errorf("create response must not contain hash field: %s", body)
	}
	return created.Key, created.Record
}

// listKeys 拉取密钥列表。
func listKeys(t *testing.T, p *Panel) []apikey.Record {
	t.Helper()
	code, body := p.doKeys(http.MethodGet, "/panel/api/keys", "")
	if code != http.StatusOK {
		t.Fatalf("list code=%d", code)
	}
	var listed keysResp
	if err := json.Unmarshal([]byte(body), &listed); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	return listed.Keys
}

func TestKeysCreateAndList(t *testing.T) {
	p := panelWithKeys(apikey.New(""))
	raw, _ := createKey(t, p, "laptop")
	code, body := p.doKeys(http.MethodGet, "/panel/api/keys", "")
	if code != http.StatusOK {
		t.Fatalf("list code=%d", code)
	}
	if strings.Contains(body, raw) {
		t.Error("list must not expose plaintext")
	}
	if keys := listKeys(t, p); len(keys) != 1 || keys[0].Name != "laptop" {
		t.Fatalf("list = %+v", keys)
	}
}

func TestKeysRevoke(t *testing.T) {
	p := panelWithKeys(apikey.New(""))
	_, rec := createKey(t, p, "phone")
	if code, body := p.doKeys(http.MethodPost, "/panel/api/keys/"+rec.ID+"/revoke", ""); code != http.StatusOK {
		t.Fatalf("revoke code=%d body=%s", code, body)
	}
	keys := listKeys(t, p)
	if len(keys) != 1 || !keys[0].Revoked {
		t.Fatalf("after revoke: %+v", keys)
	}
}

func TestKeysDelete(t *testing.T) {
	p := panelWithKeys(apikey.New(""))
	_, rec := createKey(t, p, "temp")
	if code, body := p.doKeys(http.MethodPost, "/panel/api/keys/"+rec.ID+"/delete", ""); code != http.StatusOK {
		t.Fatalf("delete code=%d body=%s", code, body)
	}
	if keys := listKeys(t, p); len(keys) != 0 {
		t.Fatalf("after delete: %+v", keys)
	}
}

func TestKeysEndpointsNotImplementedWithoutStore(t *testing.T) {
	p := New(Config{Version: "test", APIKey: "test-key"})
	for _, c := range []struct{ method, path, body string }{
		{http.MethodGet, "/panel/api/keys", ""},
		{http.MethodPost, "/panel/api/keys", `{}`},
		{http.MethodPost, "/panel/api/keys/x/revoke", ""},
		{http.MethodPost, "/panel/api/keys/x/delete", ""},
	} {
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, authed(c.method, c.path, c.body))
		if rec.Code != http.StatusNotImplemented {
			t.Errorf("%s %s: code=%d, want 501", c.method, c.path, rec.Code)
		}
	}
}

// 命名密钥不能用于面板（面板仍只认默认 api_key）。
func TestNamedKeyCannotAccessPanel(t *testing.T) {
	store := apikey.New("")
	_, raw, _ := store.Create("client")
	p := panelWithKeys(store)
	req := httptest.NewRequest(http.MethodGet, "/panel/api/keys", nil)
	req.Header.Set("Authorization", "Bearer "+raw)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("named key on panel: code=%d, want 401", rec.Code)
	}
}
