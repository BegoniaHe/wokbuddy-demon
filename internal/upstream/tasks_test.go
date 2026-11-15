package upstream

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
)

// TestClaimRewardWebEndpoint 领奖走 Web 域（workbuddy.cn）、任务码在路径里、无 body。
// 这是与 CLI 域（copilot.tencent.com/v2/.../reward/claim，task_code 在 body）的关键区别——
// 后者路径不存在，曾导致长期 400 "task not completed" 误判为"上游不支持领取"。
func TestClaimRewardWebEndpoint(t *testing.T) {
	t.Parallel()

	var (
		gotPath, gotMethod, gotBody string
		gotPlatform, gotOrigin      string
		gotReferer                  string
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = r.URL.Path, r.Method
		gotPlatform = r.Header.Get("X-Client-Platform")
		gotOrigin = r.Header.Get("Origin")
		gotReferer = r.Header.Get("Referer")
		buf := make([]byte, 64)
		n, _ := r.Body.Read(buf)
		gotBody = string(buf[:n])

		w.Write([]byte(`{"code":0,"msg":"OK","data":{"already_claimed":false,"credit":100,"energy":5}}`))
	}))
	defer srv.Close()

	c := &Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL, WebBaseCN: srv.URL}
	a := &auth.Auth{AccessToken: "at", UID: "u1"}

	credit, energy, err := c.ClaimReward(a, "Model_chat_GLM5.2")
	if err != nil {
		t.Fatalf("claim: %v", err)
	}

	if credit != 100 || energy != 5 {
		t.Errorf("credit/energy = %d/%d, want 100/5", credit, energy)
	}

	if gotMethod != http.MethodPost {
		t.Errorf("method=%s want POST", gotMethod)
	}

	if want := "/activity/growth/tasks/Model_chat_GLM5.2/claim"; gotPath != want {
		t.Errorf("path=%q want %q（任务码必须在路径里）", gotPath, want)
	}

	if gotBody != "" {
		t.Errorf("claim 不应携带 body，got %q", gotBody)
	}

	if gotPlatform != "web" {
		t.Errorf("x-client-platform=%q want web", gotPlatform)
	}

	if gotOrigin != srv.URL {
		t.Errorf("Origin=%q want %q（必须与请求主机同域）", gotOrigin, srv.URL)
	}

	if gotReferer != srv.URL+"/profile/growth-center" {
		t.Errorf("Referer=%q want %q", gotReferer, srv.URL+"/profile/growth-center")
	}
}

// TestClaimRewardAlreadyClaimed 重复领取：上游返回 already_claimed=true，
// 本地应视为"无新增奖励但不报错"（幂等语义）。
func TestClaimRewardAlreadyClaimed(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"code":0,"msg":"OK","data":{"already_claimed":true}}`))
	}))
	defer srv.Close()

	c := &Client{HTTP: srv.Client(), WebBaseCN: srv.URL}

	credit, energy, err := c.ClaimReward(&auth.Auth{AccessToken: "at", UID: "u1"}, "chat_5")
	if err != nil {
		t.Fatalf("already_claimed should not error: %v", err)
	}

	if credit != 0 || energy != 0 {
		t.Errorf("already claimed should yield 0/0, got %d/%d", credit, energy)
	}
}

// TestClaimRewardNotCompleted 未达标：上游 400 + task not completed 应作为错误透出。
func TestClaimRewardNotCompleted(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]any{"code": 400, "msg": "task not completed"})
	}))
	defer srv.Close()

	c := &Client{HTTP: srv.Client(), WebBaseCN: srv.URL}
	if _, _, err := c.ClaimReward(&auth.Auth{AccessToken: "at", UID: "u1"}, "chat_5"); err == nil {
		t.Fatal("want error for not-completed task")
	}
}

// TestClaimRewardMPNoWebReplayOnServerError chat 域 5xx 不应降级 Web 域重放：
// 同一份上游状态，重放只会白打一次并可能在原错误（限流）已生效时放大压力。
// 只有 400（部分任务/租户形态）才走降级路径。
func TestClaimRewardMPNoWebReplayOnServerError(t *testing.T) {
	t.Parallel()

	var webCalls atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "claim") && r.Header.Get("X-Client-Platform") == "web" {
			webCalls.Add(1)
			w.Write([]byte(`{"code":0,"msg":"OK","data":{"credit":100,"energy":5}}`))

			return
		}

		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"code":500,"msg":"boom"}`))
	}))
	defer srv.Close()

	c := &Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL, WebBaseCN: srv.URL}
	a := &auth.Auth{AccessToken: "at", UID: "u1"}

	if _, _, err := c.ClaimRewardMP(a, "Model_chat_GLM5.2"); err == nil {
		t.Fatal("500 应原样上抛")
	}

	if got := webCalls.Load(); got != 0 {
		t.Fatalf("web 域重放 %d 次 want 0（只有 400 才降级）", got)
	}
}

// TestClaimRewardMPWebReplayOnBadRequest chat 域 400 仍按既有契约降级 Web 域。
func TestClaimRewardMPWebReplayOnBadRequest(t *testing.T) {
	t.Parallel()

	var webCalls atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "claim") && r.Header.Get("X-Client-Platform") == "web" {
			webCalls.Add(1)
			w.Write([]byte(`{"code":0,"msg":"OK","data":{"credit":100,"energy":5}}`))

			return
		}

		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"code":400,"msg":"bad"}`))
	}))
	defer srv.Close()

	c := &Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL, WebBaseCN: srv.URL}
	a := &auth.Auth{AccessToken: "at", UID: "u1"}

	credit, energy, err := c.ClaimRewardMP(a, "Model_chat_GLM5.2")
	if err != nil {
		t.Fatalf("claim: %v", err)
	}

	if credit != 100 || energy != 5 {
		t.Errorf("credit/energy=%d/%d want 100/5", credit, energy)
	}

	if got := webCalls.Load(); got != 1 {
		t.Fatalf("web 域重放 %d 次 want 1", got)
	}
}
