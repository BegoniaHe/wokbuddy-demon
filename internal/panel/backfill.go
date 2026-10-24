package panel

import (
	"errors"
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

var (
	errSplitBackfillWindowFailed = errors.New("splitBackfillWindow failed")
	errFetchBackfillPagesFailed  = errors.New("fetchBackfillPages failed")
	errBackfillAccountNotFound   = errors.New("account not found")
)

// usageBackfill 显式触发的上游逐笔用量回填/对账（issue #31）：按账号（缺省全部）
// 拉取最近 N 天的上游用量，按天聚合覆盖本地用量桶。返回记录数 / 覆盖桶数 / 失败汇总。
//
//	POST /panel/api/usage/backfill
//	Body: {"uid":"<可选>","days":7}
//
//nolint:funlen // 请求解析/窗口拆分/逐号回填/汇总线性展开。
func (p *Panel) usageBackfill(w http.ResponseWriter, r *http.Request) {
	var req struct {
		UID  string `json:"uid"`
		Days int    `json:"days"`
	}
	if err := decodeJSONBody(io.LimitReader(r.Body, 1<<20), &req); err != nil {
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
		records, buckets, err := p.backfillOneUID(uid, start, end)
		if err != nil {
			failures[uid] = err.Error()
			continue
		}

		totalRecords += records
		totalBuckets += buckets
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

// backfillOneUID 回填单个账号，返回抓取记录数与覆盖桶数。
func (p *Panel) backfillOneUID(uid string, start, end time.Time) (int, int, error) {
	account := p.cfg.Pool.AuthByUID(uid)
	if account == nil {
		return 0, 0, errBackfillAccountNotFound
	}

	rows, records, err := p.backfillAccount(account, uid, start, end)
	if err != nil {
		log.Printf("WARN: [panel] usage backfill %s: %v", logfmt.Label(account.UID, account.NicknameValue()), err)

		return 0, 0, err
	}

	return records, p.cfg.Usage.Backfill(rows), nil
}

// resolveBackfillUIDs 解析回填目标账号：显式 uid 优先，否则全部池内账号。
func (p *Panel) resolveBackfillUIDs(uid string) []string {
	if u := strings.TrimSpace(uid); u != "" {
		return []string{u}
	}

	var uids []string

	all := p.cfg.Pool.List()
	for i := range all {
		uids = append(uids, all[i].UID)
	}

	return uids
}

const (
	// requestUsagePageSize 单页上限（实测 1000 可用）。
	requestUsagePageSize = 1000
	// requestUsageMaxPages 单个时间窗的分页硬上限；窗口 total < 3000 时最多 3 页。
	requestUsageMaxPages = 50
	// requestUsageResultCap 上游单次时间范围最多返回 3000 条：total 会被截到 3000，
	// 且实测超出部分被丢弃（7 天窗口会漏掉今天的记录）。命中即二分时间窗。
	requestUsageResultCap = 3000
	// requestUsageMinWindow 二分保护：窗口缩到 1 分钟仍撞上限则报错，避免无限递归。
	requestUsageMinWindow = time.Minute
)

// backfillAccount 拉取单账号逐笔用量并按 (天, 模型) 聚合。requestId 跨页/跨窗口
// 去重，避免上游分页重叠或时间窗边界重叠时重复计数。
func (p *Panel) backfillAccount(
	account4 *auth.Auth, uid string, start, end time.Time,
) ([]usage.BackfillRow, int, error) {
	realm := account4.Realm()
	rows := map[string]*usage.BackfillRow{}
	seen := map[string]struct{}{}
	fallbackDay := end.Local().Format("2006-01-02")

	recs, err := p.fetchBackfillRecords(account4, start, end)
	if err != nil {
		return nil, 0, err
	}

	records := 0

	for _, rec := range recs {
		if addBackfillRecord(rows, seen, realm, uid, fallbackDay, rec) {
			records++
		}
	}

	out := make([]usage.BackfillRow, 0, len(rows))
	for _, row := range rows {
		out = append(out, *row)
	}

	return out, records, nil
}

// fetchBackfillRecords 拉取 [start,end] 内全部逐笔记录。
//
// 上游单次时间范围最多返回 3000 条（total 截到 3000，超出部分静默丢弃）；直接查
// 7 天会把最新的今天记录挤掉。命中上限时二分时间窗，直到每个窗口都低于上限。
func (p *Panel) fetchBackfillRecords(
	account *auth.Auth, start, end time.Time,
) ([]upstream.RequestUsageRecord, error) {
	first, err := p.cfg.Upstream.RequestUsage(account, start, end, 1, requestUsagePageSize)
	if err != nil {
		return nil, fmt.Errorf("window %v..%v: %w", start, end, err)
	}

	if first.Total >= requestUsageResultCap {
		return p.splitBackfillWindow(account, start, end)
	}

	return p.fetchBackfillPages(account, start, end, first)
}

// splitBackfillWindow 把命中 3000 条上限的时间窗二分，左右窗口用秒级边界避免重叠。
func (p *Panel) splitBackfillWindow(
	account2 *auth.Auth, start, end time.Time,
) ([]upstream.RequestUsageRecord, error) {
	if end.Sub(start) <= requestUsageMinWindow {
		return nil, fmt.Errorf("window %v..%v exceeds %d-record cap: %w",
			start, end, requestUsageResultCap, errSplitBackfillWindowFailed)
	}

	mid := start.Add(end.Sub(start) / 2).Truncate(time.Second)

	leftEnd := mid.Add(-time.Second)
	if !mid.After(start) || !leftEnd.After(start) || !mid.Before(end) {
		return nil, fmt.Errorf("window %v..%v split failed: %w", start, end, errSplitBackfillWindowFailed)
	}

	left, err := p.fetchBackfillRecords(account2, start, leftEnd)
	if err != nil {
		return nil, err
	}

	right, err := p.fetchBackfillRecords(account2, mid, end)
	if err != nil {
		return nil, err
	}

	return append(left, right...), nil
}

// fetchBackfillPages 在确定 total < 3000 后翻完剩余页。
func (p *Panel) fetchBackfillPages(
	account3 *auth.Auth, start, end time.Time, first *upstream.RequestUsagePage,
) ([]upstream.RequestUsageRecord, error) {
	out := append([]upstream.RequestUsageRecord(nil), first.Records...)

	next := first.NextPage
	for page := 2; next != 0; page++ {
		// 翻到硬上限仍有下一页：宁可失败也不返回截断数据（回填是覆盖写入，
		// 截断会把本地当天桶替换成不完整计数）。
		if page > requestUsageMaxPages {
			return nil, fmt.Errorf("window %v..%v exceeds %d pages: %w",
				start, end, requestUsageMaxPages, errFetchBackfillPagesFailed)
		}

		requestUsagePage, err := p.cfg.Upstream.RequestUsage(account3, start, end, page, requestUsagePageSize)
		if err != nil {
			return nil, fmt.Errorf("window %v..%v page %d: %w", start, end, page, err)
		}

		if len(requestUsagePage.Records) == 0 {
			break
		}

		out = append(out, requestUsagePage.Records...)
		next = requestUsagePage.NextPage
	}

	return out, nil
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
		row = &usage.BackfillRow{Day: day, Realm: realm, UID: uid, Model: rec.Model, Requests: 0, Credits: 0}
		rows[key] = row
	}

	row.Requests++
	row.Credits += rec.Credit

	return true
}
