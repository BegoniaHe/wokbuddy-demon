package panel

import (
	"bytes"
	"encoding/json"
	"errors"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
	"github.com/BegoniaHe/wokbuddy-demon/internal/pool"
	"github.com/BegoniaHe/wokbuddy-demon/internal/upstream"
)

func TestSecureRoundtrip(t *testing.T) {
	plain := []byte(`[{"uid":"u1","access_token":"at","refresh_token":"rt"}]`)
	env, err := encryptPayload("correct horse battery", plain)
	if err != nil {
		t.Fatal(err)
	}
	got, err := decryptPayload("correct horse battery", env)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if !bytes.Equal(got, plain) {
		t.Fatalf("roundtrip got=%s want=%s", got, plain)
	}
	if _, err := decryptPayload("wrong password", env); !errors.Is(err, errSecureDecrypt) {
		t.Fatalf("wrong password err=%v want errSecureDecrypt", err)
	}
}

func TestDecryptRejectsMalformed(t *testing.T) {
	cases := [][]byte{
		[]byte(`not json`),
		[]byte(`{"v":2,"kdf":"pbkdf2-sha256","iter":1000,"salt":"","nonce":"","ct":""}`),
		[]byte(`{"v":1,"kdf":"scrypt","iter":1000,"salt":"","nonce":"","ct":""}`),
		[]byte(`{"v":1,"kdf":"pbkdf2-sha256","iter":99999999,"salt":"","nonce":"","ct":""}`),
		[]byte(`{"v":1,"kdf":"pbkdf2-sha256","iter":1000,"salt":"!!!","nonce":"","ct":""}`),
	}
	for _, raw := range cases {
		if _, err := decryptPayload("pw", raw); !errors.Is(err, errSecureDecrypt) {
			t.Errorf("raw=%s err=%v want errSecureDecrypt", raw, err)
		}
	}
}

// fakeBillingServer 返回通用成功信封（供导入时的签到/余额调用）。
func fakeBillingServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"code":0,"msg":"ok","data":{}}`))
	}))
}

func newSecurePanel(t *testing.T) (*Panel, *pool.Pool, *httptest.Server) {
	t.Helper()
	srv := fakeBillingServer(t)
	t.Cleanup(srv.Close)
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	p := pool.New("")
	return &Panel{cfg: Config{Pool: p, Upstream: up, AuthDir: t.TempDir()}}, p, srv
}

func TestExportImportEncryptedEndToEnd(t *testing.T) {
	pn, p, _ := newSecurePanel(t)
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at-secret", RefreshToken: "rt-secret", ExpiresAt: 9999999999, Nickname: "甲"})

	// 导出。
	body, _ := json.Marshal(map[string]string{"password": "correct horse battery"})
	rec := httptest.NewRecorder()
	pn.exportEncrypted(rec, httptest.NewRequest(http.MethodPost, "/panel/api/export/encrypted", bytes.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("export code=%d body=%s", rec.Code, rec.Body)
	}
	envelope := rec.Body.Bytes()
	if bytes.Contains(envelope, []byte("at-secret")) {
		t.Fatal("导出文件不得含明文 token")
	}

	// 导入到一个新面板。
	pn2, p2, _ := newSecurePanel(t)
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("file", "export.wbenc.json")
	fw.Write(envelope)
	mw.WriteField("password", "correct horse battery")
	mw.Close()
	req := httptest.NewRequest(http.MethodPost, "/panel/api/import/encrypted", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec2 := httptest.NewRecorder()
	pn2.importEncrypted(rec2, req)
	if rec2.Code != http.StatusOK {
		t.Fatalf("import code=%d body=%s", rec2.Code, rec2.Body)
	}
	var res struct {
		Imported int `json:"imported"`
		Skipped  int `json:"skipped"`
	}
	json.Unmarshal(rec2.Body.Bytes(), &res)
	if res.Imported != 1 || res.Skipped != 0 {
		t.Fatalf("imported=%d skipped=%d want 1/0 body=%s", res.Imported, res.Skipped, rec2.Body)
	}
	if a := p2.AuthByUID("u1"); a == nil || a.AccessTokenValue() != "at-secret" {
		t.Fatalf("imported account missing or token mismatch: %+v", a)
	}
}

// TestImportEncryptedRejectsOversized 超过请求体上限的文件在解析前即被拒（issue #30 复审）。
func TestImportEncryptedRejectsOversized(t *testing.T) {
	pn, _, _ := newSecurePanel(t)
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("file", "big.json")
	fw.Write(bytes.Repeat([]byte("x"), secureMaxUploadBytes+1))
	mw.WriteField("password", "correct horse battery")
	mw.Close()
	req := httptest.NewRequest(http.MethodPost, "/panel/api/import/encrypted", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()
	pn.importEncrypted(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d want 400 body=%s", rec.Code, rec.Body)
	}
}

func TestImportEncryptedWrongPassword(t *testing.T) {
	pn, _, _ := newSecurePanel(t)
	env, _ := encryptPayload("right-password", []byte(`[]`))
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("file", "x.json")
	fw.Write(env)
	mw.WriteField("password", "wrong-password")
	mw.Close()
	req := httptest.NewRequest(http.MethodPost, "/panel/api/import/encrypted", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()
	pn.importEncrypted(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d want 400 body=%s", rec.Code, rec.Body)
	}
}
