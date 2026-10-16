package atomicwrite

import (
	"errors"
	"testing"
	"time"
)

// shortRetry 把退避参数调到近零，避免单测真实等待。
func shortRetry(t *testing.T) {
	t.Helper()
	oldAttempts, oldBase, oldMax := renameMaxAttempts, renameBaseDelay, renameMaxDelay
	renameMaxAttempts, renameBaseDelay, renameMaxDelay = 5, time.Microsecond, 10*time.Microsecond
	t.Cleanup(func() { renameMaxAttempts, renameBaseDelay, renameMaxDelay = oldAttempts, oldBase, oldMax })
}

func TestRenameWithRetrySucceedsAfterTransient(t *testing.T) {
	shortRetry(t)
	transient := errors.New("transient")
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
	shortRetry(t)
	busy := errors.New("busy")
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
	shortRetry(t)
	fatal := errors.New("fatal")
	var calls int
	doRename := func(string, string) error { calls++; return fatal }
	if err := renameWithRetry("a", "b", doRename, func(error) bool { return false }); !errors.Is(err, fatal) {
		t.Fatalf("err=%v want fatal", err)
	}
	if calls != 1 {
		t.Fatalf("calls=%d want 1", calls)
	}
}
