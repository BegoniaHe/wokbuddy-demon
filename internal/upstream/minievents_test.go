package upstream

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
)

func TestMPEventBase(t *testing.T) {
	t.Parallel()

	account := &auth.Auth{UID: "u-1", Nickname: "测试"}

	base := mpEventBase(account)
	for _, k := range []string{"ideType", "extName", "ideName", "platform", "userId"} {
		if _, ok := base[k]; !ok {
			t.Errorf("missing common field %s", k)
		}
	}

	if base["ideType"] != "WorkBuddy_MP" || base["extName"] != "workbuddy-mp" {
		t.Errorf("fingerprint ideType=%v extName=%v", base["ideType"], base["extName"])
	}

	// machineId 按 uid 稳定派生：同号恒定、异号互异（不再是全局硬编码常量）。
	got, _ := base["machineId"].(string)
	if !uuidV4HeaderPattern.MatchString(got) {
		t.Errorf("machineId = %q 不是 UUIDv4 形态", got)
	}

	if mpEventBase(account)["machineId"] != got {
		t.Error("同账号 machineId 不稳定")
	}

	if mpEventBase(&auth.Auth{UID: "u-2"})["machineId"] == got {
		t.Error("不同账号 machineId 相同")
	}
}

func TestSchoolChatTimesEvents(t *testing.T) {
	t.Parallel()

	table := SchoolChatTimesEvents("conv-1")
	if table["eventCode"] != "chat_request_send" {
		t.Errorf("eventCode=%v", table["eventCode"])
	}

	if table["conversationId"] != "conv-1" || table["codebuddy.session_id"] != "conv-1" {
		t.Errorf("conversation join fields missing: %v", table)
	}

	b, _ := json.Marshal(table)
	if !strings.Contains(string(b), "agentName") {
		t.Error("agentName missing")
	}
}
