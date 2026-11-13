// capture_test.go 钉住取证采样开关（issue #496）：未设置时零落盘，设置时只写
// 脱敏样本，目录不可写只告警不阻断。这些用例改写进程级环境变量，必须串行。
package server

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
)

// readOnlyCapture 读目录里唯一的样本文件并解成记录。
func readOnlyCapture(t *testing.T, dir string) captureRecord {
	t.Helper()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}

	if len(entries) != 1 {
		t.Fatalf("capture files=%d want 1", len(entries))
	}

	raw, err := os.ReadFile(filepath.Join(dir, entries[0].Name()))
	if err != nil {
		t.Fatalf("read sample: %v", err)
	}

	var record captureRecord
	if err := json.Unmarshal(raw, &record); err != nil {
		t.Fatalf("decode sample: %v", err)
	}

	return record
}

// TestCaptureDisabledWritesNothing 未设置 WB2API_CAPTURE_DIR 时不产生任何文件。
func TestCaptureDisabledWritesNothing(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(captureDirEnv, dir)

	captureSample("narration_tail", "glm-5.2", true, "tail", nil)

	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 1 {
		t.Fatalf("设置时应有 1 份样本, entries=%d err=%v", len(entries), err)
	}

	// 取消 env 后再写：零新增。
	if err := os.Unsetenv(captureDirEnv); err != nil {
		t.Fatalf("unsetenv: %v", err)
	}

	captureSample("narration_tail", "glm-5.2", true, "tail", nil)

	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 1 {
		t.Fatalf("未设置时不应新增文件, entries=%d err=%v", len(entries), err)
	}
}

// TestCaptureSampleRedactedAndNamed 样本命名可复现，尾部脱敏，不含凭证。
func TestCaptureSampleRedactedAndNamed(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(captureDirEnv, dir)

	tail := "我先查一下文件： Authorization: Bearer sk-live-secret-1234567890 uid=5c162cc9"
	captureSample("narration_tail", "glm-5.2", true, tail, map[string]any{"tools_only": false})

	namePattern := regexp.MustCompile(`^capture-\d{8}T\d{6}\.\d{9}-narration_tail\.json$`)

	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("entries=%d err=%v", len(entries), err)
	}

	if !namePattern.MatchString(entries[0].Name()) {
		t.Errorf("样本文件名不可复现: %s", entries[0].Name())
	}

	raw, err := os.ReadFile(filepath.Join(dir, entries[0].Name()))
	if err != nil {
		t.Fatalf("read sample: %v", err)
	}

	assertNoSecret(t, string(raw), "sk-live-secret", "5c162cc9")

	record := readOnlyCapture(t, dir)
	if record.Shape != "narration_tail" || !record.Stream || record.Model != "glm-5.2" {
		t.Errorf("样本形状不对: %+v", record)
	}

	if record.TailChars == 0 || record.Tail == "" {
		t.Errorf("尾部应保留（脱敏后）: %+v", record)
	}
}

// assertNoSecret 断言样本里没有未脱敏的敏感值。
func assertNoSecret(t *testing.T, raw string, secrets ...string) {
	t.Helper()

	for _, secret := range secrets {
		if strings.Contains(raw, secret) {
			t.Errorf("样本含未脱敏内容 %q: %s", secret, raw)
		}
	}
}

// TestCaptureUnwritableDirDoesNotBlock 目录不可写：只告警，不 panic、不阻断请求。
func TestCaptureUnwritableDirDoesNotBlock(t *testing.T) {
	file := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}

	t.Setenv(captureDirEnv, filepath.Join(file, "capture"))

	captureSample("narration_tail", "glm-5.2", true, "tail", nil)
}

// TestCaptureEndToEndNarration 端到端：预告句续写触发时落一份脱敏样本，
// 响应本身不受影响。
func TestCaptureEndToEndNarration(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(captureDirEnv, dir)

	var calls atomic.Int32

	fake := newFakeUpstream(t, func(string) (int, string, bool) {
		if calls.Add(1) == 1 {
			return http.StatusOK, sseNarrationStop, true
		}

		return http.StatusOK, sseTailText, true
	})
	state := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(&Config{Pool: state, Upstream: fake})

	rec := postChat(t, h, `{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}

	body := rec.Body.String()
	if !strings.Contains(body, "我先查一下文件：") || !strings.Contains(body, "续写正文") {
		t.Errorf("取证开关不应改变响应: %s", body)
	}

	record := readOnlyCapture(t, dir)
	if record.Shape != "narration_tail" {
		t.Errorf("shape=%q want narration_tail", record.Shape)
	}

	if !strings.Contains(record.Tail, "我先查一下文件") {
		t.Errorf("尾部应保留预告句: %+v", record)
	}

	if strings.Contains(record.Tail, "u1") {
		t.Errorf("样本不应含 UID: %+v", record)
	}
}
