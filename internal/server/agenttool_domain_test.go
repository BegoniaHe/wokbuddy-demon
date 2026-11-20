package server

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
	"github.com/BegoniaHe/wokbuddy-demon/internal/pool"
	"github.com/BegoniaHe/wokbuddy-demon/internal/upstream"
)

// TestRunAgentToolDomainLevelFailure 15021/15022 是域名/边缘级判定（issue #605）：
// 同一 URL 换号结果一致，本域只打一个号（不轮转）；15022 另标记换域重试。
func TestRunAgentToolDomainLevelFailure(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name           string
		err            error
		wantStatus     int
		wantRetryOther bool
	}{
		{
			"15021 domain routing policy",
			fmt.Errorf("%w (code=15021)", upstream.ErrWebFetchDomainBlocked),
			http.StatusBadRequest,
			false,
		},
		{
			"15022 target unreachable",
			fmt.Errorf("%w (code=15022)", upstream.ErrWebFetchTargetUnreachable),
			http.StatusBadGateway,
			true,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			testPool := pool.New("")
			for _, uid := range []string{"a1", "a2", "a3"} {
				testPool.Add(&auth.Auth{UID: uid, AccessToken: "at", ExpiresAt: 9999999999})
			}

			handler := &Handler{cfg: Config{Pool: testPool}}

			calls := 0

			_, _, fail := runAgentTool(context.Background(), handler, "webfetch", 3, true, "cn",
				func(context.Context, *auth.Auth) (int, error) {
					calls++

					return 0, testCase.err
				})

			if calls != 1 {
				t.Fatalf("upstream calls = %d, want 1 (domain-level failures must not rotate)", calls)
			}

			if fail == nil {
				t.Fatal("fail = nil")
			}

			if fail.status != testCase.wantStatus {
				t.Fatalf("status = %d, want %d", fail.status, testCase.wantStatus)
			}

			if fail.retryOtherRealm != testCase.wantRetryOther {
				t.Fatalf("retryOtherRealm = %v, want %v", fail.retryOtherRealm, testCase.wantRetryOther)
			}
		})
	}
}
