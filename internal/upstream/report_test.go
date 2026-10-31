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
	"time"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
)

var (
	uuidV7TestPattern = regexp.MustCompile(`^[0-9a-f]{12}7[0-9a-f]{3}[89ab][0-9a-f]{15}$`)
	uuidV4TestPattern = regexp.MustCompile(`^[0-9a-f]{12}4[0-9a-f]{3}[89ab][0-9a-f]{15}$`)
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

		// 同一 /v2/report 端点与桌面 report 同头集：CLI 活跃上报也要带 X-Product。
		if got := r.Header.Get("X-Product"); got != "SaaS" {
			t.Errorf("X-Product=%q want SaaS（同端点同头集）", got)
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

	if strings.Contains(string(raw), "wb2api") || strings.Contains(string(raw), "wb2a") {
		t.Errorf("出站 report body 含品牌字面量: %s", raw)
	}

	if table["mode"] != "craft" {
		t.Errorf("mode=%v want craft", table["mode"])
	}

	assertChatActivityIDs(t, table)
	assertChatActivitySample(t, table)
	assertChatActivityFingerprint(t, table)
}

// assertChatActivityIDs 断言 requestId / traceId / rootRequestId 的形态与来源隔离。
func assertChatActivityIDs(t *testing.T, table map[string]any) {
	t.Helper()

	reqID, _ := table["requestId"].(string)
	if !uuidV7TestPattern.MatchString(reqID) {
		t.Errorf("requestId=%q 不是 UUIDv7 32 hex", reqID)
	}

	// traceId 是独立 W3C trace id（随机 v4），不能与 requestId 的 UUIDv7 混用。
	traceID, _ := table["traceId"].(string)
	if !uuidV4TestPattern.MatchString(traceID) {
		t.Errorf("traceId=%q 不是 UUIDv4 形态的 32 hex", traceID)
	}

	if traceID == reqID {
		t.Errorf("traceId 不应复用 requestId %q（官方两者不同源）", reqID)
	}

	if table["rootRequestId"] != reqID {
		t.Errorf("rootRequestId=%v want requestId %q", table["rootRequestId"], reqID)
	}
}

// assertChatActivitySample 断言业务形态：isMaxMode 与条件字段缺席。
func assertChatActivitySample(t *testing.T, table map[string]any) {
	t.Helper()

	// 官方 isMaxMode = getOverrideThinking(id)?.thinkingEnabled === true，
	// 普通会话即 false；网关没有 thinking 覆盖状态，取默认值。
	if table["isMaxMode"] != false {
		t.Errorf("isMaxMode=%v want false（官方默认值，网关无 thinking 覆盖状态）", table["isMaxMode"])
	}

	// source / inputAttemptId / connectorId 都是条件发字段，网关无对应状态 → 整键缺席。
	assertAbsent(t, table, "网关无该状态，不应出现在载荷里", "source", "inputAttemptId", "connectorId")

	// 官方把 codebuddy.session_id / codebuddy.conversation_request_id 作为
	// W3C baggage 注入 traceparent / baggage 请求头，不是 /v2/report 事件字段。
	assertAbsent(t, table, "turn carrier 键属于请求头，不应写进事件体",
		"codebuddy.session_id", "codebuddy.conversation_request_id")

	assertChatActivityWorkspace(t, table)
}

// assertChatActivityWorkspace 断言无工作区形态：fileUri 为空串，VcsInfoProcessor
// 只在 fileUri 非空时写 vcs 四键并删除它，故空串形态下四键缺席。
func assertChatActivityWorkspace(t *testing.T, table map[string]any) {
	t.Helper()

	if fileURI, ok := table["fileUri"]; !ok {
		t.Error("fileUri 应保留为空串（官方无工作区形态）")
	} else if fileURI != "" {
		t.Errorf("fileUri=%v want 空串（网关无工作区）", fileURI)
	}

	assertAbsent(t, table, "网关无工作区，不应带 vcs 四键",
		"vcsType", "vcsRepo", "vcsBranchName", "vcsRevId")

	if _, ok := table["expertId"]; ok {
		t.Error("无专家上下文时不应带 expertId")
	}

	if table["buddyId"] != "" || table["buddyName"] != "" {
		t.Errorf("无 buddy 上下文时应为空串: %v/%v", table["buddyId"], table["buddyName"])
	}
}

// assertChatActivityFingerprint 断言官方报告服务注入的设备与包指纹；该事件不是例外。
func assertChatActivityFingerprint(t *testing.T, table map[string]any) {
	t.Helper()

	for _, key := range []string{"ideName", "ideType", "ideVersion", "machineId", "sessionId", "extName", "os", "arch"} {
		if _, ok := table[key]; !ok {
			t.Errorf("事件缺少桌面指纹键 %s", key)
		}
	}

	if table["os"] != "win32" {
		t.Errorf("os=%v want win32（process.platform）", table["os"])
	}

	if _, ok := table["userAgent"]; ok {
		t.Error("桌面报告体不应带 userAgent（官方 OSEventProcessor 不写该键）")
	}

	if _, ok := table["qimei36"]; ok {
		t.Error("不应上报 qimei36（无真实设备标识，派生值属于伪造）")
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
// 键集**精确相等**（不多不少），agentType=main、isMaxMode=false；5.8.x 的
// maxToken / maxSteps / temperature / maxRetries / skillCount / totalCount 不再
// 出现；source / inputAttemptId 是条件发字段，载荷里没有；buddy* 由
// chatRequestEventMap 在指纹合并阶段补。
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
		"mentionContextCount", "command", "fileUri", "recommendId", "skillId",
		"presentAt", "traceId",
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
// isMaxMode=false、agentName=cli、debounce 时间三件套；条件发字段与 5.8.x
// 已删字段都不复活。
func assertChatRequestEventValues(t *testing.T, event chatRequestEvent, got map[string]any) {
	t.Helper()

	if event.AgentType != "main" {
		t.Errorf("agentType=%q want main（官方只可能是 main/subagent/team）", event.AgentType)
	}

	if event.IsMaxMode {
		t.Error("isMaxMode 必须为 false（网关无 thinking 覆盖状态，官方默认即 false）")
	}

	if event.AgentName != "cli" {
		t.Errorf("agentName=%q want cli（桌面内置 CLI 主会话）", event.AgentName)
	}

	// 官方 report() 入队 reportDelay=0，FIRE_DELAY=2s 后 flush 时改成
	// flush-timestamp；timestamp / presentAt 都是入队（请求发起）时刻。
	if delay, _ := got["reportDelay"].(float64); int64(delay) != desktopReportDebounceMs {
		t.Errorf("reportDelay=%v want %d（官方 debounce 批窗口）", got["reportDelay"], desktopReportDebounceMs)
	}

	if event.Timestamp != event.PresentAt {
		t.Errorf("timestamp=%d presentAt=%d want 相等（入队即请求发起时刻）",
			event.Timestamp, event.PresentAt)
	}

	if delta := time.Now().UnixMilli() - event.Timestamp; delta < desktopReportDebounceMs {
		t.Errorf("timestamp 距 now=%dms want >= %d（flush 前才重算 reportDelay）",
			delta, desktopReportDebounceMs)
	}

	if traceID, _ := got["traceId"].(string); !uuidV4TestPattern.MatchString(traceID) {
		t.Errorf("traceId=%q 不是 UUIDv4 形态的 32 hex", traceID)
	}

	if traceID, _ := got["traceId"].(string); traceID == event.RequestID {
		t.Errorf("traceId 不应复用 requestId %q", event.RequestID)
	}

	assertAbsent(t, got, "网关无该状态，不应出现在载荷里",
		"source", "inputAttemptId", "connectorId",
		"vcsType", "vcsRepo", "vcsBranchName", "vcsRevId", "expertId")
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
