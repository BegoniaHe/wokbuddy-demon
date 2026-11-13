package upstream

import (
	"strings"
	"testing"
)

func TestRedactForLogRemovesSensitiveValues(t *testing.T) {
	t.Parallel()

	longToken := "QWxhZGRpbjpvcGVuIHNlc2FtZQ1234567890AYWJj"
	in := `{"error":"bad request","authorization":"Bearer sk-abc123supersecret",` +
		`"access_token":"tok-1234567890","url":"https://api.example.com/x?token=querysecret&key=k2",` +
		`"raw":"` + longToken + `"}`

	out := RedactForLog(in, 0)
	for _, secret := range []string{"sk-abc123supersecret", "tok-1234567890", "querysecret", "k2", longToken} {
		if strings.Contains(out, secret) {
			t.Errorf("secret %q leaked: %s", secret, out)
		}
	}

	for _, key := range []string{"authorization", "access_token", "url", "bad request"} {
		if !strings.Contains(out, key) {
			t.Errorf("diagnostic %q dropped: %s", key, out)
		}
	}
}

func TestRedactForLogSingleLine(t *testing.T) {
	t.Parallel()

	out := RedactForLog("line1\nline2\r\nline3\ttab", 0)
	for _, bad := range []string{"\n", "\r", "\t"} {
		if strings.Contains(out, bad) {
			t.Errorf("control char %q survived: %q", bad, out)
		}
	}
}

func TestRedactForLogBounded(t *testing.T) {
	t.Parallel()

	out := RedactForLog(strings.Repeat("a", 500), 50)
	if len(out) != 50 {
		t.Errorf("len=%d want 50", len(out))
	}
}

func TestRedactForLogKeepsBenignLongNames(t *testing.T) {
	t.Parallel()

	model := "claude-sonnet-4-5-20250929-very-long-model-name"

	out := RedactForLog(`{"error":"model not found","model":"`+model+`"}`, 0)
	if !strings.Contains(out, model) {
		t.Errorf("benign model name redacted: %s", out)
	}
}

func TestRedactForLogBareUUIDAndHex(t *testing.T) {
	t.Parallel()

	in := "id=550e8400-e29b-41d4-a716-446655440000 hex=deadbeefdeadbeefdeadbeefdeadbeef"

	out := RedactForLog(in, 0)
	if strings.Contains(out, "550e8400") || strings.Contains(out, "deadbeef") {
		t.Fatalf("bare token leaked: %s", out)
	}
}

func TestRedactForLogBasicAndJSONAuthorization(t *testing.T) {
	t.Parallel()

	in := `{"authorization":"Basic dXNlcjpwYXNz"} header=Authorization: Basic dXNlcjpwYXNz`

	out := RedactForLog(in, 0)
	if strings.Contains(out, "dXNlcjpwYXNz") {
		t.Fatalf("basic credential leaked: %s", out)
	}
}

// TestErrorStringRedactsSecrets 上游 *Error 的 Error() 是日志与面板回显出口：
// Msg 保留原文供分类，但字符串形态必须先脱敏。
func TestErrorStringRedactsSecrets(t *testing.T) {
	t.Parallel()

	err := &Error{
		Kind:   ErrClient,
		Status: 400,
		Msg:    `{"code":400,"msg":"bad","access_token":"tok-1234567890"}`,
	}

	if got := err.Error(); strings.Contains(got, "tok-1234567890") {
		t.Fatalf("access token leaked into Error(): %s", got)
	}
	// 分类仍要看到原始关键词。
	if err.Msg == "" || strings.Contains(err.Msg, "[REDACTED]") {
		t.Fatalf("Msg 应保留原文供分类: %q", err.Msg)
	}
}
