// capture.go 取证采样（issue #496）：把「异常收尾形态」的脱敏样本落盘。
//
// 默认关闭——未设置 WB2API_CAPTURE_DIR 时不建目录、不写文件、不打日志，请求行为
// 与不启用取证时完全一致。开启后每个样本一个 JSON 文件：
//
//	<dir>/capture-<UTC 时间戳>-<形态>.json
//
// 样本只含请求形状（模型、是否流式、声明工具数）、脱敏后的响应尾部与判定结果；
// 不写 token / UID / Cookie。目录不可写只告警一次，不影响请求。
package server

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/BegoniaHe/wokbuddy-demon/internal/upstream"
)

const (
	// captureDirEnv 取证采样目录（用户可见的运维开关）。
	captureDirEnv = "WB2API_CAPTURE_DIR"
	// captureTailChars 样本里响应尾部的字符上限（截断后仍能看出收尾形态）。
	captureTailChars = 400
	// captureMaxFiles 目录内样本文件上限：每个样本一个文件，长期开启会把磁盘写满。
	// 超限后滚动删除最旧的样本（只碰本包生成、capture- 前缀 + .json 后缀的文件），
	// 保留最近一段时间的取证窗口。
	captureMaxFiles = 500

	// Detail 键名：调用点与样本读取方共用，避免同一字面量散落各处。
	captureKeyDeclaredTools = "declared_tools"
	captureKeyToolCalls     = "tool_calls"
)

// captureRecord 单个取证样本。Detail 由调用方给，只允许放可公开的形状信息。
type captureRecord struct {
	Time      string         `json:"time"`
	Shape     string         `json:"shape"`
	Model     string         `json:"model"`
	Stream    bool           `json:"stream"`
	TailChars int            `json:"tail_chars"`
	Tail      string         `json:"tail"`
	Detail    map[string]any `json:"detail,omitempty"`
}

// captureWarnOnce 目录/写盘失败只告警一次：取证是运维面，不能刷屏也不能阻断请求。
var captureWarnOnce sync.Once

// reCaptureUID 账号标识的键值形态（uid=... / "uid": "..."）。RedactForLog 不认
// 8 位短 hex，取证样本必须额外抹掉，安全红线是「样本不写 UID」（issue #496）。
var reCaptureUID = regexp.MustCompile(`(?i)("?uid"?\s*[:=]\s*)(?:"[^"]*"|'[^']*'|[^\s"',;}]+)`)

// captureSample 记录一份取证样本。tail 会被 RedactForLog 脱敏并截断；未开启时
// 直接返回（零目录、零文件、零日志）。
func captureSample(shape, model string, stream bool, tail string, detail map[string]any) {
	dir := strings.TrimSpace(os.Getenv(captureDirEnv))
	if dir == "" {
		return
	}

	//nolint:gosec // 目录来自运维显式设置的环境变量，文件名由内置常量与时间戳构成。
	if err := os.MkdirAll(dir, 0o700); err != nil {
		captureWarn(err)

		return
	}

	now := time.Now().UTC()

	record := captureRecord{
		Time:      now.Format(time.RFC3339Nano),
		Shape:     shape,
		Model:     model,
		Stream:    stream,
		TailChars: utf8.RuneCountInString(tail),
		Tail: upstream.RedactForLog(reCaptureUID.ReplaceAllString(tail, `${1}[REDACTED]`),
			captureTailChars),
		Detail: detail,
	}

	raw, err := json.Marshal(record)
	if err != nil {
		captureWarn(err)

		return
	}

	rotateCaptureDir(dir)

	name := "capture-" + now.Format("20060102T150405.000000000") + "-" + captureSlug(shape) + ".json"
	//nolint:gosec // 同上：写盘路径 = 运维指定目录 + 内置形态名 + 时间戳，无外部输入。
	if err := os.WriteFile(filepath.Join(dir, name), append(raw, '\n'), 0o600); err != nil {
		captureWarn(err)
	}
}

// rotateCaptureDir 在写新样本前把目录里的样本数压到上限以下。
// 文件名带纳秒时间戳，字典序即时间序，因此按名字排序删最旧即可。
func rotateCaptureDir(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) <= captureMaxFiles {
		return
	}

	names := make([]string, 0, len(entries))

	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasPrefix(name, "capture-") || !strings.HasSuffix(name, ".json") {
			continue
		}

		names = append(names, name)
	}

	// 未达上限时不删：样本数恰好等于上限也要先腾出一格，否则加上本次写入就超限。
	if len(names) < captureMaxFiles {
		return
	}

	slices.Sort(names)

	for _, name := range names[:len(names)-captureMaxFiles+1] {
		//nolint:gosec // 路径 = 运维指定目录 + 本函数上一步筛出的本包样本名。
		if rmErr := os.Remove(filepath.Join(dir, name)); rmErr != nil {
			captureWarn(rmErr)

			return
		}
	}
}

// captureWarn 取证失败只告警一次。
func captureWarn(err error) {
	captureWarnOnce.Do(func() {
		log.Printf("WARN: [server] capture samples unavailable: %v（取证采样已降级，不影响请求）", err)
	})
}

// captureSlug 把形态名收敛成文件名字符集（调用方给的都是内置常量，仍然兜一层）。
func captureSlug(shape string) string {
	var slug strings.Builder

	for _, r := range strings.ToLower(shape) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_', r == '-':
			slug.WriteRune(r)
		default:
			slug.WriteByte('_')
		}
	}

	if slug.Len() == 0 {
		return "sample"
	}

	return slug.String()
}
