package upstream

import (
	"errors"
	"net/http"
	"strings"
	"testing"
)

// TestFrameKindCodeExtraction SSE error 帧优先取帧内业务码，取不到才回落 message。
func TestFrameKindCodeExtraction(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		payload string
		want    ErrKind
	}{
		{"error.code 数字", `{"error":{"code":14018,"message":"credits exhausted"}}`, ErrHardCredit},
		{"error.code 字符串", `{"error":{"code":"14015","message":"auth expired"}}`, ErrSessionDead},
		{"error.data.code", `{"error":{"data":{"code":11129},"message":"tool schema invalid"}}`, ErrBadParams},
		{"data.code", `{"data":{"code":6002},"message":"minute usage limit"}`, ErrSoftRate},
		{"模型权限码", `{"error":{"code":11136,"message":"no model permission"}}`, ErrModelBlocked},
		{"模型下线码", `{"error":{"code":11141,"message":"model offline"}}`, ErrModelAccess},
		{"6004 特判保留", `{"error":{"code":6004,"message":"daily model limit"}}`, ErrSoftRate},
		{"帧内内容拦截码", `{"error":{"code":"11-128","message":"request intercepted"}}`, ErrContentBlocked},
		{"无码回落 message", `{"error":{"message":"prompt is too long"}}`, ErrPromptTooLong},
		{"无码回落限流文案", `{"error":{"message":"rate limit"}}`, ErrSoftRate},
		{"未知码 + 文案", `{"error":{"code":1,"message":"额度不足"}}`, ErrHardCredit},
		{"未知码无文案", `{"error":{"code":1}}`, ErrNone},
		{"非 JSON", `not-json`, ErrNone},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			if got := FrameKind(testCase.payload); got != testCase.want {
				t.Errorf("FrameKind(%q)=%v want %v", testCase.payload, got, testCase.want)
			}
		})
	}
}

// TestDoJSONPreservesStructuredCode HTTP 200 + 业务码必须保留原始 JSON 结构，
// 让 Classify 能按码识别，而不是拼成 `code=N msg=...` 纯文本。
func TestDoJSONPreservesStructuredCode(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		body string
		want ErrKind
	}{
		{"请求级码", `{"code":11129,"msg":"tool schema invalid"}`, ErrBadParams},
		{"额度码", `{"code":14002,"msg":"agent quota exhausted"}`, ErrHardCredit},
		{"限流码", `{"code":6002,"msg":"minute usage limit"}`, ErrSoftRate},
		{"未知码", `{"code":9999,"msg":"unknown"}`, ErrClient},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			client := testClient(func(*http.Request) (*http.Response, error) {
				return jsonResp(http.StatusOK, testCase.body), nil
			})

			req, err := http.NewRequest(http.MethodPost, "https://billing.example/x", nil)
			if err != nil {
				t.Fatal(err)
			}

			_, err = client.doJSONFor(nil, req)

			var uerr *Error
			if !errors.As(err, &uerr) {
				t.Fatalf("err=%v want *Error", err)
			}

			if uerr.Kind != testCase.want {
				t.Errorf("kind=%v want %v", uerr.Kind, testCase.want)
			}

			if !strings.Contains(uerr.Msg, `"code":`) || strings.Contains(uerr.Msg, "code=") {
				t.Errorf("msg=%q 应保留结构化 code，不得拼 code=N 纯文本", uerr.Msg)
			}
		})
	}
}

// TestDoJSONKeepsCodeBeyondTruncation 结构化业务码长度超过 Msg 截断阈值时，
// 消费方仍应能通过 Error.Code 识别（签到/trial 幂等判定依赖它）。
func TestDoJSONKeepsCodeBeyondTruncation(t *testing.T) {
	t.Parallel()

	body := `{"code":14051,"msg":"` + strings.Repeat("x", 250) + `"}`
	client := testClient(func(*http.Request) (*http.Response, error) {
		return jsonResp(http.StatusOK, body), nil
	})

	req, err := http.NewRequest(http.MethodPost, "https://billing.example/x", nil)
	if err != nil {
		t.Fatal(err)
	}

	_, err = client.doJSONFor(nil, req)

	var uerr *Error
	if !errors.As(err, &uerr) {
		t.Fatalf("err=%v want *Error", err)
	}

	if uerr.Code != "14051" {
		t.Errorf("code=%q want 14051", uerr.Code)
	}

	if !trialAlreadyErr(uerr) {
		t.Error("trialAlreadyErr 应通过 Code 识别截断后的 14051")
	}

	if len(uerr.Msg) != 200 {
		t.Errorf("msg len=%d want truncated 200", len(uerr.Msg))
	}
}

// TestFrameKindTopLevelBusiness 顶层 {code,msg} 与 error 帧同口径分类（issue #244）。
func TestFrameKindTopLevelBusiness(t *testing.T) {
	t.Parallel()

	cases := []struct {
		payload string
		want    ErrKind
	}{
		{`{"code":14003,"msg":"model busy"}`, ErrSoftRate},
		{`{"code":11133,"msg":"messages is required","extError":{"param":"messages"}}`, ErrBadParams},
		{`{"code":0,"msg":"ok"}`, ErrNone},
		{`{"code":11129,"msg":"bad tool","choices":[]}`, ErrNone},
	}

	for _, testCase := range cases {
		if got := FrameKind(testCase.payload); got != testCase.want {
			t.Errorf("FrameKind(%s)=%v want %v", testCase.payload, got, testCase.want)
		}
	}
}
