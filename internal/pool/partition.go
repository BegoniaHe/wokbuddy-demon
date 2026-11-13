// partition.go 号池分区（issue #491）：按请求类型把可用账号分成文本池与媒体池。
//
// 动机：图片/视频请求比文本更容易触发上游按账号的风控与限额；一次画图被判异常
// 会连坐同一个账号上的文本请求。分区让媒体请求只走显式标记的媒体号，文本请求只走
// 其余账号，两侧互不影响。
//
// 口径：
//   - 默认关闭（media_partition=false）时本文件全部逻辑短路，选号行为与之前一致；
//   - 媒体号由配置 upstream... 见 cmd/server/config.go 的 pool.media_accounts 列表；
//   - 分区只影响选号池，不改账号归属、健康台账与持久化状态；
//   - 分区号耗尽时**明确失败**（选号返回 nil → 上层 503 no_healthy_account），
//     不跨区回落：跨区回落等于绕过分区，与动机相反。
package pool

// SetMediaPartition 注入分区开关与媒体号集合（装配期与配置热应用调用）。
// enabled=false 时清空集合，选号完全回到未分区行为。
func (p *Pool) SetMediaPartition(enabled bool, mediaUIDs []string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.mediaPartition = enabled

	if !enabled {
		p.mediaUIDs = nil

		return
	}

	p.mediaUIDs = make(map[string]bool, len(mediaUIDs))
	for _, uid := range mediaUIDs {
		if uid != "" {
			p.mediaUIDs[uid] = true
		}
	}
}

// MediaPartition 报告分区是否启用（只读，供面板/状态展示）。
func (p *Pool) MediaPartition() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()

	return p.mediaPartition
}

// partitionReason 返回账号不满足本次请求分区的排除原因；空 = 放行。
// 调用方必须已持有 p.mu。
func (p *Pool) partitionReason(entry2 *entry, media bool) string {
	if !p.mediaPartition {
		return ""
	}

	if p.mediaUIDs[entry2.a.UID] != media {
		return "partition"
	}

	return ""
}
