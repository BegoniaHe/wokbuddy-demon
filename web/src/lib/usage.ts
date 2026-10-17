// 用量页纯函数（排序 / 时间轴解析）。

export interface UsageTotals {
  requests?: number;
  errors?: number;
  prompt_tokens?: number;
  completion_tokens?: number;
  total_tokens?: number;
  avg_latency_ms?: number;
  avg_ttft_ms?: number;
  avg_tokens_per_second?: number;
  credits?: number;
  credit_tokens?: number;
  credits_per_1m_tokens?: number;
  credit_samples?: number;
  cache_hit_tokens?: number;
  cache_miss_tokens?: number;
}

export interface UsageRow {
  key?: string;
  extra?: string;
  realm?: string;
  nickname?: string;
  requests?: number;
  errors?: number;
  prompt_tokens?: number;
  completion_tokens?: number;
  total_tokens?: number;
  avg_latency_ms?: number;
  avg_tokens_per_second?: number;
  rate?: number | string;
  credits?: number;
  credit_tokens?: number;
  credits_per_1m_tokens?: number;
  credit_samples?: number;
  cache_hit_tokens?: number;
  cache_miss_tokens?: number;
}

export interface SeriesPoint {
  t: string;
  scope?: string;
  prompt_tokens?: number;
  completion_tokens?: number;
  total_tokens?: number;
  requests?: number;
  avg_ttft_ms?: number;
  credits?: number;
}

export interface UsageData {
  totals?: UsageTotals;
  by_account?: UsageRow[];
  by_model?: UsageRow[];
  by_realm?: UsageRow[];
  credit_by_account?: UsageRow[];
  credit_by_model?: UsageRow[];
  series?: SeriesPoint[];
  window_from?: string;
  window_to?: string;
  buckets?: number;
  since?: string;
  file_bytes?: number;
}

export type DimKey = 'account' | 'model' | 'realm';
export type CreditDim = 'account' | 'model';
export type UsSort = 'total' | 'requests' | 'errors' | 'latency';

export const US_DIMS: Record<DimKey, { key: keyof UsageData; withRealm: boolean; withPerf: boolean; span: number; title: string }> = {
  account: { key: 'by_account', withRealm: true, withPerf: true, span: 10, title: '账号' },
  model: { key: 'by_model', withRealm: false, withPerf: false, span: 7, title: '模型' },
  realm: { key: 'by_realm', withRealm: false, withPerf: false, span: 7, title: 'realm' },
};

// usSortRows 按当前排序字段降序。
export function usSortRows(rows: UsageRow[], sort: UsSort): UsageRow[] {
  const out = rows.slice();
  const num = (v: unknown) => {
    const n = Number(v || 0);
    return Number.isFinite(n) ? n : 0;
  };
  const val = (a: UsageRow) =>
    sort === 'requests'
      ? num(a.requests)
      : sort === 'errors'
        ? num(a.errors)
        : sort === 'latency'
          ? num(a.avg_latency_ms)
          : num(a.total_tokens);
  out.sort((a, b) => val(b) - val(a));
  return out;
}

// parsePointTime 把后端时间桶 t 解析成毫秒（hour: "2026-09-16T13" / day: "2026-09-16"）。
export function parsePointTime(p: SeriesPoint): number | null {
  const s = p.t.length === 13 ? p.t + ':00:00' : p.t + 'T00:00:00';
  const d = new Date(s);
  return isNaN(d.getTime()) ? null : d.getTime();
}

// fmtTokTimeLabel 时间桶短标签（日桶 MM-DD，小时桶 HH:00）。
// 必须复用 parsePointTime 规范化：小时桶 t 为 "YYYY-MM-DDTHH"（13 字符），
// 直接 new Date 在 V8 下是 Invalid Date，会渲染出 "NaN:00"。
export function fmtTokTimeLabel(p: SeriesPoint): string {
  const ms = parsePointTime(p);
  if (ms === null) return p.t;
  const d = new Date(ms);
  return p.scope === 'day'
    ? d.getMonth() + 1 + '-' + String(d.getDate()).padStart(2, '0')
    : String(d.getHours()).padStart(2, '0') + ':00';
}

// cacheRateTone 缓存命中率健康度色（≥90 绿 / 80-90 黄 / <80 红）。
export function cacheRateTone(pct: number): 'ok' | 'warn' | 'bad' {
  return pct >= 90 ? 'ok' : pct >= 80 ? 'warn' : 'bad';
}
