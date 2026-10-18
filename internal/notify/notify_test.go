package notify

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestNotifierSendsAndDeduplicates(t *testing.T) {
	got := make(chan payload, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var p payload
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			t.Errorf("decode: %v", err)
		}
		got <- p
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	n := New(Config{URL: srv.URL, DedupeWindow: time.Minute})
	defer n.Close()
	e := Event{Event: EventSessionDead, UID: "abcdefghijk", Nickname: "nick", Realm: "cn", Summary: "dead"}
	n.Notify(e)
	n.Notify(e) // 同事件同账号窗口内去重

	select {
	case p := <-got:
		if p.Event != EventSessionDead || p.UID != "abcdefgh" {
			t.Fatalf("payload=%+v", p)
		}
		if _, err := time.Parse(time.RFC3339, p.Time); err != nil {
			t.Fatalf("time=%q not RFC3339: %v", p.Time, err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("webhook 未收到事件")
	}
	select {
	case p := <-got:
		t.Fatalf("去重失败，重复发送: %+v", p)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestNotifierEventFilter(t *testing.T) {
	got := make(chan payload, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var p payload
		_ = json.NewDecoder(r.Body).Decode(&p)
		got <- p
	}))
	defer srv.Close()

	n := New(Config{URL: srv.URL, Events: []string{EventCreditLow}})
	defer n.Close()
	n.Notify(Event{Event: EventSessionDead, UID: "u1"})
	n.Notify(Event{Event: EventCreditLow, UID: "u2"})
	select {
	case p := <-got:
		if p.Event != EventCreditLow {
			t.Fatalf("未订阅事件被发送: %+v", p)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("订阅事件未发送")
	}
}

func TestNotifierDisabledAndInvalidURL(t *testing.T) {
	n := New(Config{})
	n.Notify(Event{Event: EventSessionDead})
	n.Close() // 不应 panic

	if err := ValidateURL("ftp://example.com"); err == nil {
		t.Fatal("ftp URL 应被拒绝")
	}
	if err := ValidateURL("https://example.com/hook"); err != nil {
		t.Fatalf("合法 URL: %v", err)
	}
	if err := ValidateURL(""); err != nil {
		t.Fatalf("空 URL 应允许: %v", err)
	}
}

// TestNotifierSendFailureDoesNotBlock 上游 5xx 只记日志，Notify/Close 正常返回。
func TestNotifierSendFailureDoesNotBlock(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	n := New(Config{URL: srv.URL})
	n.Notify(Event{Event: EventTaskFailed, UID: "u1"})
	n.Close()
}

// TestValidateURLRedactsCredentials 校验错误不得回显 webhook URL（通常即 bearer token）。
func TestValidateURLRedactsCredentials(t *testing.T) {
	for _, raw := range []string{
		"ftp://user:secret@example.com/hook",
		"http://user:secret@example.com/%zz",
	} {
		err := ValidateURL(raw)
		if err == nil {
			t.Fatalf("ValidateURL(%q) 应报错", raw)
		}
		if strings.Contains(err.Error(), "secret") {
			t.Fatalf("错误回显了凭证: %v", err)
		}
	}
}
