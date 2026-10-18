package upstream

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
)

// TestReportDesktopEventFingerprint 断言桌面指纹上报：走 chatBase、UA 为桌面形状、
// 事件数组自动注入 workbuddy-desktop 公共指纹（覆盖同名业务键）。
func TestReportDesktopEventFingerprint(t *testing.T) {
	t.Parallel()

	var (
		got   []map[string]any
		gotUA string
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertReportDesktopRequest(t, r)
		gotUA = r.Header.Get("User-Agent")

		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatalf("body must be array: %v", err)
		}

		w.Write([]byte(`{"code":0,"msg":"OK"}`))
	}))
	defer srv.Close()

	c := &Client{HTTP: srv.Client(), ChatBaseCN: srv.URL}
	account := &auth.Auth{AccessToken: "at", UID: "u-dt", Nickname: "RenJie"}

	err := c.ReportDesktopEvent(account,
		DesktopEvent{"eventCode": "agent_task_created", "mode": "craft"})
	if err != nil {
		t.Fatalf("report desktop: %v", err)
	}

	if want := desktopUAFor(account); gotUA != want {
		t.Errorf("UA=%q want %q", gotUA, want)
	}

	assertDesktopEventFingerprint(t, got)
}

// assertReportDesktopRequest 断言桌面上报请求的路径与账号头。
func assertReportDesktopRequest(t *testing.T, r *http.Request) {
	t.Helper()

	if r.URL.Path != "/v2/report" {
		t.Errorf("path=%s want /v2/report", r.URL.Path)
	}

	if r.Header.Get("X-User-Id") != "u-dt" {
		t.Errorf("X-User-Id=%q want u-dt", r.Header.Get("X-User-Id"))
	}

	if r.Header.Get("X-Product") != "SaaS" {
		t.Errorf("X-Product=%q want SaaS", r.Header.Get("X-Product"))
	}
}

// assertDesktopEventFingerprint 断言上报事件被注入 workbuddy-desktop 公共指纹。
func assertDesktopEventFingerprint(t *testing.T, got []map[string]any) {
	t.Helper()

	if len(got) != 1 {
		t.Fatalf("events=%d want 1", len(got))
	}

	table := got[0]
	if table["extName"] != "workbuddy-desktop" {
		t.Errorf("extName=%v want workbuddy-desktop", table["extName"])
	}

	if table["ideType"] != "WorkBuddy" || table["ideName"] != "WorkBuddy" {
		t.Errorf("ideType/ideName=%v/%v want WorkBuddy", table["ideType"], table["ideName"])
	}

	if table["userId"] != "u-dt" {
		t.Errorf("userId=%v want u-dt", table["userId"])
	}

	if table["userNickname"] != "RenJie" {
		t.Errorf("userNickname=%v want RenJie", table["userNickname"])
	}

	if table["mode"] != "craft" {
		t.Errorf("业务字段 mode=%v 被指纹覆盖或缺失", table["mode"])
	}
	// machineId 应为 uid 稳定派生（36 hex）。
	if len(table["machineId"].(string)) != 36 {
		t.Errorf("machineId 长度=%d want 36", len(table["machineId"].(string)))
	}
}

// TestDesktopChatSequenceShape 断言完整对话事件链的事件码顺序与关键字段。
func TestDesktopChatSequenceShape(t *testing.T) {
	t.Parallel()

	events := DesktopChatSequence("conv-1", "req-1", "msg-1", "fast-model", "fast-model")

	wantCodes := []string{
		"agent_task_created", "chat_message_send", "chat_request_send",
		"chat_message_response", "chat_message_status", "chat_request_response",
	}
	if len(events) != len(wantCodes) {
		t.Fatalf("events=%d want %d", len(events), len(wantCodes))
	}

	for i, want := range wantCodes {
		if events[i]["eventCode"] != want {
			t.Errorf("events[%d].eventCode=%v want %v", i, events[i]["eventCode"], want)
		}
	}

	resp := events[3]
	if resp["isSuccessful"] != true {
		t.Errorf("chat_message_response.isSuccessful=%v want true（RichMeow 判据）", resp["isSuccessful"])
	}

	if resp["finishReason"] != "stop" {
		t.Errorf("finishReason=%v want stop", resp["finishReason"])
	}

	if resp["conversationId"] != "conv-1" {
		t.Errorf("conversationId=%v want conv-1", resp["conversationId"])
	}
}

// TestSetAppearanceTheme 断言主题设置端点形状（点亮 Hp_Appearance 的 API）。
func TestSetAppearanceTheme(t *testing.T) {
	t.Parallel()

	var got map[string]any

	account := &auth.Auth{AccessToken: "at", UID: "u1"}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/user-asset/appearance/set" {
			t.Errorf("path=%s want /v2/user-asset/appearance/set", r.URL.Path)
		}

		if r.Header.Get("User-Agent") != desktopUAFor(account) {
			t.Errorf("UA=%q want desktop UA", r.Header.Get("User-Agent"))
		}

		raw, _ := io.ReadAll(r.Body)
		json.Unmarshal(raw, &got)
		w.Write([]byte(`{"code":0,"msg":"OK"}`))
	}))
	defer srv.Close()

	c := &Client{HTTP: srv.Client(), ChatBaseCN: srv.URL}
	if err := c.SetAppearanceTheme(account, "theme-tkmw7j"); err != nil {
		t.Fatalf("set theme: %v", err)
	}

	if got["kind"] != "theme" || got["resource_key"] != "theme-tkmw7j" {
		t.Errorf("body=%v want kind=theme resource_key=theme-tkmw7j", got)
	}
}
