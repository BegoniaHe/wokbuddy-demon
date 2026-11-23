package upstream

import (
	"strings"
	"testing"

	"github.com/BegoniaHe/wokbuddy-demon/internal/jsonx"
)

// TestContinuationInstructionsCarryGatewayPrefix 三种续写指令都带「非用户消息」
// 前缀（issue #606），并且 BuildContinuationBody 把它写进追加的 user 消息。
func TestContinuationInstructionsCarryGatewayPrefix(t *testing.T) {
	t.Parallel()

	kinds := []TailKind{TailNone, TailPunctuated, TailPromise, TailStructural}

	for _, kind := range kinds {
		body := BuildContinuationBody(
			[]byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`), "part", kind)

		var decoded struct {
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := jsonx.Unmarshal(body, &decoded); err != nil {
			t.Fatalf("kind=%v decode: %v", kind, err)
		}

		if len(decoded.Messages) != 3 {
			t.Fatalf("kind=%v messages=%d want 3", kind, len(decoded.Messages))
		}

		last := decoded.Messages[len(decoded.Messages)-1]
		if last.Role != "user" {
			t.Fatalf("kind=%v last role=%q want user", kind, last.Role)
		}

		if !strings.HasPrefix(last.Content, gatewayContinuationPrefix) {
			t.Errorf("kind=%v content=%q want prefix %q", kind, last.Content, gatewayContinuationPrefix)
		}
	}
}
