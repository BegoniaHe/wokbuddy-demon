package upstream

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
)

var (
	uuidV4TestPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	uuidV7TestPattern = regexp.MustCompile(`^[0-9a-f]{12}7[0-9a-f]{3}[89ab][0-9a-f]{15}$`)
)

// TestReportChatActivitySendsArrayWithUserID 断言出站 body 是数组、含 userId、eventCode 正确。
func TestReportChatActivitySendsArrayWithUserID(t *testing.T) {
	t.Parallel()

	var (
		got []map[string]any
		raw []byte
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/report" {
			t.Errorf("path=%s want /v2/report", r.URL.Path)
		}

		if r.Method != http.MethodPost {
			t.Errorf("method=%s want POST", r.Method)
		}

		raw, _ = io.ReadAll(r.Body)

		var arr []map[string]any
		if err := json.Unmarshal(raw, &arr); err != nil {
			t.Fatalf("body must be a JSON array: %v (body=%s)", err, raw)
		}

		got = arr

		w.Write([]byte(`{"code":0,"msg":"OK"}`))
	}))
	defer srv.Close()

	conv := "0f0e0d0c-0b0a-4908-8706-050403020100"

	c := &Client{HTTP: srv.Client(), BillingBaseCN: srv.URL}
	if err := c.ReportChatActivity(&auth.Auth{AccessToken: "at", UID: "u-active"}, conv, ""); err != nil {
		t.Fatalf("report: %v", err)
	}

	if len(got) != 1 {
		t.Fatalf("events=%d want 1", len(got))
	}

	assertChatActivityBody(t, got[0], raw, conv)
}

// assertChatActivityBody 断言活跃上报事件的 ID 形态与无品牌约束。
func assertChatActivityBody(t *testing.T, table map[string]any, raw []byte, conv string) {
	t.Helper()

	if table["eventCode"] != "chat_request_send" {
		t.Errorf("eventCode=%v want chat_request_send", table["eventCode"])
	}

	if table["userId"] != "u-active" {
		t.Errorf("userId=%v want u-active（缺失则服务端 200 但静默丢弃）", table["userId"])
	}

	if table["conversationId"] != conv {
		t.Errorf("conversationId=%v want %s", table["conversationId"], conv)
	}

	reqID, _ := table["requestId"].(string)
	if !uuidV7TestPattern.MatchString(reqID) {
		t.Errorf("requestId=%q 不是 UUIDv7 32 hex", reqID)
	}

	if table["traceId"] != reqID || table["rootRequestId"] != reqID {
		t.Errorf("traceId/rootRequestId=%v/%v want requestId %q", table["traceId"], table["rootRequestId"], reqID)
	}

	if strings.Contains(string(raw), "wb2api") || strings.Contains(string(raw), "wb2a") {
		t.Errorf("出站 report body 含品牌字面量: %s", raw)
	}

	if table["mode"] != "craft" {
		t.Errorf("mode=%v want craft", table["mode"])
	}
}

// TestReportChatActivityServerError 业务 code 非 0 返回 *Error。
func TestReportChatActivityServerError(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`boom`))
	}))
	defer srv.Close()

	c := &Client{HTTP: srv.Client(), BillingBaseCN: srv.URL}

	err := c.ReportChatActivity(&auth.Auth{AccessToken: "at", UID: "u1"}, "cid", "")
	if err == nil {
		t.Fatal("want error on 500")
	}
}
