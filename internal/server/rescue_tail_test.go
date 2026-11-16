// rescue_tail_test.go 钉住续写救援闸门的形态分流（issue #570）：
// 无标点承诺句照救、标点型悬停受长度闸门、末句「等用户回话」否决、工具轮按最后
// 一句的收尾词/意图词分流，以及救援上限（预告句两次、结构未闭合一次）。
package server

import (
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
	"github.com/BegoniaHe/wokbuddy-demon/internal/jsonx"
	"github.com/BegoniaHe/wokbuddy-demon/internal/upstream"
)

// sseStopContent 构造「只有一段正文 + stop 终态」的上游流。
func sseStopContent(content string) string {
	raw, _ := jsonx.Marshal(content)

	return `data: {"id":"c1","choices":[{"index":0,"delta":{"content":` + string(raw) + `}}]}` + "\n\n" +
		`data: {"id":"c1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n" +
		"data: [DONE]\n\n"
}

// fakeUpstreamCapturingBodies 返回按 call 序号给响应的假上游，并记录每次请求体
// （续写请求也在内，用于断言续写指令文案）。
func fakeUpstreamCapturingBodies(t *testing.T, reply func(call int) string) (*upstream.Client, *[]string) {
	t.Helper()

	var (
		mu     sync.Mutex
		bodies []string
		calls  atomic.Int32
	)

	client := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			if request.URL.Host == "models.dev" {
				return jsonResponse(http.StatusOK, "{}"), nil
			}

			raw, _ := io.ReadAll(request.Body)

			mu.Lock()

			bodies = append(bodies, string(raw))

			mu.Unlock()

			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader(reply(int(calls.Add(1))))),
			}, nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}

	return client, &bodies
}

func chatBodies(bodies *[]string) []string {
	return *bodies
}

// TestChatPromiseTailNudgeStream 无标点承诺句：救一次，且续写指令是承诺句文案
// （不能说成「停在冒号上」，否则模型会反过来争辩）。
func TestChatPromiseTailNudgeStream(t *testing.T) {
	t.Parallel()

	client, bodies := fakeUpstreamCapturingBodies(t, func(call int) string {
		if call == 1 {
			return sseStopContent("我将开始修改 internal/verify/check.go")
		}

		return sseStopContent("改好了。")
	})
	state := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(&Config{Pool: state, Upstream: client})

	rec := postChat(t, h, `{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}

	got := chatBodies(bodies)
	if len(got) != 2 {
		t.Fatalf("请求数=%d want 2（承诺句救一次）", len(got))
	}

	if !strings.Contains(got[1], "承诺句") {
		t.Errorf("承诺句形态应带承诺句续写指令: %s", got[1])
	}

	if strings.Contains(got[1], "以冒号结尾") {
		t.Errorf("承诺句不应复用标点型悬停指令: %s", got[1])
	}

	if body := rec.Body.String(); !strings.Contains(body, "改好了。") {
		t.Errorf("续写正文应下发: %s", body)
	}
}

// TestChatPunctuatedTailNudgeStream 标点型悬停：续写指令点明「停在冒号上」。
func TestChatPunctuatedTailNudgeStream(t *testing.T) {
	t.Parallel()

	client, bodies := fakeUpstreamCapturingBodies(t, func(call int) string {
		if call == 1 {
			return sseStopContent("我先查一下文件：")
		}

		return sseStopContent("查完了。")
	})
	state := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(&Config{Pool: state, Upstream: client})

	rec := postChat(t, h, `{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}

	got := chatBodies(bodies)
	if len(got) != 2 {
		t.Fatalf("请求数=%d want 2（标点型悬停救一次）", len(got))
	}

	if !strings.Contains(got[1], "以冒号结尾") {
		t.Errorf("标点型悬停应带冒号提示的续写指令: %s", got[1])
	}
}

// TestChatPromiseTailRescuesTwice 连续两段承诺句各救一次，第三次不再救。
func TestChatPromiseTailRescuesTwice(t *testing.T) {
	t.Parallel()

	client, bodies := fakeUpstreamCapturingBodies(t, func(int) string {
		return sseStopContent("我将开始修改 internal/server/handler.go")
	})
	state := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(&Config{Pool: state, Upstream: client})

	rec := postChat(t, h, `{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}

	// 1 次原始请求 + 2 次续写；第三次停在承诺句上按原样收尾。
	if got := len(chatBodies(bodies)); got != 3 {
		t.Fatalf("请求数=%d want 3（每请求最多救两次）", got)
	}

	if !strings.Contains(rec.Body.String(), "internal/server/handler.go") {
		t.Errorf("承诺句正文不应丢失: %s", rec.Body.String())
	}
}

// TestChatStructuralTailRescuesOnce 结构未闭合（未闭合代码块）只补一次：残缺标记
// 留在已输出前缀里，追加正文补不完它。
func TestChatStructuralTailRescuesOnce(t *testing.T) {
	t.Parallel()

	client, bodies := fakeUpstreamCapturingBodies(t, func(int) string {
		return sseStopContent("```go\nfunc main() {")
	})
	state := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(&Config{Pool: state, Upstream: client})

	rec := postChat(t, h, `{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}

	if got := len(chatBodies(bodies)); got != 2 {
		t.Fatalf("请求数=%d want 2（结构未闭合只补一次）", got)
	}
}

// TestChatFinalSentenceYieldNotRescued 末句在等用户拍板：不续写（替用户拍板会拿到
// 工具调用并被采纳，等于替用户执行）。
func TestChatFinalSentenceYieldNotRescued(t *testing.T) {
	t.Parallel()

	for _, stream := range []bool{true, false} {
		t.Run(map[bool]string{true: "stream", false: "non-stream"}[stream], func(t *testing.T) {
			t.Parallel()

			client, bodies := fakeUpstreamCapturingBodies(t, func(int) string {
				return sseStopContent("我已经过了一遍，请确认后我再发送：")
			})
			state := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
			handler := NewHandler(&Config{Pool: state, Upstream: client})

			body := `{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`
			if stream {
				body = `{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`
			}

			rec := postChat(t, handler, body)
			if rec.Code != http.StatusOK {
				t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
			}

			if got := len(chatBodies(bodies)); got != 1 {
				t.Fatalf("请求数=%d want 1（末句在等用户回话，不应续写）", got)
			}
		})
	}
}

// TestChatNonStreamPromiseTailContinued 非流式承诺句：与流式同口径，两段正文拼接。
func TestChatNonStreamPromiseTailContinued(t *testing.T) {
	t.Parallel()

	client, bodies := fakeUpstreamCapturingBodies(t, func(call int) string {
		if call == 1 {
			return sseStopContent("我将开始修改 internal/verify/check.go")
		}

		return sseStopContent("改好了。")
	})
	state := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(&Config{Pool: state, Upstream: client})

	rec := postChat(t, h, `{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}

	got := chatBodies(bodies)
	if len(got) != 2 {
		t.Fatalf("请求数=%d want 2（非流式同样救承诺句）", len(got))
	}

	if !strings.Contains(got[1], "承诺句") {
		t.Errorf("非流式承诺句应带承诺句续写指令: %s", got[1])
	}

	var resp map[string]any
	if err := jsonx.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}

	content, _ := firstChoice(resp)["message"].(map[string]any)["content"].(string)
	if content != "我将开始修改 internal/verify/check.go改好了。" {
		t.Errorf("content=%q want 两段拼接", content)
	}
}

// TestChatToolRoundIntentSentenceRescued 工具轮「句号收尾 + 意图词」仍判没写完：
// 续写只采纳 tool_calls，首段正文保留。
func TestChatToolRoundIntentSentenceRescued(t *testing.T) {
	t.Parallel()

	client, bodies := fakeUpstreamCapturingBodies(t, func(call int) string {
		if call == 1 {
			return sseStopContent("前面三处引用都已经逐个核对过，参数名与返回类型都对得上。我继续检查其余链接。")
		}

		return sseTailToolCall
	})
	state := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(&Config{Pool: state, Upstream: client})

	body := `{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}],` +
		declaredToolsBody + `}`

	rec := postChat(t, h, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}

	if got := len(chatBodies(bodies)); got != 2 {
		t.Fatalf("请求数=%d want 2（工具轮意图句应救一次）", got)
	}

	got := rec.Body.String()
	if !strings.Contains(got, `"exec_command"`) {
		t.Errorf("tool_calls 未下发: %s", got)
	}

	if strings.Contains(got, "不该出现的续写正文") {
		t.Errorf("工具轮续写正文应丢弃: %s", got)
	}
}

// TestChatToolRoundClosingSentenceNotRescued 工具轮收尾词优先：说「全部完成 + 我来总结」
// 是收尾，不救。
func TestChatToolRoundClosingSentenceNotRescued(t *testing.T) {
	t.Parallel()

	client, bodies := fakeUpstreamCapturingBodies(t, func(int) string {
		return sseStopContent("前面三处引用都已经逐个核对过，参数名与返回类型都对得上。任务已全部完成，我来总结一下。")
	})
	state := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(&Config{Pool: state, Upstream: client})

	body := `{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}],` +
		declaredToolsBody + `}`

	rec := postChat(t, h, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}

	if got := len(chatBodies(bodies)); got != 1 {
		t.Fatalf("请求数=%d want 1（收尾词优先，判为答完）", got)
	}
}

// TestChatMidTextYieldMarkerStillRescued 正文中段的「是否需要」是技术叙述，只看末句
// 窗口：尾部停在承诺句上照救。
func TestChatMidTextYieldMarkerStillRescued(t *testing.T) {
	t.Parallel()

	text := "## 三、整改建议\n\n把每一次实测读数归档，为「是否需要更换」提供量化支撑。\n\n---\n\n" +
		"我将开始修改 internal/verify/check.go"

	client, bodies := fakeUpstreamCapturingBodies(t, func(call int) string {
		if call == 1 {
			return sseStopContent(text)
		}

		return sseStopContent("改好了。")
	})
	state := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(&Config{Pool: state, Upstream: client})

	rec := postChat(t, h, `{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}

	if got := len(chatBodies(bodies)); got != 2 {
		t.Fatalf("请求数=%d want 2（中段措辞不否决，尾部承诺句照救）", got)
	}
}

// TestChatPunctuatedTailTooLongNotRescued 标点型悬停超过长度闸门：多半是正常排版
// （说明 + 列表），不救。
func TestChatPunctuatedTailTooLongNotRescued(t *testing.T) {
	t.Parallel()

	text := strings.Repeat("我把三处改动逐行过了一遍，其中两点需要单独说明。", 20) + "改动清单如下："

	client, bodies := fakeUpstreamCapturingBodies(t, func(int) string {
		return sseStopContent(text)
	})
	state := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(&Config{Pool: state, Upstream: client})

	rec := postChat(t, h, `{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}

	if got := len(chatBodies(bodies)); got != 1 {
		t.Fatalf("请求数=%d want 1（超长标点型悬停不救）", got)
	}
}
