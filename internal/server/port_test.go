package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/BegoniaHe/wokbuddy-demon/internal/upstream"
)

// fakeResponder 记录核心写出的结构化结果，用于证明 chatFlow 无需 net/http 即可运行。
type fakeResponder struct {
	jsonStatus int
	jsonBody   any
	errStatus  int
	errCode    string
	errMsg     string
	errHint    string
}

func (f *fakeResponder) JSON(status int, body any) {
	f.jsonStatus, f.jsonBody = status, body
}

func (f *fakeResponder) Error(status int, code, msg, hint string) {
	f.errStatus, f.errCode, f.errMsg, f.errHint = status, code, msg, hint
}

func (f *fakeResponder) Header(string, string) {}

func (f *fakeResponder) Stream(io.Reader, func(string) string, *upstream.MarkupRepair, func(string)) error {
	return nil
}

// TestChatFlowEmitsThroughPort 核心（chatFlow）只通过 chatResponder 端口写出结果：
// 不经 httptest / http.ResponseWriter，也能驱动末端错误透传。
func TestChatFlowEmitsThroughPort(t *testing.T) {
	h := &Handler{cfg: Config{}}
	out := &fakeResponder{}
	f := h.newChatFlow(chatInput{
		body:  []byte(`{"model":"cn:glm-5.2","stream":false}`),
		model: "cn:glm-5.2",
	}, out)

	f.writeFinalError()

	if out.errStatus != http.StatusServiceUnavailable {
		t.Fatalf("errStatus=%d want 503", out.errStatus)
	}
	if out.errCode != "no_healthy_account" {
		t.Fatalf("errCode=%q want no_healthy_account", out.errCode)
	}
	if f.st.status != http.StatusServiceUnavailable {
		t.Fatalf("stat status=%d want 503", f.st.status)
	}
}

// TestUnknownPathReturnsOpenAIError 未注册路径返回 OpenAI 风格 JSON 404，
// 而非 ServeMux 裸 404；含方法不匹配（GET /v1/chat/completions）也落同一兜底。
func TestUnknownPathReturnsOpenAIError(t *testing.T) {
	h := NewHandler(Config{})
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/v1/unknown"},
		{http.MethodPost, "/v1/embeddings"},
		{http.MethodGet, "/v1/chat/completions"},
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s %s: status=%d want 404", tc.method, tc.path, rec.Code)
		}
		var body struct {
			Error struct {
				Message string `json:"message"`
				Type    string `json:"type"`
			} `json:"error"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("%s %s: not json: %v (body=%q)", tc.method, tc.path, err, rec.Body.String())
		}
		if body.Error.Type != "invalid_request_error" {
			t.Errorf("%s %s: type=%q want invalid_request_error", tc.method, tc.path, body.Error.Type)
		}
		if body.Error.Message == "" {
			t.Errorf("%s %s: empty message", tc.method, tc.path)
		}
	}
}
