package redisstore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"testing"
)

var (
	errTestScan = errors.New("scan down")
	errTestMGet = errors.New("mget down")
)

// captureRedisLog 串行捕获 log 输出：调用期间替换标准 logger，结束后恢复。
// 本文件不加 t.Parallel()，避免与其他并行用例争用全局 logger。
func captureRedisLog(t *testing.T) *bytes.Buffer {
	t.Helper()

	var buf bytes.Buffer

	prevOutput := log.Writer()
	prevFlags := log.Flags()

	log.SetOutput(&buf)
	log.SetFlags(0)

	t.Cleanup(func() {
		log.SetOutput(prevOutput)
		log.SetFlags(prevFlags)
	})

	return &buf
}

func bindKeys(count int) []string {
	keys := make([]string, 0, count)
	for i := range count {
		keys = append(keys, fmt.Sprintf("%ssession-%03d", bindPrefix, i))
	}

	return keys
}

// TestLoadBindsBatchesAndSkipsEmpty 100+ 绑定按 100 一批 MGET，空值跳过并剥离前缀；
// 全部批次成功时不产生 WARN（成功汇总仍由 session 输出）。
//
//nolint:paralleltest,cyclop // 串行捕获全局 logger；表驱动断言较多。
func TestLoadBindsBatchesAndSkipsEmpty(t *testing.T) {
	buf := captureRedisLog(t)
	keys := bindKeys(250)

	var batches []int

	scan := func(_ context.Context, cursor uint64) ([]string, uint64, error) {
		if cursor == 0 {
			return keys[:200], 200, nil
		}

		return keys[200:], 0, nil
	}

	mget := func(_ context.Context, batch ...string) ([]any, error) {
		batches = append(batches, len(batch))

		values := make([]any, 0, len(batch))
		for _, key := range batch {
			switch {
			case strings.HasSuffix(key, "-010"):
				values = append(values, "")
			case strings.HasSuffix(key, "-020"):
				values = append(values, nil)
			default:
				values = append(values, "uid:"+strings.TrimPrefix(key, bindPrefix))
			}
		}

		return values, nil
	}

	out := loadBinds(context.Background(), scan, mget)

	if len(out) != 248 {
		t.Fatalf("restored=%d want 248 (empty/nil skipped)", len(out))
	}

	if len(batches) != 3 || batches[0] != 100 || batches[1] != 100 || batches[2] != 50 {
		t.Fatalf("MGET batches=%v want [100 100 50]", batches)
	}

	for key := range out {
		if strings.HasPrefix(key, bindPrefix) {
			t.Fatalf("key %q should be trimmed", key)
		}
	}

	if got := out["session-249"]; got != "uid:session-249" {
		t.Errorf("session-249=%q", got)
	}

	if buf.Len() != 0 {
		t.Fatalf("complete restore should not warn: %s", buf.String())
	}
}

// TestLoadBindsScanFailureWarns SCAN 失败记 WARN，带 discovered/restored 且不泄漏值。
//
//nolint:paralleltest // 串行捕获全局 logger。
func TestLoadBindsScanFailureWarns(t *testing.T) {
	buf := captureRedisLog(t)

	scan := func(_ context.Context, cursor uint64) ([]string, uint64, error) {
		if cursor == 0 {
			return []string{bindPrefix + "a", bindPrefix + "b"}, 1, nil
		}

		return nil, 0, errTestScan
	}

	mget := func(_ context.Context, batch ...string) ([]any, error) {
		values := make([]any, 0, len(batch))
		for range batch {
			values = append(values, "secret-uid")
		}

		return values, nil
	}

	out := loadBinds(context.Background(), scan, mget)
	if len(out) != 2 {
		t.Fatalf("restored=%d want 2", len(out))
	}

	logged := buf.String()
	for _, want := range []string{"WARN: [redisstore] LoadBinds SCAN failed", "discovered=2", "restored=2"} {
		if !strings.Contains(logged, want) {
			t.Errorf("log missing %q: %s", want, logged)
		}
	}

	if strings.Contains(logged, "secret-uid") {
		t.Fatalf("log leaked binding value: %s", logged)
	}
}

// TestLoadBindsBatchFailureWarns 单批 MGET 失败记 WARN 并保留其他批次，最后再报不完整。
//
//nolint:paralleltest // 串行捕获全局 logger。
func TestLoadBindsBatchFailureWarns(t *testing.T) {
	buf := captureRedisLog(t)
	keys := bindKeys(150)

	var call int

	scan := func(context.Context, uint64) ([]string, uint64, error) {
		return keys, 0, nil
	}

	mget := func(_ context.Context, batch ...string) ([]any, error) {
		call++
		if call == 2 {
			return nil, errTestMGet
		}

		values := make([]any, 0, len(batch))
		for range batch {
			values = append(values, "uid")
		}

		return values, nil
	}

	out := loadBinds(context.Background(), scan, mget)
	if len(out) != 100 {
		t.Fatalf("restored=%d want 100", len(out))
	}

	logged := buf.String()
	for _, want := range []string{"MGET batch failed discovered=150 restored=100", "LoadBinds incomplete discovered=150 restored=100"} {
		if !strings.Contains(logged, want) {
			t.Errorf("log missing %q: %s", want, logged)
		}
	}
}

// TestLoadBindsTruncatedWarns MGET 返回值少于请求 key（截断）时记 WARN，
// 只采用实际返回的部分。
//
//nolint:paralleltest // 串行捕获全局 logger。
func TestLoadBindsTruncatedWarns(t *testing.T) {
	buf := captureRedisLog(t)
	keys := bindKeys(150)

	var call int

	scan := func(context.Context, uint64) ([]string, uint64, error) {
		return keys, 0, nil
	}

	mget := func(_ context.Context, batch ...string) ([]any, error) {
		call++

		limit := len(batch)
		if call == 2 {
			limit = 10
		}

		values := make([]any, 0, limit)
		for range limit {
			values = append(values, "uid")
		}

		return values, nil
	}

	out := loadBinds(context.Background(), scan, mget)
	if len(out) != 110 {
		t.Fatalf("restored=%d want 110", len(out))
	}

	logged := buf.String()
	for _, want := range []string{"MGET truncated discovered=150 restored=100", "LoadBinds incomplete discovered=150 restored=110"} {
		if !strings.Contains(logged, want) {
			t.Errorf("log missing %q: %s", want, logged)
		}
	}
}
