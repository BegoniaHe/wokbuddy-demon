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
