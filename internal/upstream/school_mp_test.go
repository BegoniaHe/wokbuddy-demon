package upstream

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
)

func TestMPEventBase(t *testing.T) {
	t.Parallel()

	a := &auth.Auth{UID: "u-1", Nickname: "测试"}

	base := mpEventBase(a)
	for _, k := range []string{"ideType", "extName", "ideName", "platform", "userId"} {
		if _, ok := base[k]; !ok {
			t.Errorf("missing common field %s", k)
		}
	}

	if base["ideType"] != "WorkBuddy_MP" || base["extName"] != "workbuddy-mp" {
		t.Errorf("fingerprint ideType=%v extName=%v", base["ideType"], base["extName"])
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
