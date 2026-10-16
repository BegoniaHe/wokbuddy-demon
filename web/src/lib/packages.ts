// 积分构成页纯函数（来源归并 / 到期聚合 / 逐包明细）。
import { fmtTok } from '@/lib/format';

export interface Pkg {
  package_code?: string;
  name?: string;
  sub_product_code?: string;
  size?: number;
  remain?: number;
  used?: number;
  end_time?: string;
  created_at?: string;
  expires_at?: number;
}

export interface PkgAccount {
  uid?: string;
  nickname?: string;
  realm?: string;
  error?: string;
  size?: number;
  remain?: number;
  packages?: Pkg[];
}

export const PK_COLORS = ['#4f8cff', '#25b08b', '#e8a33d', '#c96bd6', '#e2607a', '#5aa9e6', '#8fbf3f', '#b58b5a', '#7d8fa8', '#d4785c'];
export const PK_ACCOUNT_COLORS = ['#4f8cff', '#25b08b', '#e8a33d', '#c96bd6', '#e2607a', '#20a4a4', '#8fbf3f', '#d4785c', '#7c83db', '#c48a2f', '#b45f8c', '#5aa9e6'];

export const PK_DEFAULT_DETAIL_LIMIT = 5;
export const PK_DAY_MS = 24 * 3600 * 1000;
export const PK_SORT_LABELS: Record<string, string> = {
  end_asc: '按到期升序 · 近的在前',
  size_desc: '按面额降序',
};

export function pkColor(i: number): string {
  return PK_COLORS[i % PK_COLORS.length]!;
}

export function pkAccountColorMap(list: PkgAccount[]): Map<string, string> {
  const uids = (list || [])
    .filter((a) => a && !a.error && a.uid)
    .map((a) => String(a.uid))
    .sort();
  const colors = new Map<string, string>();
  uids.forEach((uid, i) => colors.set(uid, PK_ACCOUNT_COLORS[i % PK_ACCOUNT_COLORS.length]!));
  return colors;
}

export interface PkgSource {
  key: string;
  name: string;
  code: string;
  n: number;
  remain: number;
  size: number;
  used: number;
  minEnd: string;
  minCreated: string;
}

// pkBySource 把包按 code+name 归并（上游对两类包可能复用同一 name）。
export function pkBySource(packs: Pkg[]): PkgSource[] {
  const m = new Map<string, PkgSource>();
  for (const p of packs) {
    const k = (p.package_code || '') + '|' + (p.name || '(未命名)');
    const e = m.get(k) || {
      key: k,
      name: p.name || '(未命名)',
      code: p.package_code || '',
      n: 0,
      remain: 0,
      size: 0,
      used: 0,
      minEnd: '',
      minCreated: '',
    };
    e.n += 1;
    e.remain += Number(p.remain || 0);
    e.size += Number(p.size || 0);
    e.used += Number(p.used || 0);
    const t = (p.end_time || '').slice(0, 10);
    if (t && (!e.minEnd || t < e.minEnd)) e.minEnd = t;
    const c = (p.created_at || '').slice(0, 10);
    if (c && (!e.minCreated || c < e.minCreated)) e.minCreated = c;
    m.set(k, e);
  }
  return [...m.values()].sort((a, b) => b.size - a.size);
}

export function pkDetailLimitValue(raw: unknown): number {
  const n = Number(raw);
  return Number.isFinite(n) && n > 0 ? Math.floor(n) : PK_DEFAULT_DETAIL_LIMIT;
}

export function pkExpiryMs(p: Pkg): number | null {
  const raw = Number(p.expires_at);
  if (Number.isFinite(raw) && raw > 0) return raw;
  const text = String(p.end_time || '').trim();
  if (!text) return null;
  let iso = text.includes('T') ? text : text.replace(' ', 'T');
  if (!/(?:Z|[+-]\d\d:\d\d)$/.test(iso)) iso += '+08:00';
  const parsed = Date.parse(iso);
  return Number.isFinite(parsed) ? parsed : null;
}

// pkDetailCompare 单账号逐包明细排序（end_asc 到期升序 / size_desc 面额降序）。
export function pkDetailCompare(a: Pkg, b: Pkg, mode: string): number {
  const sizeOf = (p: Pkg) => {
    const n = Number(p.size);
    return Number.isFinite(n) ? n : 0;
  };
  if (mode === 'size_desc') {
    const d = sizeOf(b) - sizeOf(a);
    if (d !== 0) return d;
  }
  const ea = pkExpiryMs(a);
  const eb = pkExpiryMs(b);
  if (ea == null && eb != null) return 1;
  if (ea != null && eb == null) return -1;
  if (ea != null && eb != null && ea !== eb) return ea - eb;
  return sizeOf(b) - sizeOf(a);
}

export interface PkgDetailGroups {
  visible: Pkg[];
  rest: Pkg[];
  used: Pkg[];
  restSize: number;
  restRemain: number;
  usedSize: number;
}

export function pkDetailGroups(packs: Pkg[], limit: unknown, mode: string): PkgDetailGroups {
  const active: Pkg[] = [];
  const used: Pkg[] = [];
  let usedSize = 0;
  let restSize = 0;
  let restRemain = 0;
  for (const p of packs || []) {
    const remain = Number(p.remain);
    if (remain > 0) {
      active.push(p);
      continue;
    }
    used.push(p);
    const size = Number(p.size);
    if (Number.isFinite(size)) usedSize += size;
  }
  active.sort((a, b) => pkDetailCompare(a, b, mode));
  used.sort((a, b) => pkDetailCompare(a, b, mode));
  const visible = active.slice(0, pkDetailLimitValue(limit));
  const rest = active.slice(visible.length);
  for (const p of rest) {
    const size = Number(p.size);
    if (Number.isFinite(size)) restSize += size;
    const remain = Number(p.remain);
    if (Number.isFinite(remain)) restRemain += remain;
  }
  return { visible, rest, used, restSize, restRemain, usedSize };
}

export function pkCreditOpacity(days: number | null): number {
  if (days == null || !Number.isFinite(Number(days))) return 1;
  return 0.25 + (0.75 * Math.max(0, Math.min(29, Number(days) - 1))) / 29;
}

export function pkExpiryText(expiresAt: number | null): string {
  if (!expiresAt) return '无到期时间';
  const diff = expiresAt - Date.now();
  if (diff <= 0) return '已到期';
  const minutes = Math.max(1, Math.ceil(diff / 60000));
  if (minutes < 60) return '剩余 ' + minutes + ' 分钟';
  const hours = Math.ceil(diff / 3600000);
  if (hours < 24) return '剩余 ' + hours + ' 小时';
  return '剩余 ' + Math.ceil(diff / PK_DAY_MS) + ' 天';
}

export function pkExpiryDateTime(expiresAt: number | null): string {
  if (!expiresAt) return '—';
  return new Date(expiresAt).toLocaleString('zh-CN', {
    timeZone: 'Asia/Shanghai',
    hour12: false,
    year: 'numeric',
    month: '2-digit',
    day: '2-digit',
    hour: '2-digit',
    minute: '2-digit',
    second: '2-digit',
  });
}

export interface Segment {
  amount: number;
  expiresAt: number | null;
  days: number | null;
  source: string;
  uid: string;
  accountName: string;
}

export function pkAccountSegments(a: PkgAccount, now: number): Segment[] {
  let balance = Math.max(0, Number(a.remain || 0));
  const out: Segment[] = [];
  for (const p of a.packages || []) {
    const remain = Number(p.remain || 0);
    if (!Number.isFinite(remain) || remain <= 0 || balance <= 0) continue;
    const amount = Math.min(balance, remain);
    const expiresAt = pkExpiryMs(p);
    out.push({
      amount,
      expiresAt,
      days: expiresAt == null ? null : Math.max(0, Math.ceil((expiresAt - now) / PK_DAY_MS)),
      source: p.name || '积分',
      uid: String(a.uid || ''),
      accountName: a.nickname || String(a.uid || '').slice(0, 8) || '未命名账号',
    });
    balance -= amount;
  }
  return out.sort((x, y) => {
    if (x.expiresAt == null && y.expiresAt != null) return 1;
    if (x.expiresAt != null && y.expiresAt == null) return -1;
    return (x.expiresAt || 0) - (y.expiresAt || 0);
  });
}

export interface CreditDayRow {
  days: number;
  credits: number;
  segments: Segment[];
}

export function summarizeCreditDays(
  list: PkgAccount[],
  now: number,
): { rows: CreditDayRow[]; accountCount: number; unavailable: number } {
  const buckets = new Map<number, CreditDayRow>();
  let unavailable = 0;
  for (const a of list || []) {
    if (a.error || !Number.isFinite(Number(a.remain))) {
      unavailable++;
      continue;
    }
    for (const segment of pkAccountSegments(a, now)) {
      if (segment.days == null) continue;
      let row = buckets.get(segment.days);
      if (!row) {
        row = { days: segment.days, credits: 0, segments: [] };
        buckets.set(segment.days, row);
      }
      row.credits += segment.amount;
      row.segments.push(segment);
    }
  }
  const rows = [...buckets.values()].sort((a, b) => a.days - b.days);
  for (const row of rows) {
    row.segments.sort(
      (a, b) =>
        (a.expiresAt || Infinity) - (b.expiresAt || Infinity) ||
        a.accountName.localeCompare(b.accountName) ||
        a.source.localeCompare(b.source),
    );
  }
  return { rows, accountCount: (list || []).length, unavailable };
}

export function fmtPkgTok(n: unknown): string {
  return fmtTok(n);
}
