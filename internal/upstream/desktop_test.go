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

	client := &Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, Profile: testProfileSet(t)}
	account := &auth.Auth{AccessToken: "at", UID: "u-dt", Nickname: "RenJie"}

	err := client.ReportDesktopEvent(account,
		DesktopEvent{"eventCode": "agent_task_created", "mode": "craft"})
	if err != nil {
		t.Fatalf("report desktop: %v", err)
	}

	if want := client.desktopUAFor(account); gotUA != want {
		t.Errorf("UA=%q want %q", gotUA, want)
	}

	// body 的 userAgent 与上报头同源，os 对齐 navigator.platform（Win32）。
	if want := client.desktopUAFor(account); got[0]["userAgent"] != want {
		t.Errorf("userAgent=%v want %q（与桌面上报 UA 同源）", got[0]["userAgent"], want)
	}

	if got := got[0]["os"]; got != "Win32" {
		t.Errorf("os=%v want Win32（navigator.platform）", got)
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
//
//nolint:cyclop // 逐字段校验公共指纹，拆开反而难读。
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

	if got, ok := table["reportDelay"].(float64); !ok || got != 0 {
		t.Errorf("reportDelay=%v want 0（单请求立即发送）", table["reportDelay"])
	}

	if _, ok := table["presentAt"]; ok {
		t.Error("基础 /v2/report 指纹不应注入 presentAt")
	}
	// machineId 按账号派生、sessionId 进程内随机，都应是带连字符的 UUIDv4。
	for _, key := range []string{"machineId", "sessionId"} {
		if got, _ := table[key].(string); !uuidV4HeaderPattern.MatchString(got) {
			t.Errorf("%s=%q want UUIDv4", key, got)
		}
	}

	assertDesktopMachineFingerprint(t, table)
}

// TestReportDesktopEventBusinessFieldsOverride 业务载荷同名键优先于公共指纹：
// 真实设备对齐时可用 os / userAgent 覆盖默认伪装值。
func TestReportDesktopEventBusinessFieldsOverride(t *testing.T) {
	t.Parallel()

	var got []map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatalf("body must be array: %v", err)
		}

		w.Write([]byte(`{"code":0,"msg":"OK"}`))
	}))
	defer srv.Close()

	client := &Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, Profile: testProfileSet(t)}
	account := &auth.Auth{AccessToken: "at", UID: "u-override"}

	err := client.ReportDesktopEvent(account, DesktopEvent{
		"eventCode": "agent_task_created",
		"os":        "Linux",
		"userAgent": "RealDevice/1",
	})
	if err != nil {
		t.Fatalf("report desktop: %v", err)
	}

	if len(got) != 1 {
		t.Fatalf("events=%d want 1", len(got))
	}

	if got[0]["os"] != "Linux" || got[0]["userAgent"] != "RealDevice/1" {
		t.Errorf("os/userAgent=%v/%v want Linux/RealDevice/1（业务字段优先）", got[0]["os"], got[0]["userAgent"])
	}
}

// TestDesktopSessionIDProcessScoped sessionId 在同一进程内按账号稳定、不同账号不同，
// 且不随指纹调用重新生成。
func TestDesktopSessionIDProcessScoped(t *testing.T) {
	t.Parallel()

	client := newProfileClient(t)
	firstAccount := &auth.Auth{UID: "u-session-a", Nickname: "a"}
	secondAccount := &auth.Auth{UID: "u-session-b", Nickname: "b"}

	first := client.desktopSessionID(firstAccount)

	second := client.desktopSessionID(firstAccount)
	if first != second {
		t.Fatalf("同账号 sessionId 不稳定：%q vs %q", first, second)
	}

	if !uuidV4HeaderPattern.MatchString(first) {
		t.Fatalf("sessionId=%q want UUIDv4", first)
	}

	if other := client.desktopSessionID(secondAccount); other == first {
		t.Fatalf("不同账号共用 sessionId=%q", first)
	}

	fp := client.desktopFingerprint(firstAccount)
	if fp["sessionId"] != first {
		t.Errorf("fingerprint sessionId=%v want %q", fp["sessionId"], first)
	}
}

// TestDesktopTimezonePerRealm cn 固定上海；global 取宿主 TZ，取不到则省略。
func TestDesktopTimezonePerRealm(t *testing.T) {
	t.Setenv("TZ", "America/New_York")

	if got := desktopTimezone("cn"); got != "Asia/Shanghai" {
		t.Errorf("cn timezone=%q want Asia/Shanghai", got)
	}

	if got := desktopTimezone("global"); got != "America/New_York" {
		t.Errorf("global timezone=%q want America/New_York", got)
	}

	client := newProfileClient(t)
	global := &auth.Auth{UID: "u-gl-tz", Domain: "www.workbuddy.ai"}

	if got := client.desktopFingerprint(global)["timezone"]; got != "America/New_York" {
		t.Errorf("global 指纹 timezone=%v want America/New_York", got)
	}
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

	client := newProfileClient(t)
	cnAccount := &auth.Auth{UID: "u-cn", Nickname: "cn", AccessToken: "at"}
	globalAccount := &auth.Auth{UID: "u-gl", Nickname: "gl", AccessToken: "at", Domain: "www.workbuddy.ai"}

	cnFP := client.desktopFingerprint(cnAccount)
	if cnFP["ideVersion"] != "9.9.9" || cnFP["commit"] != "cccccccccccccccccccccccccccccccccccccccc" {
		t.Errorf("CN 版本三元组=%v/%v", cnFP["ideVersion"], cnFP["commit"])
	}

	if _, ok := cnFP["qimei36"]; ok {
		t.Error("CN 指纹不应带 qimei36")
	}

	globalFP := client.desktopFingerprint(globalAccount)
	if globalFP["ideVersion"] != "9.9.10" || globalFP["commit"] != "dddddddddddddddddddddddddddddddddddddddd" {
		t.Errorf("global 版本三元组=%v/%v", globalFP["ideVersion"], globalFP["commit"])
	}

	if _, ok := globalFP["qimei36"]; ok {
		t.Error("global 指纹不应带 qimei36")
	}

	if got, want := client.desktopUAFor(cnAccount), "WorkBuddy/9.9.9 WorkBuddy/9.9.9 CLI/8.8.8"; got != want {
		t.Errorf("CN desktop UA=%q want %q", got, want)
	}

	assertDesktopUAAndOS(t, cnFP, client, cnAccount, "CN")

	if got, want := client.desktopUAFor(globalAccount), "workbuddy-ai/9.9.10 workbuddy-ai/9.9.10 CLI/8.8.9"; got != want {
		t.Errorf("global desktop UA=%q want %q", got, want)
	}

	assertDesktopUAAndOS(t, globalFP, client, globalAccount, "global")
}

// assertDesktopUAAndOS 断言指纹的 userAgent 与桌面上报 UA 同源、os 对齐 Win32。
func assertDesktopUAAndOS(t *testing.T, fingerprint map[string]any, client *Client, account *auth.Auth, realm string) {
	t.Helper()

	if want := client.desktopUAFor(account); fingerprint["userAgent"] != want {
		t.Errorf("%s userAgent=%v want %q", realm, fingerprint["userAgent"], want)
	}

	if fingerprint["os"] != "Win32" {
		t.Errorf("%s os=%v want Win32", realm, fingerprint["os"])
	}
}

// TestDesktopBuildFingerprintProfile 桌面构建三元组只来自 profile，
// 不再有内置 Win32 基线可回写。
func TestDesktopBuildFingerprintProfile(t *testing.T) {
	t.Parallel()

	client := newProfileClient(t)

	cn := client.desktopBuildFor(&auth.Auth{UID: "u-cn"})
	if cn.version != "9.9.9" || cn.commit != "cccccccccccccccccccccccccccccccccccccccc" || cn.releaseDate != 1792000000000 {
		t.Errorf("cn build=%+v want 9.9.9/cccc/1792000000000", cn)
	}

	global := client.desktopBuildFor(&auth.Auth{UID: "u-gl", Domain: "www.workbuddy.ai"})
	if global.version != "9.9.10" || global.commit != "dddddddddddddddddddddddddddddddddddddddd" || global.releaseDate != 1792100000000 {
		t.Errorf("global build=%+v want 9.9.10/dddd/1792100000000", global)
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

// TestDesktopChatSequenceAssistantMessageID 助手消息必须使用独立于用户消息的
// UUIDv7 messageId，并在 send/response/status 三个事件间保持一致。
func TestDesktopChatSequenceAssistantMessageID(t *testing.T) {
	t.Parallel()

	events := DesktopChatSequence("conv-1", "req-1", "user-msg", "fast-model", "fast-model")

	assistantID, _ := events[1]["messageId"].(string)
	if !uuidV7HeaderPattern.MatchString(assistantID) {
		t.Fatalf("assistant messageId=%q want UUIDv7", assistantID)
	}

	if assistantID == events[0]["messageId"] {
		t.Fatalf("assistant messageId 复用了用户消息 id: %q", assistantID)
	}

	for _, i := range []int{3, 4} {
		if got := events[i]["messageId"]; got != assistantID {
			t.Errorf("events[%d].messageId=%v want %q", i, got, assistantID)
		}
	}
}

// TestSetAppearanceTheme 断言主题设置端点形状（点亮 Hp_Appearance 的 API）。
func TestSetAppearanceTheme(t *testing.T) {
	t.Parallel()

	var got map[string]any

	account := &auth.Auth{AccessToken: "at", UID: "u1"}
	wantUA := "WorkBuddy/9.9.9 WorkBuddy/9.9.9 CLI/8.8.8"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/user-asset/appearance/set" {
			t.Errorf("path=%s want /v2/user-asset/appearance/set", r.URL.Path)
		}

		if r.Header.Get("User-Agent") != wantUA {
			t.Errorf("UA=%q want desktop UA", r.Header.Get("User-Agent"))
		}

		raw, _ := io.ReadAll(r.Body)
		json.Unmarshal(raw, &got)
		w.Write([]byte(`{"code":0,"msg":"OK"}`))
	}))
	defer srv.Close()

	c := &Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, Profile: testProfileSet(t)}
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
