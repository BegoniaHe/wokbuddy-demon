package reqlog

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestRecorderMetricsAndRecent(t *testing.T) {
	t.Parallel()

	r := New(Config{})
	r.Begin()
	r.Record(&Event{RequestID: "ok", Status: 200, OK: true, Outcome: OutcomeSuccess, DurationMs: 10})
	r.Begin()
	r.Record(&Event{RequestID: "bad", Status: 429, OK: false, Outcome: OutcomeHTTPError, DurationMs: 30})

	s := r.Snapshot()
	assertMetricsCounts(t, s)
	assertMetricsRates(t, s)
	assertRecentNewestFirst(t, s)
}

func assertMetricsCounts(t *testing.T, s Snapshot) {
	t.Helper()

	if s.Completed != 2 || s.InFlight != 0 || s.Succeeded != 1 || s.Failed != 1 {
		t.Fatalf("counts = %+v", s)
	}
}

func assertMetricsRates(t *testing.T, s Snapshot) {
	t.Helper()

	if s.SuccessRate != 50 || s.HTTPSuccessRate != 50 || s.AvgDurationMs != 20 {
		t.Fatalf("rates = success:%v http:%v avg:%v", s.SuccessRate, s.HTTPSuccessRate, s.AvgDurationMs)
	}
}

func assertRecentNewestFirst(t *testing.T, s Snapshot) {
	t.Helper()

	if len(s.Recent) != 2 || s.Recent[0].RequestID != "bad" || s.Recent[1].RequestID != "ok" {
		t.Fatalf("recent = %+v, want newest first", s.Recent)
	}
}

func TestArchiveRotationReadAndFilter(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	r := New(Config{Enabled: true, Dir: dir, FileMaxBytes: 120, MaxBytes: 1 << 20, RetentionDays: 7})

	base := time.Now().Add(-time.Minute)
	for i := range 12 {
		r.Record(&Event{
			Time:       base.Add(time.Duration(i) * time.Second),
			RequestID:  "multi-" + string(rune('a'+i)),
			Model:      "glm-5.3",
			Account:    "账号(uid8)",
			Status:     200,
			OK:         true,
			Outcome:    OutcomeSuccess,
			DurationMs: int64(i + 1),
		})
	}

	r.Record(&Event{
		Time:       base.Add(20 * time.Second),
		RequestID:  "other",
		Model:      "other-model",
		Status:     500,
		Outcome:    OutcomeHTTPError,
		DurationMs: 99,
	})
	r.Close()

	stats := r.Snapshot().Archive
	if !stats.Enabled || stats.Files < 2 || stats.Bytes == 0 || stats.DroppedWrites != 0 {
		t.Fatalf("archive stats = %+v", stats)
	}

	rows, err := r.ReadArchive(5, &Filter{Model: "glm"})
	if err != nil {
		t.Fatal(err)
	}

	if len(rows) != 5 || rows[0].RequestID != "multi-l" || rows[4].RequestID != "multi-h" {
		t.Fatalf("filtered rows = %+v", rows)
	}
}

// ReadArchive 的契约是「按事件时间倒序返回最近记录」，与归档文件的 mtime 无关。
// 快速轮转（FileMaxBytes 很小）时同一秒内会写出多个文件，Linux 下这些文件的 mtime
// 可能完全相同；把归档目录整体拷贝 / 恢复备份也会打乱 mtime。此用例把所有归档文件的
// mtime 改成与事件时间相反的顺序，钉住「只能按事件时间排序」。
func TestReadArchiveOrdersByEventTimeNotFileMtime(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	r := New(Config{Enabled: true, Dir: dir, FileMaxBytes: 1, MaxBytes: 1 << 20, RetentionDays: 7})
	base := time.Now().Add(-time.Hour)

	const n = 5
	for i := range n {
		r.Record(&Event{
			Time:      base.Add(time.Duration(i) * time.Second),
			RequestID: "ev-" + string(rune('a'+i)),
			Model:     "glm-5.3",
			Status:    200,
			OK:        true,
			Outcome:   OutcomeSuccess,
		})
	}

	r.Close()

	// FileMaxBytes=1 让每个事件独占一个归档文件：基准文件 requests-<day>.jsonl 装的是
	// 最早的事件，字典序却排在 requests-<day>.N.jsonl 之后。按 os.ReadDir 顺序递增地
	// 设置 mtime，最早事件所在文件就拿到最大 mtime、被放在最后读取。
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}

	if len(entries) != n {
		t.Fatalf("归档文件数 = %d, want %d", len(entries), n)
	}

	rewriteMtimesAscending(t, dir, entries)

	rows, err := r.ReadArchive(n, &Filter{Model: "glm"})
	if err != nil {
		t.Fatal(err)
	}

	want := make([]string, 0, n)
	for i := range n {
		want = append(want, "ev-"+string(rune('a'+n-1-i)))
	}

	assertRequestOrder(t, requestIDs(rows), want)
}

// rewriteMtimesAscending 按目录项顺序递增地改写文件 mtime，用于打乱「mtime 可还原时间序」的假设。
func rewriteMtimesAscending(t *testing.T, dir string, entries []os.DirEntry) {
	t.Helper()

	stamp := time.Unix(1700000000, 0)
	for i, e := range entries {
		ts := stamp.Add(time.Duration(i) * time.Second)
		if err := os.Chtimes(filepath.Join(dir, e.Name()), ts, ts); err != nil {
			t.Fatal(err)
		}
	}
}

// requestIDs 抽取事件列表的 RequestID，便于断言顺序。
func requestIDs(rows []Event) []string {
	ids := make([]string, 0, len(rows))
	for _, e := range rows {
		ids = append(ids, e.RequestID)
	}

	return ids
}

// assertRequestOrder 断言事件 RequestID 序列与期望逐位相等。
func assertRequestOrder(t *testing.T, got, want []string) {
	t.Helper()

	if len(got) != len(want) {
		t.Fatalf("rows = %v, want %v", got, want)
	}

	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("rows[%d] = %s, want %s\n got %v", i, got[i], want[i], got)
		}
	}
}

// 来源字段随事件落盘并可被 client_ip / user_agent 过滤（包含匹配、大小写不敏感）。
func TestArchiveFilterByClientInfo(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	r := New(Config{Enabled: true, Dir: dir, MaxBytes: 1 << 20, RetentionDays: 7})
	base := time.Now().Add(-time.Minute)

	rows := []Event{
		{
			RequestID: "a", Model: "glm-5.3", Status: 200, OK: true, Outcome: OutcomeSuccess,
			ClientIP: "203.0.113.7", UserAgent: "python-requests/2.31.0",
		},
		{
			RequestID: "b", Model: "glm-5.3", Status: 200, OK: true, Outcome: OutcomeSuccess,
			ClientIP: "198.51.100.4", UserAgent: "Mozilla/5.0 Chrome/120.0.0.0",
		},
		{RequestID: "c", Model: "glm-5.3", Status: 200, OK: true, Outcome: OutcomeSuccess},
	}
	for i, e := range rows {
		e.Time = base.Add(time.Duration(i) * time.Second)
		r.Record(&e)
	}

	r.Close()

	got := readOne(t, r, Filter{ClientIP: "203.0.113."})
	if got.RequestID != "a" {
		t.Fatalf("client ip filter = %+v", got)
	}

	if got.UserAgent != "python-requests/2.31.0" {
		t.Fatalf("user agent not persisted: %+v", got)
	}

	// UA 片段过滤大小写不敏感（面板搜索框直接传用户输入）。
	ua := readOne(t, r, Filter{UserAgent: "chrome/120"})
	if ua.RequestID != "b" {
		t.Fatalf("ua filter = %+v", ua)
	}

	// 来源为空的旧事件不该被非空来源条件误命中。
	readNone(t, r, Filter{ClientIP: "10.0.0.1"})
}

// readOne 读取归档并断言恰好命中一条，返回该条事件。
func readOne(t *testing.T, r *Recorder, filter Filter) Event {
	t.Helper()

	got, err := r.ReadArchive(10, &filter)
	if err != nil {
		t.Fatal(err)
	}

	if len(got) != 1 {
		t.Fatalf("rows = %+v, want 1", got)
	}

	return got[0]
}

// readNone 读取归档并断言无命中。
func readNone(t *testing.T, r *Recorder, filter Filter) {
	t.Helper()

	got, err := r.ReadArchive(10, &filter)
	if err != nil {
		t.Fatal(err)
	}

	if len(got) != 0 {
		t.Fatalf("rows = %+v, want 0", got)
	}
}

// 时间区间过滤（「今天」/「自定义」）：闭区间，任一侧为零值即该侧不设界。
func TestArchiveFilterByTimeRange(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	r := New(Config{Enabled: true, Dir: dir, MaxBytes: 1 << 20, RetentionDays: 30})
	base := time.Now().Add(-72 * time.Hour).Truncate(time.Minute)

	ids := []string{"d3", "d2", "d1", "now"}
	for i, id := range ids {
		r.Record(&Event{
			Time: base.Add(time.Duration(i) * 24 * time.Hour), RequestID: id,
			Status: 200, OK: true, Outcome: OutcomeSuccess,
		})
	}

	r.Close()

	rows, err := r.ReadArchive(10, &Filter{From: base.Add(24 * time.Hour), To: base.Add(48 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	// 倒序返回，闭区间命中 d2、d1。
	assertRequestOrder(t, requestIDs(rows), []string{"d1", "d2"})

	onlyFrom, err := r.ReadArchive(10, &Filter{From: base.Add(48 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}

	assertRequestOrder(t, requestIDs(onlyFrom), []string{"now", "d1"})

	onlyTo, err := r.ReadArchive(10, &Filter{To: base})
	if err != nil {
		t.Fatal(err)
	}

	assertRequestOrder(t, requestIDs(onlyTo), []string{"d3"})

	// 时间区间与其它条件是与关系。
	readNone(t, r, Filter{From: base.Add(24 * time.Hour), To: base.Add(48 * time.Hour), Outcome: OutcomeHTTPError})
}

func TestArchiveQueueDropCounter(t *testing.T) {
	t.Parallel()

	w := &archiveWriter{
		cfg:  Config{Enabled: true, Dir: t.TempDir()},
		ch:   make(chan Event, 1),
		done: make(chan struct{}),
	}
	w.ch <- Event{RequestID: "occupied"}

	w.enqueue(&Event{RequestID: "dropped"})

	if got := w.dropped.Load(); got != 1 {
		t.Fatalf("dropped=%d want 1", got)
	}
}

// TestArchiveEnqueueAfterCloseCountsDropped 关停后写者已退出：入队既不会落盘，
// 也必须计入 dropped——停机窗口的最后一批归档不能静默消失。
func TestArchiveEnqueueAfterCloseCountsDropped(t *testing.T) {
	t.Parallel()

	w := &archiveWriter{
		cfg:  Config{Enabled: true, Dir: t.TempDir()},
		ch:   make(chan Event, 8),
		done: make(chan struct{}),
	}
	// 写者已退出的状态：closed 置位（close() 内部就是这么做的）。
	w.closed.Store(true)

	w.enqueue(&Event{RequestID: "after-close"})

	if got := w.dropped.Load(); got != 1 {
		t.Fatalf("dropped=%d want 1（关停后入队必须计数）", got)
	}

	if len(w.ch) != 0 {
		t.Fatalf("关停后不应再入队, len(ch)=%d", len(w.ch))
	}
}

// TestArchiveToolCounters JSONL 落盘与读回都保留 tools_declared / tool_calls，
// 且 omitempty 生效：未声明 tools 的请求两条都不写（缺失 = 不进分母）。
func TestArchiveToolCounters(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	recorder := writeToolCounterEvents(t, dir)

	rows := readToolCounterRows(t, recorder)

	byID := map[string]Event{}
	for _, row := range rows {
		byID[row.RequestID] = row
	}

	assertToolCounters(t, byID["declared-zero"], 3, 0)
	assertToolCounters(t, byID["declared-called"], 2, 1)
	assertToolCounters(t, byID["no-tools"], 0, 0)
	assertToolCounterJSONL(t, dir)
}

// writeToolCounterEvents 落三条事件：声明零调用、声明有调用、未声明。
func writeToolCounterEvents(t *testing.T, dir string) *Recorder {
	t.Helper()

	recorder := New(Config{Enabled: true, Dir: dir, MaxBytes: 1 << 20, RetentionDays: 7})
	base := time.Now().Add(-time.Minute)

	recorder.Record(&Event{
		Time: base, RequestID: "declared-zero", Status: 200, OK: true, Outcome: OutcomeSuccess,
		ToolsDeclared: 3,
	})
	recorder.Record(&Event{
		Time: base.Add(time.Second), RequestID: "declared-called", Status: 200, OK: true, Outcome: OutcomeSuccess,
		ToolsDeclared: 2, ToolCalls: 1,
	})
	recorder.Record(&Event{
		Time: base.Add(2 * time.Second), RequestID: "no-tools", Status: 200, OK: true, Outcome: OutcomeSuccess,
	})
	recorder.Close()

	return recorder
}

func readToolCounterRows(t *testing.T, recorder *Recorder) []Event {
	t.Helper()

	rows, err := recorder.ReadArchive(10, &Filter{})
	if err != nil {
		t.Fatal(err)
	}

	return rows
}

func assertToolCounters(t *testing.T, event2 Event, declared, calls int) {
	t.Helper()

	if event2.ToolsDeclared != declared || event2.ToolCalls != calls {
		t.Fatalf("%s tools=%d/%d want %d/%d",
			event2.RequestID, event2.ToolsDeclared, event2.ToolCalls, declared, calls)
	}
}

// assertToolCounterJSONL 原始 JSONL：未声明 tools 的行不能出现这两个键（omitempty 是契约）。
func assertToolCounterJSONL(t *testing.T, dir string) {
	t.Helper()

	files, err := filepath.Glob(filepath.Join(dir, "*"))
	if err != nil || len(files) == 0 {
		t.Fatalf("archive files = %v err=%v", files, err)
	}

	raw, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}

	for line := range strings.SplitSeq(strings.TrimSpace(string(raw)), "\n") {
		if strings.Contains(line, "no-tools") && strings.Contains(line, `"tools_declared"`) {
			t.Fatalf("未声明 tools 的行不应写 tools_declared: %s", line)
		}

		if strings.Contains(line, "declared-zero") && !strings.Contains(line, `"tools_declared":3`) {
			t.Fatalf("声明 tools 的行应写 tools_declared=3: %s", line)
		}
	}
}

// TestWriteEventClampsClientFields 客户端可控字段（Model/Path）必须截断：JSON 转义
// 对控制字符是 6 倍膨胀，超长模型名会产出 >1 MiB 单行并撞上读取侧扫描上限。
func TestWriteEventClampsClientFields(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	r := New(Config{Enabled: true, Dir: dir, FileMaxBytes: 1 << 20, MaxBytes: 32 << 20, RetentionDays: 7})

	huge := strings.Repeat("\x01", 300<<10)
	r.Record(&Event{
		Time:      time.Now(),
		RequestID: "long",
		Model:     huge,
		Path:      strings.Repeat("p", 100<<10),
		Status:    200,
		OK:        true,
		Outcome:   OutcomeSuccess,
	})
	r.Close()

	rows, err := r.ReadArchive(10, &Filter{})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}

	if len(rows) != 1 {
		t.Fatalf("rows=%d want 1", len(rows))
	}

	// TruncateEllipsis 会在上限后追加一个省略号，允许这点额外字节。
	budget := maxEventFieldLen + len("…")
	if len(rows[0].Model) > budget || len(rows[0].Path) > budget {
		t.Fatalf("未截断: model=%d path=%d budget=%d", len(rows[0].Model), len(rows[0].Path), budget)
	}
}

// TestReadArchiveToleratesOverlongLine 历史遗留的超长行不能把整个文件判成读取失败。
func TestReadArchiveToleratesOverlongLine(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	r := New(Config{Enabled: true, Dir: dir, FileMaxBytes: 1 << 20, MaxBytes: 32 << 20, RetentionDays: 7})

	r.Record(&Event{
		Time: time.Now(), RequestID: "ok", Model: "m", Status: 200, OK: true,
		Outcome: OutcomeSuccess,
	})
	// 关停排空队列并落盘，之后手工追加一条超过扫描上限的行。
	r.Close()

	files, err := filepath.Glob(filepath.Join(dir, "*.jsonl"))
	if err != nil || len(files) == 0 {
		t.Fatalf("归档文件未生成: %v %v", files, err)
	}

	file, err := os.OpenFile(files[0], os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}

	if _, writeErr := file.WriteString(strings.Repeat("x", 2<<20) + "\n"); writeErr != nil {
		t.Fatal(writeErr)
	}

	if closeErr := file.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}

	rows, err := r.ReadArchive(10, &Filter{})
	if err != nil {
		t.Fatalf("超长行不应让整次读取失败: %v", err)
	}

	if len(rows) == 0 {
		t.Fatal("超长行之前的正常记录应仍可读")
	}
}

// TestRecentRingBufferOrder 环形缓冲必须与原先「prepend + 截断」完全同序：
// 最新在前、超过上限后覆盖最旧一条。
func TestRecentRingBufferOrder(t *testing.T) {
	t.Parallel()

	r := New(Config{})

	total := recentLimit + 7
	for i := range total {
		r.Record(&Event{RequestID: "r" + strconv.Itoa(i), Status: 200, OK: true, Outcome: OutcomeSuccess})
	}

	recent := r.Snapshot().Recent
	if len(recent) != recentLimit {
		t.Fatalf("recent=%d want %d", len(recent), recentLimit)
	}

	// 最新在前、写入序连续递减 1（覆盖环形回绕后的顺序）。
	want := total - 1

	for i := range recent {
		got, err := strconv.Atoi(strings.TrimPrefix(recent[i].RequestID, "r"))
		if err != nil || got != want {
			t.Fatalf("recent[%d]=%q want r%d", i, recent[i].RequestID, want)
		}

		want--
	}
}
