package upstream

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
)

// TestChatQuestionPoolShape 断言内嵌题池已被过滤且可用于出站：
// 数量、长度、去重、无品牌字面量，连续抽取不重复。
func TestChatQuestionPoolShape(t *testing.T) {
	t.Parallel()

	if len(chatterQuestions) != 150 {
		t.Fatalf("题池=%d 条 want 150", len(chatterQuestions))
	}

	seen := make(map[string]struct{}, len(chatterQuestions))
	for _, question := range chatterQuestions {
		if question == "" || len(question) > 80 {
			t.Errorf("非法问句 len=%d: %q", len(question), question)
		}

		if strings.Contains(question, "wb2api") || strings.Contains(question, "wb2a") {
			t.Errorf("题池含品牌字面量: %q", question)
		}

		if _, ok := seen[question]; ok {
			t.Errorf("题池重复: %q", question)
		}

		seen[question] = struct{}{}
	}

	prev := pickChatQuestion()
	for range 20 {
		next := pickChatQuestion()
		if next == prev {
			t.Fatalf("连续两次抽到同一问句: %q", next)
		}

		prev = next
	}
}

// TestRunNightChatsUsesQuestionPool 捕获两次夜猫子对话，断言 user 内容来自
// 题池且互不相同（不是固定字面量）。
func TestRunNightChatsUsesQuestionPool(t *testing.T) {
	t.Parallel()

	if testing.Short() {
		t.Skip("RunNightChats 固定 4s 间隔，短测跳过")
	}

	var (
		mu   sync.Mutex
		raws [][]byte
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, chatCompletionsPath) {
			raw, _ := io.ReadAll(r.Body)

			mu.Lock()

			raws = append(raws, raw)
			mu.Unlock()

			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w,
				"data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\n"+
					"data: [DONE]\n\n")

			return
		}

		w.Write([]byte(`{"code":0,"msg":"OK"}`))
	}))
	defer srv.Close()

	client := New()
	client.ChatBaseCN = srv.URL
	client.BillingBaseCN = srv.URL

	if _, err := client.RunNightChats(&auth.Auth{UID: "u-night", AccessToken: "at"}, 2); err != nil {
		t.Fatalf("RunNightChats: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()

	assertPoolContents(t, raws)
}

// TestDesktopChatWithExpertUsesQuestionPool 捕获两次桌面 chat，断言 user 内容
// 来自题池且互不相同。
func TestDesktopChatWithExpertUsesQuestionPool(t *testing.T) {
	t.Parallel()

	var (
		mu   sync.Mutex
		raws [][]byte
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)

		mu.Lock()

		raws = append(raws, raw)
		mu.Unlock()

		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"id\":\"0123456789abcdef0123456789abcdef\"}\n\ndata: [DONE]\n\n")
	}))
	defer srv.Close()

	client := New()
	client.ChatBaseCN = srv.URL
	account := &auth.Auth{UID: "u-desktop", AccessToken: "at"}

	for range 2 {
		if _, _, err := client.DesktopChatWithExpert(account, ""); err != nil {
			t.Fatalf("DesktopChatWithExpert: %v", err)
		}
	}

	mu.Lock()
	defer mu.Unlock()

	assertPoolContents(t, raws)
}

// assertPoolContents 断言捕获的 chat body 中 user 内容均在题池内且两两不同。
func assertPoolContents(t *testing.T, raws [][]byte) {
	t.Helper()

	if len(raws) != 2 {
		t.Fatalf("捕获 %d 条对话 want 2", len(raws))
	}

	pool := make(map[string]struct{}, len(chatterQuestions))
	for _, question := range chatterQuestions {
		pool[question] = struct{}{}
	}

	contents := make([]string, 0, len(raws))
	for _, raw := range raws {
		var body struct {
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Fatalf("chat body 解析失败: %v", err)
		}

		var content string

		for _, m := range body.Messages {
			if m.Role == "user" {
				content = m.Content
				break
			}
		}

		if content == "" {
			t.Fatalf("chat body 无 user 消息: %s", raw)
		}

		if _, ok := pool[content]; !ok {
			t.Errorf("出站 user 内容不在题池内: %q", content)
		}

		contents = append(contents, content)
	}

	if contents[0] == contents[1] {
		t.Errorf("两次对话 user 内容相同: %q", contents[0])
	}
}
