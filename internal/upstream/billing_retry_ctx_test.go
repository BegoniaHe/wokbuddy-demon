package upstream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sync/atomic"
	"testing"
)

var errBillingConnectionReset = errors.New("connection reset")

// TestRetryBillingTransientCanceledStops 取消后的重试必须立即返回 ctx 错误，
// 不再白等退避、不再补打上游（issue #423）：fn 只执行首跳那次。
func TestRetryBillingTransientCanceledStops(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var calls atomic.Int32

	err := (&Client{}).retryBillingTransient(ctx, func() error {
		calls.Add(1)

		return &Error{Kind: ErrServer, Msg: "upstream 500"}
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v want context.Canceled", err)
	}

	if n := calls.Load(); n != 1 {
		t.Fatalf("fn 执行 %d 次，want 1（取消后不得再补打）", n)
	}
}

// TestRetryBillingTransientDeadlineExceededNotTransient 超时同样不是上游抖动；
// context.DeadlineExceeded 自身也实现 net.Error，不能被网络层判定误放行。
func TestRetryBillingTransientDeadlineExceededNotTransient(t *testing.T) {
	t.Parallel()

	if isTransientBillingErr(context.DeadlineExceeded) {
		t.Fatal("context.DeadlineExceeded 不应被判为瞬时错误")
	}

	if isTransientBillingErr(fmt.Errorf("wrap: %w", context.Canceled)) {
		t.Fatal("包装的 context.Canceled 不应被判为瞬时错误")
	}
}

// TestRetryBillingTransientLocalErrorNoRetry JSON 解析这类本地确定性错误不重试：
// 重试只会原样再失败一次并放大无谓出站。
func TestRetryBillingTransientLocalErrorNoRetry(t *testing.T) {
	t.Parallel()

	localErr := fmt.Errorf("resource summary parse: %w", &json.SyntaxError{Offset: 1})
	if isTransientBillingErr(localErr) {
		t.Fatal("本地解析错误不应被判为瞬时错误")
	}

	var calls atomic.Int32

	err := (&Client{}).retryBillingTransient(context.Background(), func() error {
		calls.Add(1)

		return localErr
	})
	if !errors.Is(err, localErr) {
		t.Fatalf("err=%v want 原样返回本地错误", err)
	}

	if n := calls.Load(); n != 1 {
		t.Fatalf("fn 执行 %d 次，want 1（本地错误不重试）", n)
	}
}

// TestRetryBillingTransientNetworkRetried 网络层错误（*url.Error）仍按瞬时限重试。
func TestRetryBillingTransientNetworkRetried(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32

	netErr := &url.Error{Op: "Post", URL: "https://billing.example/v2/report", Err: errBillingConnectionReset}
	if !isTransientBillingErr(netErr) {
		t.Fatal("*url.Error 应被判为瞬时错误")
	}

	err := (&Client{RetryDelay: 1}).retryBillingTransient(context.Background(), func() error {
		if calls.Add(1) == 1 {
			return netErr
		}

		return nil
	})
	if err != nil {
		t.Fatalf("网络抖动应重试成功，err=%v", err)
	}

	if n := calls.Load(); n != 2 {
		t.Fatalf("fn 执行 %d 次，want 2（1 次失败 + 1 次重试）", n)
	}
}
