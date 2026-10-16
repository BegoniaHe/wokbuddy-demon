// 模型目录筛选/排序（纯函数，便于测试）。

export interface ModelEntry {
  id: string;
  name?: string;
  vendor?: string;
  description?: string;
  tags?: string[];
  supports_tool_call?: boolean;
  supports_images?: boolean;
  supports_reasoning?: boolean;
  is_default?: boolean;
  can_disable_thinking?: boolean;
  supported_efforts?: string[];
  default_effort?: string;
  promo_factor?: number | string | null;
  promo_credits?: number | string | null;
  promo_label?: string;
  promo_note?: string;
  credits?: number | string;
  max_output_tokens?: number;
  context_length?: number;
}

export interface ModelFilter {
  q: string;
  realm: string;
  cap: string;
  effort: string;
  promo: string;
  sort: string;
}

export const DEFAULT_MODEL_FILTER: ModelFilter = {
  q: '',
  realm: '',
  cap: '',
  effort: '',
  promo: '',
  sort: 'default',
};

// mdRateValue 当前生效倍率：优先促销价（限时免费 = 0），无倍率记 Infinity（排最后）。
export function mdRateValue(m: ModelEntry): number {
  const raw = m.promo_credits != null && m.promo_credits !== '' ? m.promo_credits : m.credits;
  const n = parseFloat(String(raw ?? '').replace(/[^\d.]/g, ''));
  return Number.isFinite(n) ? n : Infinity;
}

export function mdSearchText(m: ModelEntry): string {
  return [m.id, m.name, m.vendor, m.description, (m.tags || []).join(' ')]
    .filter(Boolean)
    .join(' ')
    .toLowerCase();
}

// mdMatch 单个模型是否满足全部筛选条件（条件间 AND，空即不参与）。
export function mdMatch(m: ModelEntry, f: ModelFilter): boolean {
  if (f.q) {
    const text = mdSearchText(m);
    for (const kw of f.q.toLowerCase().split(/\s+/).filter(Boolean)) {
      if (!text.includes(kw)) return false;
    }
  }
  if (f.realm && !String(m.id || '').startsWith(f.realm + ':')) return false;
  if (f.cap === 'tool' && !m.supports_tool_call) return false;
  if (f.cap === 'vision' && !m.supports_images) return false;
  if (f.cap === 'reasoning' && !m.supports_reasoning) return false;
  if (f.cap === 'default' && !m.is_default) return false;
  if (f.effort === 'off') {
    if (!m.can_disable_thinking) return false;
  } else if (f.effort && !(m.supported_efforts || []).includes(f.effort)) {
    return false;
  }
  const factor = m.promo_factor == null ? null : Number(m.promo_factor);
  if (f.promo === 'promo' && factor == null && !m.promo_label) return false;
  if (f.promo === 'free' && factor !== 0) return false;
  if (f.promo === 'discount' && !(factor != null && factor > 0)) return false;
  return true;
}

// mdSortList 按排序条件返回新数组（不改动入参）。
export function mdSortList(list: ModelEntry[], f: ModelFilter): ModelEntry[] {
  const out = list.slice();
  const num = (v: unknown) => {
    const n = Number(v || 0);
    return Number.isFinite(n) ? n : 0;
  };
  if (f.sort === 'rate') out.sort((a, b) => mdRateValue(a) - mdRateValue(b));
  else if (f.sort === 'context') out.sort((a, b) => num(b.context_length) - num(a.context_length));
  else if (f.sort === 'output')
    out.sort((a, b) => num(b.max_output_tokens) - num(a.max_output_tokens));
  else if (f.sort === 'name') out.sort((a, b) => String(a.id || '').localeCompare(String(b.id || '')));
  return out;
}
