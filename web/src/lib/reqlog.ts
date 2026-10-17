// 请求记录格式化与筛选（纯函数，便于测试）。
import { fmtClockTime, fmtMs, fmtTok, trimFixed } from '@/lib/format';

export interface DecisionCandidate {
  uid: string;
  weight: number;
  tier: number;
}

export interface DecisionTrace {
  candidates: number;
  excluded?: Record<string, number>;
  cost_tier: number;
  explored?: boolean;
  sticky_hit?: boolean;
  rotations?: string[];
  top?: DecisionCandidate[];
  final_uid?: string;
}

export interface RequestEntry {
  time?: string;
  status?: number | string;
  outcome?: string;
  model?: string;
  account?: string;
  client_ip?: string;
  user_agent?: string;
  duration_ms?: number;
  total_tokens?: number;
  prompt_tokens?: number;
  completion_tokens?: number;
  credit?: number;
  credit_known?: boolean;
  request_id?: string;
  cache_hit_tokens?: number;
  cache_miss_tokens?: number;
  decision?: DecisionTrace;
}

export interface ReqFilter {
  q: string;
  outcome: string;
}

export function cacheRateText(hit: unknown, miss: unknown): string {
  const h = Number(hit || 0);
  const m = Number(miss || 0);
  const total = h + m;
  if (!total) return '—';
  return String(Math.round((h / total) * 1000) / 10) + '%';
}

export function fmtBytes(bytes: unknown): string {
  const n = Number(bytes || 0);
  if (n < 1024) return n + ' B';
  if (n < 1024 * 1024) return (n / 1024).toFixed(1) + ' KB';
  return (n / 1024 / 1024).toFixed(1) + ' MB';
}

const OUTCOME_LABEL: Record<string, string> = {
  success: '成功',
  http_error: 'HTTP 错误',
  stream_error: '流错误',
  interrupted: '中断',
};

export function reqOutcomeLabel(e: RequestEntry | undefined): string {
  const outcome = String((e && e.outcome) || '');
  return OUTCOME_LABEL[outcome] || outcome || '—';
}

export function reqOutcomeTone(e: RequestEntry | undefined): 'ok' | 'warn' | 'bad' | 'mute' {
  const outcome = String((e && e.outcome) || '');
  if (outcome === 'success') return 'ok';
  if (outcome === 'interrupted') return 'warn';
  return outcome ? 'bad' : 'mute';
}

export function reqTokenCell(e: RequestEntry | undefined): string {
  const total =
    Number((e && e.total_tokens) || 0) ||
    Number((e && e.prompt_tokens) || 0) + Number((e && e.completion_tokens) || 0);
  return total ? fmtTok(total) : '—';
}

export function reqCreditCell(e: RequestEntry | undefined): string {
  if (!e || !e.credit_known) return '—';
  const v = Number(e.credit);
  return Number.isFinite(v) ? trimFixed(v.toFixed(2)) : '—';
}

// reqMatch 请求记录筛选：q 对 IP/UA/模型/账号/请求 ID 做空格分词 AND 包含；outcome 精确匹配。
export function reqMatch(e: RequestEntry | undefined, f: ReqFilter): boolean {
  if (f.outcome && String((e && e.outcome) || '') !== f.outcome) return false;
  if (f.q) {
    const text = [e?.client_ip, e?.user_agent, e?.model, e?.account, e?.request_id]
      .filter(Boolean)
      .join(' ')
      .toLowerCase();
    for (const kw of f.q.toLowerCase().split(/\s+/).filter(Boolean)) {
      if (!text.includes(kw)) return false;
    }
  }
  return true;
}

export function requestLogText(e: RequestEntry | undefined): string {
  const when = fmtClockTime(e && e.time);
  const token =
    Number((e && e.total_tokens) || 0) ||
    Number((e && e.prompt_tokens) || 0) + Number((e && e.completion_tokens) || 0);
  let credit = 'credit —';
  if (e && e.credit_known) {
    const value = Number(e.credit);
    if (Number.isFinite(value)) credit = String(Number(value.toFixed(2))) + ' credit';
  }
  const cr = cacheRateText(e && e.cache_hit_tokens, e && e.cache_miss_tokens);
  return [
    when,
    String((e && e.status) || '—') + ' ' + reqOutcomeLabel(e),
    (e && e.model) || '—',
    (e && e.account) || '—',
    (e && e.client_ip) || '—',
    (e && e.user_agent) || '—',
    fmtMs(e && e.duration_ms),
    fmtTok(token) + ' tok',
    credit,
    cr === '—' ? '' : '命中 ' + cr,
    (e && e.request_id) || '—',
  ]
    .filter(Boolean)
    .join(' | ');
}
