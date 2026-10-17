package panel

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
	"github.com/BegoniaHe/wokbuddy-demon/internal/logfmt"
	"github.com/BegoniaHe/wokbuddy-demon/internal/upstream"
	"github.com/BegoniaHe/wokbuddy-demon/internal/usage"
)

// usageBackfill 显式触发的上游逐笔用量回填/对账（issue #31）：按账号（缺省全部）
// 拉取最近 N 天的上游用量，按天聚合覆盖本地用量桶。返回记录数 / 覆盖桶数 / 失败汇总。
//
//	POST /panel/api/usage/backfill
//	Body: {"uid":"<可选>","days":7}
func (p *Panel) usageBackfill(w http.ResponseWriter, r *http.Request) {
	var req struct {
		UID  string `json:"uid"`
		Days int    `json:"days"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	if p.cfg.Usage == nil {
		writeErr(w, http.StatusServiceUnavailable, "用量记录未启用")
		return
	}
	if req.Days <= 0 || req.Days > 90 {
		req.Days = 7
	}
	uids := p.resolveBackfillUIDs(req.UID)
	if len(uids) == 0 {
		writeErr(w, http.StatusBadRequest, "无可回填账号")
		return
	}

	end := time.Now()
	start := end.AddDate(0, 0, -req.Days)
	var totalRecords, totalBuckets int
	failures := map[string]string{}
	for _, uid := range uids {
		a := p.cfg.Pool.AuthByUID(uid)
		if a == nil {
			failures[uid] = "account not found"
			continue
		}
		rows, records, err := p.backfillAccount(a, uid, start, end)
		if err != nil {
			failures[uid] = err.Error()
			log.Printf("WARN: usage backfill %s: %v", logfmt.Label(a.UID, a.Nickname), err)
			continue
		}
		totalRecords += records
		totalBuckets += p.cfg.Usage.Backfill(rows)
	}
	p.cfg.Usage.Save()
	log.Printf("panel: usage backfill accounts=%d records=%d buckets=%d failures=%d",
		len(uids), totalRecords, totalBuckets, len(failures))
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"accounts": len(uids),
		"records":  totalRecords,
		"buckets":  totalBuckets,
		"failures": failures,
	})
}

// resolveBackfillUIDs 解析回填目标账号：显式 uid 优先，否则全部池内账号。
func (p *Panel) resolveBackfillUIDs(uid string) []string {
	if u := strings.TrimSpace(uid); u != "" {
		return []string{u}
	}
	var uids []string
	for _, st := range p.cfg.Pool.List() {
		uids = append(uids, st.UID)
	}
	return uids
}

// requestUsagePageSize 单页上限（实测 1000 可用；配合 50 页上限即最多 5 万条/账号）。
const requestUsagePageSize = 1000

// backfillAccount 拉取单账号逐笔用量并按 (天, 模型) 聚合。分页上限 50 页防失控；
// requestId 跨页去重，避免上游分页重叠时重复计数。
func (p *Panel) backfillAccount(a *auth.Auth, uid string, start, end time.Time) ([]usage.BackfillRow, int, error) {
	realm := a.Realm()
	rows := map[string]*usage.BackfillRow{}
	seen := map[string]struct{}{}
	fallbackDay := end.Local().Format("2006-01-02")
	records := 0
	for page := 1; page <= 50; page++ {
		pg, err := p.cfg.Upstream.RequestUsage(a, start, end, page, requestUsagePageSize)
		if err != nil {
			return nil, records, fmt.Errorf("page %d: %w", page, err)
		}
		newOnPage := 0
		for _, rec := range pg.Records {
			if addBackfillRecord(rows, seen, realm, uid, fallbackDay, rec) {
				records++
				newOnPage++
			}
		}
		if len(pg.Records) == 0 || newOnPage == 0 || pg.NextPage == 0 {
			break
		}
	}
	out := make([]usage.BackfillRow, 0, len(rows))
	for _, row := range rows {
		out = append(out, *row)
	}
	return out, records, nil
}

// addBackfillRecord 把单笔记录聚合进 (天, 模型) 桶；requestId 重复返回 false。
// 时间缺失的异常记录落到 fallbackDay，保证不丢请求数。
func addBackfillRecord(
	rows map[string]*usage.BackfillRow,
	seen map[string]struct{},
	realm, uid, fallbackDay string,
	rec upstream.RequestUsageRecord,
) bool {
	if rec.RequestID != "" {
		if _, ok := seen[rec.RequestID]; ok {
			return false
		}
		seen[rec.RequestID] = struct{}{}
	}
	day := fallbackDay
	if !rec.Time.IsZero() {
		day = rec.Time.Local().Format("2006-01-02")
	}
	key := day + "|" + rec.Model
	row := rows[key]
	if row == nil {
		row = &usage.BackfillRow{Day: day, Realm: realm, UID: uid, Model: rec.Model}
		rows[key] = row
	}
	row.Requests++
	row.Credits += rec.Credit
	return true
}
