package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/BegoniaHe/wokbuddy-demon/internal/upstream"
	"github.com/BegoniaHe/wokbuddy-demon/internal/usage"
)

// TestRecordSearchUsageBackfillsNamedKey 深检索的 token 必须回填命名密钥累加器，
// 否则按密钥的用量统计与 TPM 会漏掉搜索消耗（与出图路径同口径）。
func TestRecordSearchUsageBackfillsNamedKey(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		usage *upstream.AgenticUsage
		want  [3]int64
	}{
		{"上游给全量", &upstream.AgenticUsage{PromptTokens: 3, CompletionTokens: 4, TotalTokens: 7}, [3]int64{3, 4, 7}},
		{"缺 total 时求和", &upstream.AgenticUsage{PromptTokens: 5, CompletionTokens: 6}, [3]int64{5, 6, 11}},
		{"无 usage 只记请求", nil, [3]int64{0, 0, 0}},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			handler := NewHandler(&Config{Usage: usage.New("")})
			keyUse := &keyUsage{id: "k1"}

			req := httptest.NewRequest(http.MethodPost, "/v1/search/messages", http.NoBody)
			req = req.WithContext(context.WithValue(req.Context(), keyUsageKey{}, keyUse))

			handler.recordSearchUsage(req, "u1", "glm-5.2", time.Now(), testCase.usage, true)

			got := [3]int64{keyUse.prompt, keyUse.completion, keyUse.total}
			if got != testCase.want {
				t.Fatalf("key usage=%v want %v", got, testCase.want)
			}
		})
	}
}
