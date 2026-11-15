// logs_export.go 运行日志 ZIP 导出（issue #487）。
//
// 只读接口（scopeRead）：按日期区间把落盘的运行日志打包下载。磁盘上的日志在写入
// 时已由 runlog 脱敏，导出原样打包，保证「导出内容与落盘一致」。
package panel

import (
	"archive/zip"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/BegoniaHe/wokbuddy-demon/internal/logrotate"
)

const (
	// logsExportMaxDays 单次导出的最长日期区间（含首尾）。
	logsExportMaxDays = 31
	// logsExportMaxBytes 单次导出的未压缩总量上限。
	logsExportMaxBytes int64 = 64 << 20
)

var (
	// errLogsExportRangeReversed 区间起点晚于终点。
	errLogsExportRangeReversed = errors.New("from must not be after to")
	// errLogsExportRangeTooLong 区间超过上限。
	errLogsExportRangeTooLong = errors.New("date range too long")
	// errLogsExportEmpty 区间内没有落盘文件。
	errLogsExportEmpty = errors.New("no runtime log files in range")
	// errLogsExportTooLarge 选中的文件总量超过上限。
	errLogsExportTooLarge = errors.New("export too large")
)

// logsExport 把指定日期区间的运行日志打包为 ZIP。
// 参数 from/to 支持 unix 秒/毫秒、RFC3339、"2006-01-02T15:04" 与 "2006-01-02"；
// 缺省导出当天。区间超 31 天、总量超 64 MiB 或区间内无文件时明确报错。
func (p *Panel) logsExport(w http.ResponseWriter, r *http.Request) {
	if p.cfg.RuntimeLog == nil || !p.cfg.RuntimeLog.Enabled() {
		writeErr(w, http.StatusNotImplemented, "runtime log sink not enabled")
		return
	}

	from, to, err := parseLogsExportRange(r.URL.Query())
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}

	files, err := p.cfg.RuntimeLog.Files()
	if err != nil {
		// scopeRead 端点：os.ReadDir 的错误串带日志目录绝对路径，不回给调用方。
		log.Printf("WARN: [panel] 运行日志目录不可读: %v", err)
		writeErr(w, http.StatusInternalServerError, "运行日志目录不可读")

		return
	}

	selected, total := selectLogFiles(files, from, to)
	if len(selected) == 0 {
		writeErr(w, http.StatusNotFound, errLogsExportEmpty.Error())
		return
	}

	if total > logsExportMaxBytes {
		writeErr(w, http.StatusRequestEntityTooLarge,
			fmt.Errorf("%w: max %d MiB", errLogsExportTooLarge, logsExportMaxBytes>>20).Error())

		return
	}

	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", fmt.Sprintf(
		`attachment; filename="runtime-logs-%s_%s.zip"`,
		startOfDay(from.In(time.Local)).Format("2006-01-02"),
		startOfDay(to.In(time.Local)).Format("2006-01-02"),
	))

	writeLogZip(w, selected)
}

// parseLogsExportRange 解析并校验导出区间：缺省导出当天，最长 31 天。
func parseLogsExportRange(values url.Values) (time.Time, time.Time, error) {
	to := parseLogsRangeParam(values.Get("to"))
	from := parseLogsRangeParam(values.Get("from"))

	if to.IsZero() {
		to = time.Now()
	}

	if from.IsZero() {
		from = startOfDay(to)
	}

	if from.After(to) {
		return time.Time{}, time.Time{}, errLogsExportRangeReversed
	}

	if to.Sub(from) > logsExportMaxDays*24*time.Hour {
		return time.Time{}, time.Time{},
			fmt.Errorf("%w: max %d days", errLogsExportRangeTooLong, logsExportMaxDays)
	}

	return from, to, nil
}

// selectLogFiles 按文件所属日筛选区间内的文件，返回选中文件与未压缩总量。
// 日归属按**本机时区的自然日**判定：文件名里的日期是写入时的本地日期，参数若是
// 带偏移的 RFC3339（如末尾 Z），必须先换到本地时区再取当日零点——否则时区换算
// 会把当天文件判到区间外（UTC+ 时区的本地零点落在前一天 UTC）。
func selectLogFiles(files []logrotate.File, from, to time.Time) ([]logrotate.File, int64) {
	fromDay := startOfDay(from.In(time.Local))
	toDay := startOfDay(to.In(time.Local))

	selected := make([]logrotate.File, 0, len(files))

	var total int64

	for _, file := range files {
		if file.Day.Before(fromDay) || file.Day.After(toDay) {
			continue
		}

		selected = append(selected, file)
		total += file.Size
	}

	return selected, total
}

// writeLogZip 把选中文件逐个写入 ZIP。
//
// 区分两类失败：本地文件打不开 → 跳过该文件（不影响其余）；写侧失败（客户端断开、
// 连接 reset 是最常见的）→ 立即中止。响应头已随首个字节发出，继续压剩余文件只会
// 让下载方拿到「中央目录缺失但看起来完整」的坏包。
func writeLogZip(w http.ResponseWriter, files []logrotate.File) {
	zipWriter := zip.NewWriter(w)
	// 预算随已写字节递减：单条上限必须是「剩余预算」而不是整次导出的上限。
	// 原先 file.Size<=0（stat 失败 / 正在写入的当天文件）会退化为 logsExportMaxBytes，
	// 单个条目就能吃掉全部预算。
	remaining := logsExportMaxBytes

	for index := range files {
		file := &files[index]

		if remaining <= 0 {
			break
		}

		src, err := os.Open(file.Path)
		if err != nil {
			log.Printf("WARN: [panel] 运行日志导出跳过 %s: %v", file.Name, err)

			continue
		}

		written, err := writeZipEntry(zipWriter, file, src, remaining)
		remaining -= written

		src.Close()

		if err != nil {
			log.Printf("WARN: [panel] 运行日志 ZIP 写入中止: %v", err)

			return
		}
	}

	if err := zipWriter.Close(); err != nil {
		log.Printf("WARN: [panel] 运行日志 ZIP 收尾失败: %v", err)
	}
}

// writeZipEntry 把已打开的文件压入 ZIP（调用方负责关闭 src）。
func writeZipEntry(zipWriter *zip.Writer, file *logrotate.File, src io.Reader, budget int64) (int64, error) {
	entry, err := zipWriter.CreateHeader(&zip.FileHeader{
		Name:     file.Name,
		Method:   zip.Deflate,
		Modified: file.ModTime,
	})
	if err != nil {
		return 0, fmt.Errorf("创建 ZIP 条目: %w", err)
	}

	// 单条上限 = min(stat 时的大小, 剩余预算)：当天文件仍在写入，容量上限是在流式
	// 前算的，不限读会让单条目越过总预算。
	limit := budget
	if file.Size > 0 && file.Size < limit {
		limit = file.Size
	}

	written, err := io.Copy(entry, io.LimitReader(src, limit))
	if err != nil {
		return written, fmt.Errorf("写入 ZIP 条目: %w", err)
	}

	return written, nil
}

// parseLogsRangeParam 解析导出区间参数：在 parseTimeParam 基础上补 "2006-01-02"。
func parseLogsRangeParam(raw string) time.Time {
	if parsed := parseTimeParam(raw); !parsed.IsZero() {
		return parsed
	}

	if day, err := time.ParseInLocation("2006-01-02", strings.TrimSpace(raw), time.Local); err == nil {
		return day
	}

	return time.Time{}
}

// startOfDay 返回同一时区的当日零点。
func startOfDay(moment time.Time) time.Time {
	return time.Date(moment.Year(), moment.Month(), moment.Day(), 0, 0, 0, 0, moment.Location())
}
