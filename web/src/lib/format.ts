// 格式化与解析纯函数。全部无副作用、不触碰 DOM，便于单元测试。

// parseAPITime 解析后端返回的时间串；空/零值时间（0001-…）视作无。
export function parseAPITime(value: unknown): number {
  const text = String(value ?? '');
  if (!text || text.startsWith('0001-')) return 0;
  const ms = Date.parse(text);
  return Number.isFinite(ms) ? ms : 0;
}

export function fmtLocalDateTime(ms: number): string {
  const d = new Date(ms);
  const p = (n: number) => String(n).padStart(2, '0');
  return (
    d.getFullYear() +
    '-' +
    p(d.getMonth() + 1) +
    '-' +
    p(d.getDate()) +
    ' ' +
    p(d.getHours()) +
    ':' +
    p(d.getMinutes())
  );
}

// ago 相对时间（中文）。
export function ago(iso: unknown): string {
  const s = String(iso ?? '');
  if (!s || s.startsWith('0001-')) return '—';
  const ms = new Date(s).getTime();
  if (!Number.isFinite(ms)) return '—';
  const diff = (Date.now() - ms) / 1000;
  if (diff < 0) return '刚刚';
  if (diff < 60) return Math.floor(diff) + ' 秒前';
  if (diff < 3600) return Math.floor(diff / 60) + ' 分钟前';
  if (diff < 86400) return Math.floor(diff / 3600) + ' 小时前';
  return Math.floor(diff / 86400) + ' 天前';
}

// fmtClockTime ISO 时间串 → 本地 HH:MM:SS；空/无法解析回落 '—'。
export function fmtClockTime(iso: unknown): string {
  const s = String(iso ?? '');
  if (!s) return '—';
  const ms = new Date(s).getTime();
  if (!Number.isFinite(ms)) return '—';
  return new Date(ms).toLocaleTimeString('zh-CN', { hour12: false });
}

// dur 秒 → 「N时MM分 / N分SS秒 / N秒」。
export function dur(sec: number): string {
  sec = Math.max(0, Math.round(sec));
  const h = Math.floor(sec / 3600);
  const m = Math.floor((sec % 3600) / 60);
  const s = sec % 60;
  return h
    ? h + '时' + String(m).padStart(2, '0') + '分'
    : m
      ? m + '分' + String(s).padStart(2, '0') + '秒'
      : s + '秒';
}

// formatTokenCount 人类可读 token 数（k/m/b，四舍五入后自动升级单位）。
export function formatTokenCount(tokens: unknown): string {
  if (tokens == null || tokens === '') return '—';
  const n = Number(tokens);
  if (!Number.isFinite(n) || n < 0) return '—';
  if (n < 1000) return String(Math.round(n));
  const units: [string, number][] = [
    ['k', 1e3],
    ['m', 1e6],
    ['b', 1e9],
  ];
  let unit = units[0]!;
  for (const candidate of units) {
    if (n >= candidate[1]) unit = candidate;
  }
  let value = n / unit[1];
  let rounded = Number(value.toFixed(1));
  const next = units[units.indexOf(unit) + 1];
  if (next && rounded >= 1000) {
    unit = next;
    value = n / unit[1];
    rounded = Number(value.toFixed(1));
  }
  return rounded + unit[0];
}

export function formatLatency(ms: unknown): string {
  if (ms == null || ms === '') return '—';
  const n = Number(ms);
  if (!Number.isFinite(n) || n <= 0) return '—';
  return n < 1000 ? Math.round(n) + 'ms' : (n / 1000).toFixed(1).replace(/\.0$/, '') + 's';
}

export function formatRate(rate: unknown): string {
  if (rate == null || rate === '') return '—';
  const n = Number(rate);
  if (!Number.isFinite(n) || n < 0) return '—';
  return n.toFixed(1) + 'tok/s';
}

export interface RateLimitRow {
  model?: string;
  kind?: string;
  reset_at?: string;
  until?: string;
}

export interface RateLimitMeta {
  model: string;
  kind: string;
  detail: string;
  title: string;
}

// rateLimitMeta 把上游限流/模型不可用行翻译成人读的「还要多久、什么时候解封」。
//
// 双时钟（until < resetAt）：网关自己的 until 决定「实际何时放行」，上游声明的
// resetAt 只是对方口径。此时主文字同时给出两个时刻，**倒计时跟随 until**——
// 否则会出现「显示还要 2 小时、实际 10 分钟就掉框」，用户会认为面板在骗人。
export function rateLimitMeta(row: RateLimitRow | null | undefined, now: number): RateLimitMeta {
  const model = String((row && row.model) || '未知模型');
  const kind = String((row && row.kind) || 'rate_limit');
  const resetAt = parseAPITime(row && row.reset_at);
  const until = parseAPITime(row && row.until);
  const dualClock = Boolean(until && resetAt && until < resetAt);
  // 倒计时跟随「实际放行」时刻：双时钟场景跟 until，其余沿用 resetAt || until。
  const deadline = dualClock ? until : resetAt || until;
  const remaining = deadline > now ? Math.round((deadline - now) / 1000) : 0;
  if (kind === 'model_unavailable') {
    return {
      model,
      kind,
      detail: remaining ? '预计 ' + dur(remaining) + ' 后重试' : '等待重新探测',
      title: model + '\n模型当前不可用' + (deadline ? '\n最早重试：' + fmtLocalDateTime(deadline) : ''),
    };
  }
  if (dualClock) {
    const title = [
      model,
      '网关最早重试：' + fmtLocalDateTime(until),
      '上游重置：' + fmtLocalDateTime(resetAt),
    ];
    const detail =
      '网关 ' + fmtLocalDateTime(until) + ' 起重试（剩余 ' + dur(remaining) + '）' +
      ' · 上游称 ' + fmtLocalDateTime(resetAt) + ' 恢复';
    return { model, kind, detail, title: title.join('\n') };
  }
  const detail = resetAt
    ? '预计 ' + fmtLocalDateTime(resetAt) + ' 解封' + (remaining ? '（剩余 ' + dur(remaining) + '）' : '')
    : until
      ? '预计 ' + fmtLocalDateTime(until) + ' 恢复（剩余 ' + dur(remaining) + '）'
      : '预计解封时间未知';
  const title = [model, resetAt ? '上游重置：' + fmtLocalDateTime(resetAt) : '上游重置：时间未知'];
  return { model, kind, detail, title: title.join('\n') };
}

// ── 用量页专用格式化 ─────────────────────────────────────────────────

export function fmtTok(n: unknown): string {
  const v = Number(n || 0);
  if (v >= 1e9) return (v / 1e9).toFixed(2) + 'B';
  if (v >= 1e6) return (v / 1e6).toFixed(2) + 'M';
  if (v >= 1e3) return (v / 1e3).toFixed(1) + 'k';
  return String(v);
}

export function fmtMs(ms: unknown): string {
  const v = Number(ms || 0);
  if (!v) return '—';
  if (v >= 1000) return (v / 1000).toFixed(2) + 's';
  return Math.round(v) + 'ms';
}

// usageRate：用量页的 tok/s（带空格），与账号池 formatRate 的紧凑写法不同。
export function usageRate(r: unknown): string {
  return r ? Number(r).toFixed(1) + ' tok/s' : '—';
}

export function trimFixed(s: unknown): string {
  const str = String(s);
  if (!str.includes('.')) return str;
  return str.replace(/0+$/, '').replace(/\.$/, '');
}

export function fmtCredit(n: unknown): string {
  const v = Number(n || 0);
  if (!Number.isFinite(v)) return '—';
  return trimFixed(v.toFixed(2));
}

export function fmtCreditRatio(v: unknown, samples: unknown, tokens: unknown): string {
  if (!samples || !tokens) return '—';
  const n = Number(v || 0);
  if (!Number.isFinite(n)) return '—';
  return trimFixed(n.toFixed(4)) + ' / 1M';
}

export function fmtModelRate(rate: unknown): string {
  const s = String(rate || '').trim();
  return s ? 'x' + s : '—';
}

// dtLocalValue / dtLocalParse 与 <input type=datetime-local> 取值格式互转。
export function dtLocalValue(d: Date): string {
  const p = (n: number) => String(n).padStart(2, '0');
  return (
    d.getFullYear() +
    '-' +
    p(d.getMonth() + 1) +
    '-' +
    p(d.getDate()) +
    'T' +
    p(d.getHours()) +
    ':' +
    p(d.getMinutes())
  );
}

export function dtLocalParse(s: string): Date | null {
  if (!s) return null;
  const d = new Date(s);
  return isNaN(d.getTime()) ? null : d;
}
