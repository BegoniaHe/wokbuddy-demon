// compensation.ts 面板「官方补偿」的纯逻辑：待领判定、截止倒计时、汇总文案。
//
// 三条判据与后端 upstream.CompensationStatus.Pending() 同口径：active/claimed/credit_num
// 三者与门。前端再叠一条「查询失败必须显形」——否则上游全挂时卡片会说「当前没有待领
// 补偿」，把故障说成「没活动」，比不说更糟。

export interface CompensationRow {
  uid?: string;
  nickname?: string;
  realm?: string;
  skipped?: boolean;
  reason?: string;
  error?: string;
  active?: boolean;
  claimed?: boolean;
  credit_num?: number;
  end_time?: string;
}

// compPending 该行是否「可领且未领」（与后端 Pending() 同口径）。
export function compPending(row: CompensationRow | undefined): boolean {
  if (!row || row.skipped || row.error) return false;
  return Boolean(row.active) && !row.claimed && Number(row.credit_num || 0) > 0;
}

// compName 展示名：昵称优先，其次短 uid。
export function compName(row: CompensationRow | undefined): string {
  const nickname = String(row?.nickname || '').trim();
  if (nickname) return nickname;
  const uid = String(row?.uid || '');
  return uid ? uid.slice(0, 8) : '—';
}

// compDeadlineHours 距领取截止还剩多少小时（解析失败/无截止 → null）。
export function compDeadlineHours(endTime: unknown, now: number): number | null {
  const raw = String(endTime || '').trim();
  if (!raw) return null;
  // 上游是 "2006-01-02 15:04:05" 无时区字符串：按本地时间解析（与面板其它时间列同口径）。
  const ms = Date.parse(raw.replace(' ', 'T'));
  if (!Number.isFinite(ms)) return null;
  return Math.round(((ms - now) / 3600000) * 10) / 10;
}

// compDeadlineTone 截止紧迫度：<=48h 标红（bad），<=96h 警告，其余中性。
export function compDeadlineTone(hours: number | null): 'bad' | 'warn' | 'mute' {
  if (hours == null) return 'mute';
  if (hours <= 48) return 'bad';
  if (hours <= 96) return 'warn';
  return 'mute';
}

// compNote 卡片备注：**先看失败数**（部分/全部失败都要显形），再看待领与跳过。
export function compNote(rows: CompensationRow[] | undefined): string {
  const list = rows || [];
  const failed = list.filter((r) => r.error).length;
  const pending = list.filter((r) => compPending(r)).length;
  const skipped = list.filter((r) => r.skipped).length;
  const parts: string[] = [];
  if (pending) parts.push(pending + ' 个账号有待领补偿');
  if (failed) parts.push(failed + ' 个账号查询失败');
  if (skipped) parts.push(skipped + ' 个账号已跳过（国际版/企业版/禁用）');
  if (!parts.length) return '当前没有待领补偿';
  return parts.join(' · ');
}
