package upstream

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
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

	if got := r.Header.Get("X-Request-ID"); !uuidV7HeaderPattern.MatchString(got) {
		t.Errorf("X-Request-ID=%q want UUIDv7 32 hex", got)
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
	// machineId/sessionId 应为 uid 稳定派生的连字符 UUIDv4。
	for _, key := range []string{"machineId", "sessionId"} {
		if got, _ := table[key].(string); !uuidV4HeaderPattern.MatchString(got) {
			t.Errorf("%s=%q want UUIDv4", key, got)
		}
	}

	assertDesktopMachineFingerprint(t, table)
}

// assertDesktopMachineFingerprint 断言真机 OS 处理器字段，并确认不伪造 qimei36。
func assertDesktopMachineFingerprint(t *testing.T, table map[string]any) {
	t.Helper()

	if table["cpuModel"] == nil || table["cpuModel"] == "" {
		t.Errorf("cpuModel 缺失（真机 os 处理器必带）")
	}

	if _, ok := table["qimei36"]; ok {
		t.Error("不应上报 qimei36（无真实设备标识，派生值属于伪造）")
	}
}

// TestDesktopFingerprintPerRealm 桌面指纹的版本三元组/UA 按 realm 分档，
// 且两域都不注入 qimei36。
func TestDesktopFingerprintPerRealm(t *testing.T) {
	t.Parallel()

	cnAccount := &auth.Auth{UID: "u-cn", Nickname: "cn", AccessToken: "at"}
	globalAccount := &auth.Auth{UID: "u-gl", Nickname: "gl", AccessToken: "at", Domain: "www.workbuddy.ai"}

	cnFP := desktopFingerprint(cnAccount)
	if cnFP["ideVersion"] != cnClientVersion || cnFP["commit"] != cnDesktopCommit {
		t.Errorf("CN 版本三元组=%v/%v", cnFP["ideVersion"], cnFP["commit"])
	}

	if _, ok := cnFP["qimei36"]; ok {
		t.Error("CN 指纹不应带 qimei36")
	}

	globalFP := desktopFingerprint(globalAccount)
	if globalFP["ideVersion"] != globalClientVersion || globalFP["commit"] != globalDesktopCommit {
		t.Errorf("global 版本三元组=%v/%v", globalFP["ideVersion"], globalFP["commit"])
	}

	if _, ok := globalFP["qimei36"]; ok {
		t.Error("global 指纹不应带 qimei36")
	}

	if got, want := desktopUAFor(cnAccount), "WorkBuddy/5.8.3 WorkBuddy/5.8.3 CLI/2.161.0"; got != want {
		t.Errorf("CN desktop UA=%q want %q", got, want)
	}

	if got, want := desktopUAFor(globalAccount), "workbuddy-ai/5.8.4 workbuddy-ai/5.8.4 CLI/2.161.0"; got != want {
		t.Errorf("global desktop UA=%q want %q", got, want)
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

// TestDesktopReportHostDomainAndEnterprise X-Domain 为 host 形态，企业字段按需出现。
func TestDesktopReportHostDomainAndEnterprise(t *testing.T) {
	t.Parallel()

	var header http.Header

	var got []map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		header = r.Header.Clone()

		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &got)

		w.Write([]byte(`{"code":0,"msg":"OK"}`))
	}))
	defer srv.Close()

	c := &Client{HTTP: srv.Client(), ChatBaseCN: srv.URL}
	account := &auth.Auth{AccessToken: "at", UID: "u-dt", EnterpriseID: "ent-1"}

	if err := c.ReportDesktopEvent(account, DesktopEvent{"eventCode": "x"}); err != nil {
		t.Fatalf("report: %v", err)
	}

	host := strings.TrimPrefix(strings.TrimPrefix(srv.URL, "https://"), "http://")
	if got := header.Get("X-Domain"); got != host {
		t.Errorf("X-Domain=%q want host %q", got, host)
	}

	if got[0]["enterpriseId"] != "ent-1" || got[0]["enterprise"] != "ent-1" {
		t.Errorf("enterprise fields=%v/%v", got[0]["enterpriseId"], got[0]["enterprise"])
	}
}

// TestReadServerRequestIDSkipsEmptyID 空 id（`"id":""`）不得卡住扫描：此前
// searchFrom 不推进，同一位置每个 Read 都被重复命中，合法 id 永远读不到。
func TestReadServerRequestIDSkipsEmptyID(t *testing.T) {
	t.Parallel()

	want := strings.Repeat("ab", 16)
	sse := `data: {"id":"","object":"chat.completion.chunk"}` + "\n\n" +
		`data: {"id":"` + want + `"}` + "\n\n"

	got, err := readServerRequestID(strings.NewReader(sse))
	if err != nil {
		t.Fatalf("readServerRequestID: %v", err)
	}

	if got != want {
		t.Fatalf("requestID=%q want %q", got, want)
	}
}

// TestReadServerRequestIDSkipsInvalidID 同一批数据里先出现形状不合法的 id 时，
// 应在本次扫描内继续推进到后续合法 id，而不是等下一次 Read。
func TestReadServerRequestIDSkipsInvalidID(t *testing.T) {
	t.Parallel()

	want := strings.Repeat("0f", 16)
	sse := `data: {"id":"not-a-server-id"}` + "\n\n" +
		`data: {"id":"` + want + `"}` + "\n\n"

	got, err := readServerRequestID(strings.NewReader(sse))
	if err != nil {
		t.Fatalf("readServerRequestID: %v", err)
	}

	if got != want {
		t.Fatalf("requestID=%q want %q", got, want)
	}
}
