package upstream

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
)

// TestChatOutboundHeadersBrandFree 用 httptest 捕获 chat 出站请求：头族无品牌
// 字面量，会话 ID 为 UUIDv4，消息/请求 ID 为 UUIDv7 32 hex。
func TestChatOutboundHeadersBrandFree(t *testing.T) {
	t.Parallel()

	var (
		gotHeader http.Header
		gotBody   []byte
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeader = r.Header.Clone()
		gotBody, _ = io.ReadAll(r.Body)

		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	client := New()
	client.ChatBaseCN = srv.URL
	account := &auth.Auth{UID: "u-chat", AccessToken: "at"}

	rc, status, _, err := client.ChatStreamContext(context.Background(), account,
		[]byte(`{"model":"fast-model","messages":[{"role":"user","content":"hi"}]}`), "", ChatMeta{})
	if err != nil || status != http.StatusOK {
		t.Fatalf("chat: status=%d err=%v", status, err)
	}

	_, _ = io.Copy(io.Discard, rc)
	_ = rc.Close()

	if gotHeader == nil {
		t.Fatal("未捕获到 chat 请求头")
	}

	if strings.Contains(strings.ToLower(fmt.Sprint(gotHeader)), "wb2a") ||
		strings.Contains(strings.ToLower(string(gotBody)), "wb2a") {
		t.Fatalf("chat 出站含品牌字面量: headers=%v body=%s", gotHeader, gotBody)
	}

	if convID := gotHeader.Get("X-Conversation-Id"); !uuidV4HeaderPattern.MatchString(convID) {
		t.Errorf("X-Conversation-Id=%q want UUIDv4", convID)
	}

	for _, name := range []string{"X-Conversation-Request-Id", "X-Conversation-Message-Id", "X-Request-ID"} {
		if got := gotHeader.Get(name); !uuidV7HeaderPattern.MatchString(got) {
			t.Errorf("%s=%q want UUIDv7 32 hex", name, got)
		}
	}
}

// TestMPReportOutboundIDsBrandFree mp 上报 body 无品牌字面量，conversationId 为
// UUIDv4，requestId/traceId/rootRequestId/messageId 为 UUIDv7 32 hex。
func TestMPReportOutboundIDsBrandFree(t *testing.T) {
	t.Parallel()

	var gotRaw []byte

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotRaw, _ = io.ReadAll(r.Body)

		w.Write([]byte(`{"code":0,"msg":"OK"}`))
	}))
	defer srv.Close()

	client := New()
	client.BillingBaseCN = srv.URL

	conv := "0f0e0d0c-0b0a-4908-8706-050403020100"
	if err := client.ReportMPEvent(&auth.Auth{AccessToken: "at", UID: "u-mp"}, SchoolChatTimesEvents(conv)); err != nil {
		t.Fatalf("ReportMPEvent: %v", err)
	}

	if strings.Contains(string(gotRaw), "wb2api") || strings.Contains(string(gotRaw), "wb2a") {
		t.Fatalf("mp report body 含品牌字面量: %s", gotRaw)
	}

	var events []map[string]any
	if err := json.Unmarshal(gotRaw, &events); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if len(events) != 1 {
		t.Fatalf("events=%d want 1", len(events))
	}

	event := events[0]
	if event["conversationId"] != conv {
		t.Errorf("conversationId=%v want %s", event["conversationId"], conv)
	}

	for _, key := range []string{"requestId", "traceId", "rootRequestId", "messageId"} {
		if got, _ := event[key].(string); !uuidV7HeaderPattern.MatchString(got) {
			t.Errorf("%s=%q want UUIDv7 32 hex", key, got)
		}
	}
}
