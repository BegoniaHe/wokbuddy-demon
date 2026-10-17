// sseread.go SSE 读取的大小上限（issue #113）：单行上限 + 聚合总量上限。
//
// 背景：上游 SSE 解析此前用 bufio.Reader.ReadString('\n') 且无任何上限。上游
// （或被劫持的代理）返回一个超长无换行行、或快速持续发送超大输出时，内存会
// 持续增长——idle_timeout 只限制「空闲」，不限制「持续大数据量」。单个响应即可
// 把网关打到 OOM；非流式还会把完整结果聚合在内存里。
//
// 上限是**防御异常/恶意上游**的硬闸，不是可调行为参数（KISS：不加配置项）：
// 合法 SSE 帧远小于此（正常大模型输出是大量小帧，总量上限按完整回答量级设定）。
package upstream

import (
	"bufio"
	"errors"
	"fmt"
	"strings"
)

// ErrSSELineTooLong 单行超过 MaxSSELineBytes（issue #113）。server 的
// chatStatsReader 与 upstream 的 SSE 读取共用同一哨兵：两侧对「上游单行超大」
// 给出一致错误，任一侧先触发都能让请求以有限内存失败。
var ErrSSELineTooLong = errors.New("sse line exceeds size limit")

// MaxSSELineBytes 单行（一帧）字节上限：4 MiB。bufio.Reader 默认 buffer 64 KiB，
// 超长行靠 ReadString 反复拼接累积；达到上限即停，不再扩容。4 MiB 远超任何
// 合法帧（单帧 delta 通常 < 几 KiB；带 base64 图片的文件帧可到数百 KiB）。
const MaxSSELineBytes = 4 << 20

// maxSSEAggregateBytes 非流式聚合总量上限：32 MiB（content + reasoning +
// tool_call arguments 合计）。一个 chat 回答的完整文本达到 32 MiB 已远超
// 正常量级（约千万级字符），超限即判上游异常。
const maxSSEAggregateBytes = 32 << 20

// errSSEAggregateTooLong 聚合内容超过 maxSSEAggregateBytes。
var errSSEAggregateTooLong = errors.New("sse aggregate exceeds size limit")

// readSSELine 读一行（含 '\n'），单行字节数超过 MaxSSELineBytes 时返回
// ErrSSELineTooLong 而不继续累积内存。语义对齐 bufio.Reader.ReadString：
// 行尾不带 '\n' 时返回该行 + io.EOF；'\n' 结尾时返回 err=nil。
//
// 实现不依赖 ReadString 的字符串拼接：超长行会被 ReadString 反复 concat 成
// 任意大小。这里自己按 chunk 累积，超限立即失败。
func readSSELine(br *bufio.Reader) (string, error) {
	var sb strings.Builder
	for {
		chunk, err := br.ReadSlice('\n')
		if sb.Len()+len(chunk) > MaxSSELineBytes {
			return "", fmt.Errorf("%w: > %d bytes", ErrSSELineTooLong, MaxSSELineBytes)
		}
		sb.Write(chunk)
		if err == nil {
			return sb.String(), nil // 读到 '\n'：完整一行
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue // 行还没完：继续累积（上限由上方的累计检查兜住）
		}
		// io.EOF 或其他读错误：连同已读内容返回，语义同 ReadString。
		return sb.String(), err
	}
}

// limitAggregate 在聚合写入点检查总量上限（content / reasoning / tool_call
// arguments 三条写入路径都经此）。超限返回 errSSEAggregateTooLong，调用方
// 立即中止——不返回残缺成功响应（issue #113）。
type aggregateGuard struct {
	total int
}

// add 累加 n 字节；超限返回错误（已计入的字节不再重要：请求随即失败）。
func (g *aggregateGuard) add(n int) error {
	g.total += n
	if g.total > maxSSEAggregateBytes {
		return fmt.Errorf("%w: > %d bytes", errSSEAggregateTooLong, maxSSEAggregateBytes)
	}
	return nil
}
