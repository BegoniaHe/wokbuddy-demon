// taskcenter.go 面板「任务中心」：全账号任务扫描 + 执行队列（可配并发）+
// 成长任务队列。
//
// 语义：
//   - 扫描（scan_all）：并发拉取每账号的成长任务列表（默认+小程序口径），
//     汇总出"未完成且可自动化"的待办清单（只读，不执行）。
//   - 执行队列（run_queue + queue）：把待办项按账号分组排队执行——账号内
//     串行（复用 per-account 锁，与单任务/一键完成互斥），账号间并发
//     （concurrency 信号量限制，默认 1）。队列状态可轮询。
package panel

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
	"github.com/BegoniaHe/wokbuddy-demon/internal/upstream"
)

// ---------------------------------------------------------------------------
// 扫描（只读）
// ---------------------------------------------------------------------------

var errRunGrowthQueuedFailed = errors.New("runGrowthQueued failed")

// scanAccountItem 单账号扫描结果。
type scanAccountItem struct {
	UID       string          `json:"uid"`
	Nickname  string          `json:"nickname"`
	Growth    []upstream.Task `json:"growth,omitempty"`
	GrowthErr string          `json:"growth_error,omitempty"`
}

// growthPending 任务是否"未完成且可自动化"。
func growthPending(t *upstream.Task) bool {
	if t.Claimed {
		return false
	}
	// 上游锁定的任务不出待办：Sequential 族每日零点解锁一环，刚做完上一环时
	// 下一环以下发但 locked 形态出现在列表里——扫进队列只会 accept 不落账报
	// 失败（每日锁定窗口），零点解锁后自然回到待办。其余 locked（上游未开放）
	// 同语义：不该被自动化尝试。
	if t.Locked {
		return false
	}

	if t.Target > 0 && t.Current >= t.Target {
		// 达标未领：也入队（队列执行后会自动领）——但仅限有自动化动作的任务，
		// 否则队列执行时会因 autoActionFor 为 nil 直接报错。
		return autoActionFor(t.TaskCode) != nil
	}

	return autoActionFor(t.TaskCode) != nil
}

// scanWorkers 扫描 / 队列收集时并发上游调用的上限。导入上限是 1000 个账号，
// 无界 goroutine 会瞬时打满上游限流；与批量导入后置任务（importWorkers）同量级。
const scanWorkers = 5

// tasksScanAll 扫描全部账号：成长任务（未完成+可自动化，含 mp 口径合并）。
// 只读操作，并发拉取（受 scanWorkers 约束）。
func (p *Panel) tasksScanAll(w http.ResponseWriter, r *http.Request) {
	states := p.cfg.Pool.List()
	items := make([]scanAccountItem, len(states))

	sem := make(chan struct{}, scanWorkers)

	var wg sync.WaitGroup

	for i := range states {
		status := &states[i]
		if status.Disabled {
			continue
		}

		wg.Add(1)
		go func(i int, uid string) {
			defer wg.Done()

			sem <- struct{}{}
			defer func() { <-sem }()

			account := p.cfg.Pool.AuthByUID(uid)
			if account == nil {
				return
			}

			items[i].UID, items[i].Nickname = uid, account.NicknameValue()
			items[i].Growth, items[i].GrowthErr = p.scanAccountGrowth(account)
		}(i, status.UID)
	}

	wg.Wait()

	pending := 0
	for _, it := range items {
		pending += len(it.Growth)
	}

	log.Printf("panel: 队列扫描完成：全部账号待办 %d 项", pending)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "accounts": items, "pending_count": pending})
}

// scanAccountGrowth 拉取单账号待办成长任务（默认口径 + mp 口径合并，不排序）。
// global/enterprise 门控；默认列表失败返回错误文本（此时仍合并 mp 待办）。
func (p *Panel) scanAccountGrowth(account4 *auth.Auth) ([]upstream.Task, string) {
	// D4/企业版门控：global 与企业版无 CN 个人成长任务体系，不发起任何上游调用。
	if account4.IsGlobal() || account4.IsEnterprise() {
		return nil, ""
	}

	var grow []upstream.Task

	growthErr := ""
	if tasks, err := p.cfg.Upstream.ListTasks(account4); err != nil {
		growthErr = err.Error()
	} else {
		for i := range tasks {
			if growthPending(&tasks[i]) {
				grow = append(grow, tasks[i])
			}
		}
	}
	// 小程序口径任务（school_season 校园日 / Sequential_Tasks_1 小程序首对话）
	// 仅在 mp 头列表下发，与默认口径不重叠——合并进待办列表；mp 列表失败
	// 静默（无 mp 任务的部署/活动结束时零影响）。
	if mpTasks, err := p.cfg.Upstream.ListTasksMP(account4); err == nil {
		grow = mergeMPPending(grow, mpTasks)
	}

	return grow, growthErr
}

// mergeMPPending 把 mp 口径待办合并进 grow（按 code 去重；grow 已含的跳过）。
// 注意：mp 列表内部重复 code 不会被二次去重（与既有扫描/队列口径一致）。
func mergeMPPending(grow, mpTasks []upstream.Task) []upstream.Task {
	seen := map[string]bool{}
	for i := range grow {
		seen[grow[i].TaskCode] = true
	}

	for i := range mpTasks {
		if growthPending(&mpTasks[i]) && !seen[mpTasks[i].TaskCode] {
			grow = append(grow, mpTasks[i])
		}
	}

	return grow
}

// ---------------------------------------------------------------------------
// 执行队列
// ---------------------------------------------------------------------------

// queueItem 队列执行单元。
type queueItem struct {
	UID      string `json:"uid"`
	Nickname string `json:"nickname"`
	Kind     string `json:"kind"` // growth
	Code     string `json:"code"`
	Status   string `json:"status"` // pending | running | done | skipped | error
	Message  string `json:"message,omitempty"`
}

// queueState 队列运行状态。Seq 每次启动 +1——前端只渲染"自己启动的那一轮"，
// 执行结束后的残留 items 不会覆盖后续的扫描结果视图。
type queueState struct {
	mu        sync.Mutex
	running   bool
	startedAt time.Time
	items     []queueItem
	conc      int
	seq       int
}

// Panel 队列字段在 Panel 结构体上（panel.go）由 initQueue 惰性初始化；
// 这里集中访问器，避免改动 New 构造链。
func (p *Panel) queue() *queueState {
	p.queueOnce.Do(func() { p.q = &queueState{} })
	return p.q
}

// tasksRunQueue 启动执行队列：{concurrency:1-4, growth:bool, school:bool}。
// 先做一次扫描，把全部待办项排队（growth 按账号内 autoActions 顺序执行，
// school 逐账号跑闭环），账号内串行、账号间受并发信号量约束。
func (p *Panel) tasksRunQueue(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Concurrency int  `json:"concurrency"`
		Growth      bool `json:"growth"`
	}

	_ = json.NewDecoder(r.Body).Decode(&body)
	if !body.Growth {
		body.Growth = true
	}

	if body.Concurrency < 1 {
		body.Concurrency = 1
	}

	if body.Concurrency > 4 {
		body.Concurrency = 4
	}

	started, total, seq, msg := p.startGrowthQueue(body.Concurrency, body.Growth)
	switch {
	case seq == -1:
		writeErr(w, http.StatusConflict, msg)
	case !started:
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "started": false, "message": msg})
	default:
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "started": true, "total": total, "seq": seq})
	}
}

// startGrowthQueue 扫描全部账号待办并启动队列（HTTP「执行全部待办」与调度器
// growth 时点共用核心）。返回 (started, total, seq, msg)：seq==-1 表示队列
// 已在执行（冲突）；started=false 时 msg 为无可执行待办的说明。并发夹取
// [1,4]；growth 开关同 HTTP 入参语义。
func (p *Panel) startGrowthQueue(concurrency int, growth bool) (started bool, total, seq int, msg string) {
	if concurrency < 1 {
		concurrency = 1
	}

	if concurrency > 4 {
		concurrency = 4
	}

	queueState2 := p.queue()
	queueState2.mu.Lock()
	if queueState2.running {
		queueState2.mu.Unlock()
		return false, 0, -1, "队列正在执行中（可在任务中心查看进度）"
	}
	// 先占位：扫描（数秒级网络耗时）期间若并发再次触发，直接命中上面的 running
	// 判拒，避免两个 goroutine 同时启动互相覆盖 q.items/q.seq。无待办时回滚。
	queueState2.running = true
	queueState2.startedAt = time.Now()
	queueState2.mu.Unlock()

	accts := p.collectQueueAccounts(growth)

	// 组装队列（账号分组，保持顺序）。
	var items []queueItem

	for _, one := range accts {
		for j := range one.grow {
			items = append(items, queueItem{
				UID: one.a.UID, Nickname: one.a.NicknameValue(),
				Kind: "growth", Code: one.grow[j].TaskCode, Status: "pending",
				Message: "",
			})
		}
	}

	if len(items) == 0 {
		log.Printf("panel: 队列启动：无可执行待办（全部账号任务已完成）")
		queueState2.mu.Lock()
		queueState2.running = false
		queueState2.startedAt = time.Time{}
		queueState2.mu.Unlock()

		return false, 0, 0, "全部账号没有待办任务"
	}

	queueState2.mu.Lock()
	queueState2.items = items
	queueState2.conc = concurrency
	queueState2.seq++
	seq = queueState2.seq
	queueState2.mu.Unlock()

	go p.runQueueItems(accts, items, concurrency)

	log.Printf("panel: 队列启动：%d 项（并发 %d，成长 %v）", len(items), concurrency, growth)

	return true, len(items), seq, ""
}

// collectQueueAccounts 并发拉取各账号待办成长任务（复用扫描逻辑的拉取部分），
// 返回待办非空的账号（保持 Pool.List 的原顺序）。growth=false 时不做上游拉取。
func (p *Panel) collectQueueAccounts(growth bool) []queueAccount {
	states := p.cfg.Pool.List()

	// 按下标写回：并发拉取完成顺序不确定，不能直接 append，否则队列账号
	// 顺序随 goroutine 调度漂移（与「保持原顺序」契约不符）。
	results := make([]*queueAccount, len(states))

	sem := make(chan struct{}, scanWorkers)

	var wg sync.WaitGroup

	for i := range states {
		st := &states[i]
		if st.Disabled {
			continue
		}

		account := p.cfg.Pool.AuthByUID(st.UID)
		if account == nil {
			continue
		}

		wg.Add(1)
		go func(idx int, queueAcct *auth.Auth) {
			defer wg.Done()

			sem <- struct{}{}
			defer func() { <-sem }()

			one := queueAccount{a: queueAcct}
			if growth {
				one.grow = p.queueAccountGrowth(queueAcct)
			}

			if len(one.grow) > 0 {
				results[idx] = &one
			}
		}(i, account)
	}

	wg.Wait()

	accts := make([]queueAccount, 0, len(states))

	for _, one := range results {
		if one != nil {
			accts = append(accts, *one)
		}
	}

	return accts
}

// queueAccountGrowth 拉取账号待办成长任务并按 autoActions 顺序排序。global/企业版
// 门控；默认列表失败则整账号跳过（与扫描口径不同：扫描失败仍合并 mp 待办）。
func (p *Panel) queueAccountGrowth(account2 *auth.Auth) []upstream.Task {
	if account2.IsGlobal() || account2.IsEnterprise() {
		return nil
	}

	tasks, err := p.cfg.Upstream.ListTasks(account2)
	if err != nil {
		return nil
	}

	var grow []upstream.Task

	for i := range tasks {
		if growthPending(&tasks[i]) {
			grow = append(grow, tasks[i])
		}
	}
	// 合并小程序口径待办（与 tasksScanAll 同口径：mp 列表是默认口径超集，按 code
	// 去重；失败静默）。此前此处漏合并——扫描显示 mp 待办而队列报"无可执行待办"。
	if mpTasks, mpErr := p.cfg.Upstream.ListTasksMP(account2); mpErr == nil {
		grow = mergeMPPending(grow, mpTasks)
	}

	sort.Slice(grow, func(i, j int) bool { // 按 autoActions 顺序（依赖前置）
		return autoActionIndex(grow[i].TaskCode) < autoActionIndex(grow[j].TaskCode)
	})

	return grow
}

// RunGrowthQueueOnce 调度器 growth 时点回调（sch.SetGrowthHook 挂载）：与
// 「执行全部待办」按钮完全同管线（成长，串行并发 1）。Sequential 族
// 每日零点解锁一环，此前只能手动扫描推进；此回调让链条每天自动走一环。
// 异步执行（startGrowthQueue 启动 goroutine 即返），已在跑/无待办安全跳过。
func (p *Panel) RunGrowthQueueOnce() {
	started, total, _, _ := p.startGrowthQueue(1, true)
	if started {
		log.Printf("panel: 定时成长任务队列已启动（%d 项）", total)
	}
}

// runQueueItems 队列执行主体：按账号分组，账号内串行（per-account 锁），
// 账号间并发（信号量）。每项结果写回队列状态。
func (p *Panel) runQueueItems(accts []queueAccount, items []queueItem, concurrency int) {
	queueState2 := p.queue()
	defer func() {
		queueState2.mu.Lock()
		queueState2.running = false
		queueState2.mu.Unlock()
		log.Printf("panel: 队列执行结束（共 %d 项）", len(items))
	}()

	sem := make(chan struct{}, concurrency)

	var wg sync.WaitGroup
	for _, one := range accts {
		wg.Add(1)
		go func(one queueAccount) {
			defer wg.Done()

			sem <- struct{}{}
			defer func() { <-sem }()
			// per-account 互斥：与单任务/一键完成共用一把锁。
			if !p.tryLockAccount(one.a.UID) {
				p.queueSet(queueState2, one.a.UID, func(it *queueItem) {
					it.Status, it.Message = "skipped", "该账号有其它任务动作在执行，跳过"
				})

				return
			}
			defer p.unlockAccount(one.a.UID)
			// 前置：批量接受尚未接受的任务。上游对 not_accepted 的任务不计数——
			// 面板「一键完成」一直有这步，队列路径此前漏了（表现为上报 200 但进度
			// 一直 not_accepted、无法领奖）。失败不阻塞（行为事件才是进度判据）。
			if accepted := p.acceptPendingTasks(one.a); accepted > 0 {
				time.Sleep(reportGap) // 给上游状态流转留时间
			}

			for i := range queueState2.items {
				uid, kind, code := queueState2.snapshotAt(i)
				if uid != one.a.UID {
					continue
				}

				p.queueMarkAt(i, "running", "")

				var (
					msg string
					err error
				)
				if kind == "growth" {
					msg, err = p.runGrowthQueued(one.a, code)
				}

				if err != nil {
					p.queueMarkAt(i, "error", err.Error())
				} else {
					p.queueMarkAt(i, "done", msg)
				}

				time.Sleep(reportGap) // 项间节流
			}
		}(one)
	}

	wg.Wait()
}

// queueAccount 队列执行的账号单元（runQueueItems 参数）。
type queueAccount struct {
	a    *auth.Auth
	grow []upstream.Task
}

// snapshotAt 锁内读条目三元组（避免锁外持有指针）。
func (q *queueState) snapshotAt(i int) (uid, kind, code string) {
	q.mu.Lock()
	defer q.mu.Unlock()

	return q.items[i].UID, q.items[i].Kind, q.items[i].Code
}

// queueMarkAt 按索引更新队列条目状态（条目数组固定不再增删）。
func (p *Panel) queueMarkAt(i int, status, msg string) {
	q := p.queue()
	q.mu.Lock()
	q.items[i].Status, q.items[i].Message = status, msg
	q.mu.Unlock()
}

// queueSet 按 uid 批量改状态。
func (p *Panel) queueSet(queueState2 *queueState, uid string, fn func(*queueItem)) {
	queueState2.mu.Lock()
	defer queueState2.mu.Unlock()

	for i := range queueState2.items {
		if queueState2.items[i].UID == uid {
			fn(&queueState2.items[i])
		}
	}
}

// acceptPendingTasks 批量接受该账号未接受的任务，返回接受的个数（失败返回 0 不阻塞）。
func (p *Panel) acceptPendingTasks(account *auth.Auth) int {
	tasks, err := p.cfg.Upstream.ListTasks(account)
	if err != nil {
		return 0
	}

	var codes []string

	for i := range tasks {
		if t := &tasks[i]; !t.Claimed && !t.Locked && t.AcceptStatus != "accepted" && t.AcceptStatus != "completed" {
			codes = append(codes, t.TaskCode)
		}
	}

	if len(codes) == 0 {
		return 0
	}

	if err := p.cfg.Upstream.AcceptTasks(account, codes); err != nil {
		log.Printf("panel: 队列 accept uid=%s: %v（不阻塞）", account.UID, err)
		return 0
	}

	log.Printf("panel: 队列 accept uid=%s: 已接受 %d 个任务", account.UID, len(codes))

	return len(codes)
}

// runGrowthQueued 执行单个成长任务（动作 + 回读 + 自动领奖；与
// accountTaskAuto 同语义，结果以文字返回）。
func (p *Panel) runGrowthQueued(account3 *auth.Auth, code string) (string, error) {
	act := autoActionFor(code)
	if act == nil {
		return "", fmt.Errorf("任务 %s 无自动动作: %w", code, errRunGrowthQueuedFailed)
	}
	// taskByCode 已双口径（mp 专属码自动回落 mp 列表）。
	before, err := p.taskByCode(account3, code)
	if err != nil {
		return "", err
	}

	if before == nil {
		return "该账号无此任务", nil
	}

	if before.Claimed {
		return "已完成（已领取）", nil
	}

	msg, err := act.run(p, account3)
	if err != nil {
		return "", err
	}

	after, _ := p.taskByCodeAfterAction(account3, code)
	if after != nil && after.Claimable {
		msg = p.autoClaimQueued(account3, code, msg)
	}

	if after != nil {
		msg += "（进度 " + taskProgressText(after) + "）"
	}

	log.Printf("panel: 队列 growth uid=%s code=%s: %s", account3.UID, code, msg)

	return msg, nil
}

// autoClaimQueued 队列达标自动领奖并返回追加说明（无新增奖励时原样返回）。
func (p *Panel) autoClaimQueued(a *auth.Auth, code, msg string) string {
	credit, energy, err := p.claimTaskReward(a, code)
	if err != nil || (credit <= 0 && energy <= 0) {
		return msg
	}

	return msg + fmt.Sprintf("；自动领奖 +%d 分 +%d 能", credit, energy)
}

// tasksQueueStatus 队列状态（轮询用）。
func (p *Panel) tasksQueueStatus(w http.ResponseWriter, r *http.Request) {
	queueState2 := p.queue()
	queueState2.mu.Lock()
	defer queueState2.mu.Unlock()

	items := make([]queueItem, len(queueState2.items))
	copy(items, queueState2.items)
	writeJSON(w, http.StatusOK, map[string]any{
		"running":    queueState2.running,
		"total":      len(items),
		"conc":       queueState2.conc,
		"started":    !queueState2.startedAt.IsZero(),
		"started_at": queueState2.startedAt,
		"seq":        queueState2.seq,
		"items":      items,
	})
}

// schoolVouchers 我的券码：逐 CN 账号查开学季 /vouchers（3 并发，与 packages
// 同款限流），失败只在对应账号标 error。global 账号无开学季，不发上游调用。
func (p *Panel) schoolVouchers(w http.ResponseWriter, r *http.Request) {
	accts := p.cfg.Pool.List()

	type row struct {
		UID      string                   `json:"uid"`
		Nickname string                   `json:"nickname"`
		Vouchers []upstream.SchoolVoucher `json:"vouchers"`
		Err      string                   `json:"error,omitempty"`
	}

	out := make([]row, len(accts))
	sem := make(chan struct{}, 3)

	var wg sync.WaitGroup

	for i := range accts {
		st := &accts[i]
		if st.Disabled {
			continue // 未占位，行末统一压掉
		}

		account := p.cfg.Pool.AuthByUID(st.UID)
		if account == nil {
			continue
		}

		wg.Add(1)
		go func(i int, account5 *auth.Auth) {
			defer wg.Done()

			sem <- struct{}{}
			defer func() { <-sem }()

			row2 := row{UID: account5.UID, Nickname: account5.NicknameValue(), Vouchers: nil, Err: ""}
			switch {
			case account5.IsGlobal():
				row2.Err = "global realm（无开学季活动）"
			default:
				vs, err := p.cfg.Upstream.SchoolVouchers(account5)
				if err != nil {
					row2.Err = err.Error()
				} else {
					row2.Vouchers = vs
				}
			}

			out[i] = row2
		}(i, account)
	}

	wg.Wait()

	res := make([]row, 0, len(out))
	for _, it := range out {
		if it.UID != "" {
			res = append(res, it)
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{"accounts": res})
}
