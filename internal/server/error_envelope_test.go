package server

import (
	"net/http"
	"testing"

	"github.com/BegoniaHe/wokbuddy-demon/internal/upstream"
)

// TestFinalErrorEnvelopeLocalErrorsNoPoolHint 本地分类错误（单行/聚合超限）
// 必须映射 502 upstream_parse，且不能携带号池调度 hint。
func TestFinalErrorEnvelopeLocalErrorsNoPoolHint(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		err  error
	}{
		{"line too long", upstream.ErrSSELineTooLong},
		{"aggregate too long", upstream.ErrSSEAggregateTooLong},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()

			f := &chatFlow{lastErr: c.err}

			status, code, _, hint, upstreamMsgPassed := f.finalErrorEnvelope()
			if status != http.StatusBadGateway || code != "upstream_parse" {
				t.Errorf("status=%d code=%q want 502 upstream_parse", status, code)
			}

			if hint != "" {
				t.Errorf("hint=%q want empty（本地错误不是号池调度失败）", hint)
			}

			if upstreamMsgPassed {
				t.Error("本地错误不得标记为上游原文透传")
			}
		})
	}
}
