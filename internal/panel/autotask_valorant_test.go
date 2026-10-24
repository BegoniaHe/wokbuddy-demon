package panel

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
	"github.com/BegoniaHe/wokbuddy-demon/internal/pool"
	"github.com/BegoniaHe/wokbuddy-demon/internal/upstream"
)

// TestRunValorantExpertChain Valorant 复用专家召唤+真实对话+LOCAL 使用链。
//
//nolint:cyclop // 假上游按端点线性分派，拆开反而难读。
func TestRunValorantExpertChain(t *testing.T) {
	t.Parallel()

	var (
		mu       sync.Mutex
		reports  [][]map[string]any
		requests []string
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.URL.Path)

		switch r.URL.Path {
		case "/portal/operation-platform/market/expert/list":
			_, _ = w.Write([]byte(`{"code":0,"data":{"experts":[{"expert_id":"ex_xQHHD1u5Jkua",` +
				`"expert_type":"agent","display_name_zh":"无畏契约拼豆专家","profession_zh":"无畏契约拼豆专家",` +
				`"version":"1.2.1","categories":["expert-all"]}]}}`))
		case "/v2/report":
			raw, _ := io.ReadAll(r.Body)

			var events []map[string]any
			if err := json.Unmarshal(raw, &events); err != nil {
				t.Errorf("report body: %v", err)
			}

			mu.Lock()

			reports = append(reports, events)
			mu.Unlock()

			_, _ = w.Write([]byte(`{"code":0,"msg":"OK"}`))
		case "/v2/chat/completions":
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("data: {\"id\":\"0123456789abcdef0123456789abcdef\"}\n\n"))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	client := &upstream.Client{
		HTTP:              srv.Client(),
		ChatBaseCN:        srv.URL,
		BillingBaseCN:     srv.URL,
		WebBaseCN:         srv.URL,
		ChatBaseGlobal:    srv.URL,
		BillingBaseGlobal: srv.URL,
	}
	state := pool.New("")
	state.Add(&auth.Auth{UID: "u1", AccessToken: "at-u1"})

	msg, err := runValorant(&Panel{cfg: Config{Pool: state, Upstream: client}}, state.AuthByUID("u1"))
	if err != nil {
		t.Fatalf("runValorant: %v", err)
	}

	if !strings.Contains(msg, "无畏契约") {
		t.Fatalf("message=%q", msg)
	}

	mu.Lock()
	defer mu.Unlock()

	if len(reports) != 2 {
		t.Fatalf("reports=%d want 2", len(reports))
	}

	if !hasEvent(reports[0], "expert_summon_click", "ex_xQHHD1u5Jkua") {
		t.Fatalf("summon chain missing: %+v", reports[0])
	}

	var actualUse map[string]any

	for _, ev := range reports[1] {
		if ev["eventCode"] == "expert_actual_use" {
			actualUse = ev
		}
	}

	if actualUse == nil {
		t.Fatalf("actual_use missing: %+v", reports[1])
	}

	if actualUse["id"] != "ex_xQHHD1u5Jkua" || actualUse["mode"] != "LOCAL" ||
		actualUse["requestId"] != "0123456789abcdef0123456789abcdef" {
		t.Fatalf("actual_use=%+v", actualUse)
	}

	if !hasEvent(reports[1], "agent_task_created", "ex_xQHHD1u5Jkua") {
		t.Fatalf("chat chain must carry expert id: %+v", reports[1])
	}
}

// hasEvent 报告事件列表里是否存在指定 eventCode + 指定字段值（字段名为空时只查 code）。
func hasEvent(events []map[string]any, code, id string) bool {
	for _, ev := range events {
		if ev["eventCode"] != code {
			continue
		}

		if id == "" || ev["id"] == id || ev["expert_id"] == id {
			return true
		}
	}

	return false
}
