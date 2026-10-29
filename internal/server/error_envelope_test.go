package server

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/BegoniaHe/wokbuddy-demon/internal/outbound"
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

// TestFinalErrorEnvelopeProxyDialFailed 代理层失败必须回 502 proxy_dial_failed，
// 且不落回 no_healthy_account（issue #39：运维要能从状态码区分「槽配错」与「号全挂」）。
func TestFinalErrorEnvelopeProxyDialFailed(t *testing.T) {
	t.Parallel()

	f := &chatFlow{lastErr: fmt.Errorf("proxy: %w", outbound.ErrProxyDial)}

	status, code, msg, hint, upstreamMsgPassed := f.finalErrorEnvelope()
	if status != http.StatusBadGateway || code != "proxy_dial_failed" {
		t.Errorf("status=%d code=%q want 502 proxy_dial_failed", status, code)
	}

	if strings.Contains(msg, "no healthy account") {
		t.Errorf("msg=%q 不得表述为号池问题", msg)
	}

	if hint != "" {
		t.Errorf("hint=%q want empty（代理失败不是号池调度失败）", hint)
	}

	if upstreamMsgPassed {
		t.Errorf("本地代理错误不得标记为上游原文透传")
	}
}
