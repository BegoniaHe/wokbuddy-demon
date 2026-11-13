package atomicwrite

import (
	"errors"
	"os"
	"testing"
	"time"
)

var (
	errTransient = errors.New("transient")
	errBusy      = errors.New("busy")
	errFatal     = errors.New("fatal")
)

func TestMain(m *testing.M) {
	// 单测统一使用近零退避，避免真实等待；生产默认值不受影响。
	renameMaxAttempts, renameBaseDelay, renameMaxDelay = 5, time.Microsecond, 10*time.Microsecond

	os.Exit(m.Run())
}

// shortRetry 保留调用点语义：测试退避参数已由 TestMain 统一调到近零。
func shortRetry(t *testing.T) {
	t.Helper()
}

func TestRenameWithRetrySucceedsAfterTransient(t *testing.T) {
	t.Parallel()
	shortRetry(t)

	transient := errTransient

	var calls int

	doRename := func(string, string) error {
		calls++
		if calls < 3 {
			return transient
		}

		return nil
	}
	if err := renameWithRetry("a", "b", doRename, func(error) bool { return true }); err != nil {
		t.Fatalf("err=%v want nil", err)
	}

	if calls != 3 {
		t.Fatalf("calls=%d want 3", calls)
	}
}

func TestRenameWithRetryExhausted(t *testing.T) {
	t.Parallel()
	shortRetry(t)

	busy := errBusy

	var calls int

	doRename := func(string, string) error { calls++; return busy }

	err := renameWithRetry("a", "b", doRename, func(error) bool { return true })
	if !errors.Is(err, busy) {
		t.Fatalf("err=%v want busy", err)
	}

	if calls != renameMaxAttempts {
		t.Fatalf("calls=%d want %d", calls, renameMaxAttempts)
	}
}

func TestRenameWithRetryNonRetryableStopsImmediately(t *testing.T) {
	t.Parallel()
	shortRetry(t)

	fatal := errFatal

	var calls int

	doRename := func(string, string) error { calls++; return fatal }
	if err := renameWithRetry("a", "b", doRename, func(error) bool { return false }); !errors.Is(err, fatal) {
		t.Fatalf("err=%v want fatal", err)
	}

	if calls != 1 {
		t.Fatalf("calls=%d want 1", calls)
	}
}

// TestRenameRetryStopsAtBudget 累计退避预算为 0 时不得继续重试。
//
// 只有次数上限时，50 次 × 封顶 50ms ≈ 2.3s 睡眠；调用方（auth.SaveAtomic）全程持
// 账号锁，Windows 上一条永久无效路径就会把该账号每次保存都堵这么久。
func TestRenameRetryStopsAtBudget(t *testing.T) { //nolint:paralleltest // 改包级预算，须与其他用例串行
	old := renameRetryBudget

	renameRetryBudget = 0

	t.Cleanup(func() { renameRetryBudget = old })

	calls := 0

	doRename := func(string, string) error {
		calls++

		return errTransient
	}

	err := renameWithRetry("a", "b", doRename, func(error) bool { return true })
	if !errors.Is(err, errTransient) {
		t.Fatalf("err=%v want errTransient", err)
	}

	if calls != 1 {
		t.Fatalf("calls=%d want 1（预算耗尽后不得再退避）", calls)
	}
}
