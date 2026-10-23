package upstream

import (
	"strings"
	"testing"
)

// TestDebugHeaderValueRedactsCredentials 凭证类请求头在调试输出里整段隐藏。
func TestDebugHeaderValueRedactsCredentials(t *testing.T) {
	t.Parallel()

	for _, name := range []string{
		"Authorization", "proxy-authorization", "Cookie", "Set-Cookie",
		"X-Api-Key", "x-goog-api-key", "X-Device-Token",
	} {
		if got := debugHeaderValue(name, "Bearer secret-token"); got != redacted {
			t.Errorf("debugHeaderValue(%q) = %q, want %q", name, got, redacted)
		}
	}
}

// TestDebugHeaderValueKeepsDiagnostics 非敏感头保留会话 ID，同时压平控制字符。
func TestDebugHeaderValueKeepsDiagnostics(t *testing.T) {
	t.Parallel()

	const conversationID = "550e8400-e29b-41d4-a716-446655440000"
	if got := debugHeaderValue("X-Conversation-Id", conversationID); got != conversationID {
		t.Errorf("conversation id = %q, want %q", got, conversationID)
	}

	got := debugHeaderValue("User-Agent", "WorkBuddy/5.5.6\r\nforged: entry")
	for _, bad := range []string{"\r", "\n"} {
		if strings.Contains(got, bad) {
			t.Errorf("control char %q survived: %q", bad, got)
		}
	}
}
