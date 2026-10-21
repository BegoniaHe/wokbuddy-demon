// credithistory.go 面板「积分历史」接口：读本地账本并按需 join 账号昵称。
package panel

import (
	"net/http"
	"strconv"
)

// creditHistoryHandler GET /panel/api/credit_history?limit=&uid=
//
// 返回最近 limit 条余额变动流水（默认 200、上限 1000，钳制在 ledger 内完成），
// uid 非空时只返回该账号。昵称在**读取时**从池快照 join（账本只存 uid，
// 避免账号改名后历史昵称与实际漂移；账号被移除时昵称为空，前端回落短 uid）。
func (p *Panel) creditHistoryHandler(w http.ResponseWriter, r *http.Request) {
	if p.cfg.CreditHistory == nil {
		writeErr(w, http.StatusNotImplemented, "credit history disabled")

		return
	}

	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	uid := r.URL.Query().Get("uid")

	entries := p.cfg.CreditHistory.Recent(limit, uid)

	nicknames := map[string]string{}

	statuses := p.cfg.Pool.List()
	for i := range statuses {
		nicknames[statuses[i].UID] = statuses[i].Nickname
	}

	out := make([]map[string]any, 0, len(entries))
	for _, entry := range entries {
		out = append(out, map[string]any{
			"time":     entry.Time.UTC().Format("2006-01-02T15:04:05Z"),
			"uid":      entry.UID,
			"nickname": nicknames[entry.UID],
			"delta":    entry.Delta,
			"remain":   entry.Remain,
			"note":     entry.Note,
		})
	}

	writeJSON(w, http.StatusOK, map[string]any{"entries": out})
}
