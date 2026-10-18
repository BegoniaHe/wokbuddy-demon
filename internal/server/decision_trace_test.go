package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
	"github.com/BegoniaHe/wokbuddy-demon/internal/reqlog"
)

// TestDecisionTraceRecorded 开启后请求归档带决策链（候选/Top/最终 uid），关闭时为空。
func TestDecisionTraceRecorded(t *testing.T) {
	t.Parallel()

	rec := reqlog.New(reqlog.Config{})
	pool := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	client := newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true })
	handler := NewHandler(&Config{
		Pool: pool, Upstream: client, RequestLog: rec,
		DecisionTrace: true, DecisionTraceSample: 1,
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[]}`))
	handler.ServeHTTP(httptest.NewRecorder(), req)

	recent := rec.Snapshot().Recent
	if len(recent) == 0 || recent[0].Decision == nil {
		t.Fatalf("decision 未记录: %+v", recent)
	}

	d := recent[0].Decision
	if d.Candidates != 1 || d.FinalUID == "" || len(d.Top) != 1 {
		t.Fatalf("decision=%+v", d)
	}

	rec2 := reqlog.New(reqlog.Config{})
	h2 := NewHandler(&Config{Pool: pool, Upstream: client, RequestLog: rec2})
	req2 := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[]}`))
	h2.ServeHTTP(httptest.NewRecorder(), req2)

	if got := rec2.Snapshot().Recent; len(got) == 0 || got[0].Decision != nil {
		t.Fatalf("关闭时不应记录: %+v", got)
	}
}
