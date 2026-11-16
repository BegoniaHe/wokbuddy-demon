// 积分汇总：把「可用量」与「真实快照」分开计数，供 /status 与面板共用，
// 避免每个消费方各自遍历 accounts 求和并漏掉 disabled 的冻结快照语义。
package pool

// CreditsTotals 一个域（或全池）的积分汇总。
type CreditsTotals struct {
	// Available 可用积分：disabled 账号按 0 计。
	Available int64 `json:"available"`
	// Snapshot 真实快照：含 disabled 账号最后一次成功刷新到的余额。
	Snapshot int64 `json:"snapshot"`
	// Quota 可用账号的总额度；企业版 -1（不限）不计入。
	Quota int64 `json:"quota"`
}

// CreditsSummary 全池积分汇总与分域明细。
type CreditsSummary struct {
	CreditsTotals

	// ByRealm 按域拆分；realm 为空归 cn（与 /healthz 的域口径一致）。
	ByRealm map[string]CreditsTotals `json:"by_realm"`
}

// CreditsSummary 汇总全池积分。禁用号的 Credits 已在 statusOf 归零，这里只做累加。
func (p *Pool) CreditsSummary() CreditsSummary {
	out := CreditsSummary{ByRealm: map[string]CreditsTotals{"cn": {}, "global": {}}}

	list := p.List()
	for i := range list {
		status := &list[i]

		realm := status.Realm
		if realm == "" {
			realm = "cn"
		}

		bucket, ok := out.ByRealm[realm]
		if !ok {
			bucket = CreditsTotals{}
		}

		bucket.Available += status.Credits
		bucket.Snapshot += status.CreditsRaw

		if status.CreditsTotal > 0 {
			bucket.Quota += status.CreditsTotal
		}

		out.ByRealm[realm] = bucket
		out.Available += status.Credits
		out.Snapshot += status.CreditsRaw

		if status.CreditsTotal > 0 {
			out.Quota += status.CreditsTotal
		}
	}

	return out
}
