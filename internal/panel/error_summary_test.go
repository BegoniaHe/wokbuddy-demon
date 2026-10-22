package panel

import (
	"testing"
	"time"
)

// TestSummarizeErrors 账号 × 类型聚合、计数与最近时间（issue #258）。
func TestSummarizeErrors(t *testing.T) {
	t.Parallel()

	now := time.Now()
	entries := []LogEntry{
		{TS: now.Add(-2 * time.Minute), Text: `WARN: [server] chat acct=示例昵称(u1): session dead after refresh`},
		{TS: now.Add(-90 * time.Second), Text: `WARN: [server] chat acct=u1: session dead again`},
		{TS: now.Add(-time.Minute), Text: `WARN: [server] stream acct=u1 model=m: upstream error frame kind=soft_rate`},
		{TS: now, Text: `checkin 示例昵称(u1): 签到失败: upstream 429 rate limit`},
		{TS: now, Text: `checkin u2: 已签到`},
		{TS: now, Text: `travel u2: skip (traveling record=1)`},
		{TS: now, Text: `ERR: [scheduler] 补偿领取失败 acct=u2: boom`},
	}

	rows := SummarizeErrors(entries)
	if len(rows) != 4 {
		t.Fatalf("rows=%+v", rows)
	}

	if rows[0].Account != "u1" || rows[0].Type != "session_dead" || rows[0].Count != 2 {
		t.Fatalf("top row=%+v want u1/session_dead x2", rows[0])
	}

	for _, row := range rows {
		if row.Sample == "" || row.LastAt.IsZero() {
			t.Fatalf("missing sample/time: %+v", row)
		}
	}
}

// TestExtractErrorAccount 任务前缀与 acct=/uid= 两种账号形态。
func TestExtractErrorAccount(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		`checkin 示例昵称(a1b2c3d4): 失败`:              "a1b2c3d4",
		`WARN: acct=u1 model=m: boom`:             "u1",
		`panel: 任务动作 uid=abcdef12 code=chat_5 失败`: "abcdef12",
		`无账号失败行`:                                  "-",
	}

	for line, want := range cases {
		if got := extractErrorAccount(line); got != want {
			t.Errorf("extractErrorAccount(%q)=%q want %q", line, got, want)
		}
	}
}
