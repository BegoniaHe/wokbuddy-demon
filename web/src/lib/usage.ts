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
  errors?: number;
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

// ── 日历热力图与趋势（issue #111）─────────────────────────────────────
export type HeatMetric = 'total_tokens' | 'requests' | 'credits';
export type TrendMetric = 'tokens' | 'requests' | 'errors' | 'ttft' | 'credits';

export interface HeatDay {
  date: string; // YYYY-MM-DD（服务端日桶键）
  value?: number; // undefined = 该日无数据（不是 0）
}

export interface TrendPoint {
  t: number; // 毫秒
  label: string;
  value: number;
}

const DAY_KEY = /^\d{4}-\d{2}-\d{2}$/;

// heatValue 取日桶的指定指标；缺失/非有限数返回 undefined（按「无数据」处理）。
function heatValue(p: SeriesPoint, metric: HeatMetric): number | undefined {
  const raw = metric === 'credits' ? p.credits : p[metric];
  if (raw === undefined || raw === null) return undefined;
  const n = Number(raw);
  return Number.isFinite(n) ? n : undefined;
}

// usDayHeatmap 把日桶聚合成日历热力图数据（issue #111）。
//
// 只消费 scope=day 的点：日键就是服务端本地日，前端不按浏览器时区重算——
// 否则同一桶在跨时区的浏览器里会落到不同格子，热力图与明细/总量对不上。
// 数据范围 = 最早..最晚日桶（最多 maxDays 天），中间无数据的日期 value 省略，
// 由 UI 显示「无数据」，不补零伪造。无日桶（例如不足一天的自定义范围）返回 []。
export function usDayHeatmap(series: SeriesPoint[], metric: HeatMetric, maxDays = 365): HeatDay[] {
  const sums = new Map<string, number>();
  for (const p of series) {
    const isDay = p.scope === 'day' || (p.scope == null && DAY_KEY.test(p.t));
    if (!isDay || !DAY_KEY.test(p.t)) continue;
    const v = heatValue(p, metric);
    if (v === undefined) continue;
    sums.set(p.t, (sums.get(p.t) ?? 0) + v);
  }
  if (!sums.size) return [];
  const keys = Array.from(sums.keys()).sort();
  const last = Date.parse(keys[keys.length - 1] + 'T00:00:00Z');
  const first = Date.parse(keys[0] + 'T00:00:00Z');
  const span = Math.floor((last - first) / 86400000) + 1;
  const start = span > maxDays ? last - (maxDays - 1) * 86400000 : first;
  const out: HeatDay[] = [];
  for (let t = start; t <= last; t += 86400000) {
    const date = new Date(t).toISOString().slice(0, 10);
    out.push({ date, value: sums.get(date) });
  }
  return out;
}

// usTrendPoints 把 series 映射成单个指标的趋势点（升序，跳过缺失值）。
export function usTrendPoints(series: SeriesPoint[], metric: TrendMetric): TrendPoint[] {
  const out: TrendPoint[] = [];
  for (const p of series) {
    const t = parsePointTime(p);
    if (t === null) continue;
    let raw: number | undefined;
    switch (metric) {
      case 'tokens': {
        const pt = Number(p.prompt_tokens ?? 0);
        const ct = Number(p.completion_tokens ?? 0);
        raw = p.total_tokens != null ? Number(p.total_tokens) : pt + ct;
        break;
      }
      case 'requests':
        raw = p.requests;
        break;
      case 'errors':
        raw = p.errors;
        break;
      case 'ttft':
        raw = p.avg_ttft_ms;
        break;
      case 'credits':
        raw = p.credits;
        break;
    }
    if (raw === undefined || raw === null || !Number.isFinite(Number(raw))) continue;
    out.push({ t, label: fmtTokTimeLabel(p), value: Number(raw) });
  }
  out.sort((a, b) => a.t - b.t);
  return out;
}

// ── 时序图布局（窄屏适配，issue #202）───────────────────────────────

export interface ChartLayout {
  W: number;
  H: number;
  PL: number;
  PR: number;
  PT: number;
  PB: number;
  iw: number;
  ih: number;
  ticks: number;
}

// chartLayout 时序图的布局参数：宽屏保持既有 1200×200 / 6 刻度（零回归），
// 窄屏改用 440×230 / 4 刻度——固定宽 viewBox 在 360px 屏上会被缩到约 0.28 倍，
// 刻度与线条不可读。
export function chartLayout(narrow: boolean): ChartLayout {
  const W = narrow ? 440 : 1200;
  const H = narrow ? 230 : 200;
  const PL = narrow ? 44 : 58;
  const PR = narrow ? 10 : 14;
  const PT = 18;
  const PB = narrow ? 26 : 30;

  return {
    W,
    H,
    PL,
    PR,
    PT,
    PB,
    iw: W - PL - PR,
    ih: H - PT - PB,
    ticks: narrow ? 4 : 6,
  };
}
