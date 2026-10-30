package upstream

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
)

var uuidV7TestPattern = regexp.MustCompile(`^[0-9a-f]{12}7[0-9a-f]{3}[89ab][0-9a-f]{15}$`)

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

// TestIsTransientBillingErrProcessing 429「请求处理中」按瞬时错误重试（issue #250）。
func TestIsTransientBillingErrProcessing(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"429 请求处理中", &Error{Kind: ErrSoftRate, Status: 429, Msg: "请求处理中，请稍后重试"}, true},
		{"429 processing", &Error{Kind: ErrSoftRate, Status: 429, Msg: "request processing"}, true},
		{"429 普通限流", &Error{Kind: ErrSoftRate, Status: 429, Msg: "rate limit"}, false},
		{"业务错误", &Error{Kind: ErrClient, Status: 400, Msg: "请求处理中"}, false},
		{"5xx", &Error{Kind: ErrServer, Status: 500}, true},
		// 传输层错误（*url.Error）重试；io.ErrUnexpectedEOF 这类读取截断属于
		// 本地确定性失败，按 issue #423 不再重试。
		{"网络错误", &url.Error{Op: "Post", URL: "https://billing.example", Err: io.ErrUnexpectedEOF}, true},
		{"读取截断", io.ErrUnexpectedEOF, false},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			if got := isTransientBillingErr(testCase.err); got != testCase.want {
				t.Fatalf("isTransientBillingErr(%v)=%v want %v", testCase.err, got, testCase.want)
			}
		})
	}
}

// TestChatRequestEventShapeMatches593 钉住 chat_request_send 的 5.9.3 载荷键集：
// 键集**精确相等**（不多不少），agentType=main、isMaxMode=false、fileUri 为空串；
// 5.8.x 的 maxToken / maxSteps / temperature / maxRetries / skillCount / totalCount
// 不再出现；无工作区时不产生 vcs* 四键（VcsInfoProcessor 只在 fileUri 非空时改写，
// #366）。
func TestChatRequestEventShapeMatches593(t *testing.T) {
	t.Parallel()

	account := &auth.Auth{UID: "u-shape", AccessToken: "at"}
	event := newChatRequestEvent(account, "conv-1", "01a17a9c4d2f7b3e8c5a6d4e2f1b0a93", "glm-5.2", "GLM 5.2")

	raw, err := json.Marshal(event)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	wantKeys := []string{
		"eventCode", "timestamp", "reportDelay", "mode", "conversationId", "requestId",
		"inputLength", "requestModelId", "requestModelName", "isPlan",
		"isAutoExecuteTerminal", "isAutoModify", "codebaseEnable", "isMaxMode",
		"mentionContexts", "knowledgeId", "knowledgeName", "codebaseId",
		"mentionContextCount", "command", "expertId", "recommendId", "skillId",
		"fileUri", "presentAt", "traceId",
		"rootRequestId", "parentConversationId", "agentName", "agentType", "userId",
	}

	gotKeys := make([]string, 0, len(got))
	for key := range got {
		gotKeys = append(gotKeys, key)
	}

	slices.Sort(gotKeys)

	wanted := slices.Clone(wantKeys)
	slices.Sort(wanted)

	if !slices.Equal(gotKeys, wanted) {
		t.Fatalf("载荷键集不符\n got=%v\nwant=%v", gotKeys, wanted)
	}

	assertChatRequestEventValues(t, event, got)
}

// assertChatRequestEventValues 断言 5.9.3 的字段取值语义：agentType=main、
// isMaxMode=false、agentName 空串、fileUri 空串；条件发字段不出现、已删字段不复活、
// 无工作区时不出现 vcs* 四键。
func assertChatRequestEventValues(t *testing.T, event chatRequestEvent, got map[string]any) {
	t.Helper()

	if event.AgentType != "main" {
		t.Errorf("agentType=%q want main（官方只可能是 main/subagent/team）", event.AgentType)
	}

	if event.IsMaxMode {
		t.Error("isMaxMode 必须为 false（网关无 thinking 覆盖状态）")
	}

	if event.AgentName != "" {
		t.Errorf("agentName=%q want 空串（官方空值形态）", event.AgentName)
	}

	if event.FileURI != "" {
		t.Errorf("fileUri=%q want 空串（无工作区）", event.FileURI)
	}

	// VcsInfoProcessor 只在 fileUri 非空时写 vcs* 四键并删掉 fileUri；网关无工作区，
	// 官方在该形态下四个键都不出现。
	for _, key := range []string{"vcsType", "vcsRepo", "vcsBranchName", "vcsRevId"} {
		if _, ok := got[key]; ok {
			t.Errorf("无工作区时 %s 不应出现在载荷里（官方只在 fileUri 非空时写）", key)
		}
	}

	assertAbsent(t, got, "网关无该状态，不应出现在载荷里", "source", "inputAttemptId", "connectorId")
	assertAbsent(t, got, "5.9.3 已废弃字段仍在载荷里",
		"maxToken", "maxSteps", "temperature", "maxRetries", "skillCount", "totalCount")
}

// assertAbsent 断言这些键都不在载荷里。
func assertAbsent(t *testing.T, got map[string]any, msg string, keys ...string) {
	t.Helper()

	for _, key := range keys {
		if _, ok := got[key]; ok {
			t.Errorf("%s: %s", msg, key)
		}
	}
}
