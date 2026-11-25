package upstream

import (
	"fmt"
	"log"
	"sync"
)

// 同类降级警告的进程内去重（issue #630）。
//
// 模型目录的两路探测（`/v3/config` 与企业端点）只要有一路持续失败，每轮目录拉取
// 都会重打同一行 WARN——缓存过期一轮就重打一遍。降级状态本身在面板可见
// （GlobalCatalogSource），重复 WARN 不带来新信息，只把真故障（两路全挂）
// 淹在同一批重复行里。
//
// 取舍：按 key **永久**去重，即「恢复后再次降级」也不再提示。降级不是需要逐次
// 计数的故障，逐次可见性由每轮结果行（`warm model rates` 的 ok / failed）承担；
// 为此引入状态机与定时重置不值得。
var (
	degradedWarnMu   sync.Mutex
	degradedWarnSeen = make(map[string]struct{}, 8)
)

// warnModelDegraded 记录模型目录的单路降级：点名降了哪一路、降到哪一路，并说明目录
// 仍照常下发。src 是调用方场景（`fetch models` / `global models`），failed/degradedTo
// 是两路探测的名称。同类降级进程内只报一次，见 degradedWarnOnce。
func warnModelDegraded(key, src, failed, degradedTo string, cause error) {
	degradedWarnOnce(key, fmt.Sprintf(
		"[upstream] %s: %s failed (degraded to %s; catalog served normally): %v", src, failed, degradedTo, cause))
}

// degradedWarnOnce 首次见到 key 时打一行 WARN，之后同 key 静默。
//
// msg 由调用方用静态格式串拼好（不经本函数转发格式串）：log.Printf 的格式串
// 必须为常量，才能被 vet 的 printf 检查覆盖。
func degradedWarnOnce(key, msg string) {
	degradedWarnMu.Lock()
	defer degradedWarnMu.Unlock()

	if _, seen := degradedWarnSeen[key]; seen {
		return
	}

	degradedWarnSeen[key] = struct{}{}

	log.Print("WARN: " + msg)
}
