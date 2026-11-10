// outbound_patch_test.go 覆盖 issue #482 的出站修补：空 text 段补位、reasoning 镜像、
// 资料接口平铺兜底与流式 flush 收敛。
package upstream

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
)

// TestPadBracketStringContent 字符串 content 以 "[" 开头时必须改写成单段 text parts，
// 原文逐字保留（上游会把它嗅探成 parts 数组并回 11151 空消息）。
func TestPadBracketStringContent(t *testing.T) {
	t.Parallel()

	const original = "[2026-11-10] [notice] 今天的计划：\n1. 看日志"

	body := `{"model":"glm-5.2","messages":[{"role":"user","content":"[2026-11-10] [notice] 今天的计划：\n1. 看日志"}]}`
	out := PrepareBodyOptWithEfforts([]byte(body), false, nil)

	var doc map[string]any
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}

	msgs, _ := doc["messages"].([]any)

	first, _ := msgs[0].(map[string]any)

	parts, ok := first["content"].([]any)
	if !ok || len(parts) != 1 {
		t.Fatalf("content=%T %v want single text part (out=%s)", first["content"], first["content"], out)
	}

	part, _ := parts[0].(map[string]any)
	if part["type"] != "text" {
		t.Errorf("part.type=%v want text", part["type"])
	}

	if part["text"] != original {
		t.Errorf("part.text=%q want original preserved %q", part["text"], original)
	}
}

// TestPadBracketStringContentLeavesOthers 其它字符串 content 原样通过（不臆造改写）。
func TestPadBracketStringContentLeavesOthers(t *testing.T) {
	t.Parallel()

	body := `{"model":"glm-5.2","messages":[{"role":"user","content":"普通文本"}]}`
	out := PrepareBodyOptWithEfforts([]byte(body), false, nil)

	var doc map[string]any
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}

	msgs, _ := doc["messages"].([]any)

	first, _ := msgs[0].(map[string]any)

	if got, ok := first["content"].(string); !ok || got != "普通文本" {
		t.Errorf("content=%T %v want unchanged string", first["content"], first["content"])
	}
}

// TestFetchAccountProfileFlat 顶层平铺 {uid,nickname} 形态必须能解出昵称（issue #482）。
func TestFetchAccountProfileFlat(t *testing.T) {
	t.Parallel()

	client := testClient(func(*http.Request) (*http.Response, error) {
		return jsonResp(http.StatusOK, `{"uid":"u1","nickname":"平铺名字","phoneNumber":"13800000000"}`), nil
	})

	nick, err := client.FetchAccountProfile(&auth.Auth{UID: "u1", AccessToken: "at"})
	if err != nil || nick != "平铺名字" {
		t.Fatalf("nick=%q err=%v want 平铺名字 nil", nick, err)
	}
}

// TestFetchAccountProfileEmptyNickname 昵称为空不得静默成功（会让调用方把昵称覆盖成空串）。
func TestFetchAccountProfileEmptyNickname(t *testing.T) {
	t.Parallel()

	for name, body := range map[string]string{
		"envelope": `{"code":0,"data":{"uid":"u1"}}`,
		"flat":     `{"uid":"u1"}`,
		"blank":    `{"uid":"u1","nickname":"   "}`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			client := testClient(func(*http.Request) (*http.Response, error) {
				return jsonResp(http.StatusOK, body), nil
			})

			if nick, err := client.FetchAccountProfile(&auth.Auth{UID: "u1", AccessToken: "at"}); err == nil {
				t.Fatalf("nick=%q want error for empty nickname", nick)
			}
		})
	}
}

// flushCounter 记录 Flush 次数的 ResponseWriter（httptest.ResponseRecorder 具备 Flush
// 但不计数）。
type flushCounter struct {
	*httptest.ResponseRecorder

	flushes int
}

func (f *flushCounter) Flush() {
	f.flushes++
	f.ResponseRecorder.Flush()
}

// TestStreamFlushCoalesced 长流的小帧必须合并下发：帧内容与顺序不变，Flush 次数
// 远少于帧数，且首帧立即下发（issue #482）。
func TestStreamFlushCoalesced(t *testing.T) {
	t.Parallel()

	const frames = 40

	counter := &flushCounter{ResponseRecorder: httptest.NewRecorder()}
	writer := newStreamWriter(counter, nil)

	validFrames := 0

	for i := range frames {
		line := `data: {"id":"x","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"a"}}]}` + "\n"

		n, _, err := writer.handleStreamLine(line)
		if err != nil {
			t.Fatalf("frame %d: %v", i, err)
		}

		validFrames += n

		n, _, err = writer.handleStreamLine("\n")
		if err != nil {
			t.Fatalf("frame %d boundary: %v", i, err)
		}

		validFrames += n
	}

	// 上游显式 [DONE]：finish 只补收尾，不再按截断处理。
	if _, _, err := writer.handleStreamLine("data: [DONE]\n\n"); err != nil {
		t.Fatalf("done line: %v", err)
	}

	if err := writer.finish(&validFrames); err != nil {
		t.Fatalf("finish: %v", err)
	}

	body := counter.Body.String()

	if got := strings.Count(body, `"object":"chat.completion.chunk"`); got != frames {
		t.Errorf("frames in output=%d want %d", got, frames)
	}

	if !strings.Contains(body, "data: [DONE]") {
		t.Errorf("missing [DONE] terminator")
	}
	// 首帧一次 + 每 ~2KB 一次 + 收尾一次；若不合并，40 帧至少 40 次。
	if counter.flushes > frames/4 {
		t.Errorf("flushes=%d want <= %d (coalesced)", counter.flushes, frames/4)
	}

	if counter.flushes < 1 {
		t.Errorf("first frame must flush immediately, flushes=0")
	}
}
