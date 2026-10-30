// sse_line.go 出站 SSE 转写侧共用的切行与 data 行解析：四个协议面（chat /
// responses / anthropic / gemini）与请求统计都要从上游行里取 data 载荷，口径
// 必须一致，故集中在此文件。
package server

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// sseDataPayload 取 SSE "data:" 行的载荷并去掉首尾空白。
//
// SSE 规范允许省略冒号后的空格，上游实测两种形态都出现过；尾随 \r 与空白同样
// 归一到载荷之外。因此各路径统一走这里，不再各自手写 HasPrefix/TrimPrefix。
// 载荷内部空白不受影响（TrimSpace 只作用于两端）；非 data 行返回 ok=false。
func sseDataPayload(line string) (string, bool) {
	payload, ok := strings.CutPrefix(line, "data:")
	if !ok {
		return "", false
	}

	return strings.TrimSpace(payload), true
}

// handleSSELine 单行分派（responses / gemini 两面共用）：上游 SSE 注释行（静默
// 心跳 ": keepalive" 等）原样转发并 flush；data 行按 [DONE] 收尾，其余交 handleChunk。
// 「已结束」短路由调用方先判（各面的 finished 字段语义不同，不在这里推断）。
func handleSSELine(
	w io.Writer, flusher http.Flusher, line string, finish func() error, chunk func(string) error,
) error {
	if strings.HasPrefix(line, ":") {
		if _, err := io.WriteString(w, line+"\n\n"); err != nil {
			return fmt.Errorf("handleLine: %w", err)
		}

		flusher.Flush()

		return nil
	}

	payload, ok := sseDataPayload(line)
	if !ok {
		return nil
	}

	if payload == "[DONE]" {
		return finish()
	}

	return chunk(payload)
}

// writeSSELines 按行拆分 SSE 数据并交给 handleLine，返回本次写入字节数。
// 三个流式转写面（responses / anthropic / gemini）共用：pending 跨 Write 保留
// 未完结的残行，行尾 \r 归一到载荷之外。
func writeSSELines(pending *[]byte, nums []byte, handle func(string) error) (int, error) {
	*pending = append(*pending, nums...)
	for {
		i := bytes.IndexByte(*pending, '\n')
		if i < 0 {
			break
		}

		line := strings.TrimRight(string((*pending)[:i]), "\r")

		*pending = (*pending)[i+1:]

		if err := handle(line); err != nil {
			return 0, err
		}
	}

	return len(nums), nil
}
