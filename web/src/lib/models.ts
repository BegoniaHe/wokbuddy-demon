// 模型目录筛选/排序（纯函数，便于测试）。

import { fmtLocalDateTime, parseAPITime } from './format';

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

// ── Global 模型可用性（本地证据合成，不产生上游推理探测） ──────────────

export interface ModelLockLike {
  model?: string;
  realm?: string;
  state?: string;
  servable?: number;
  total?: number;
  locked?: number;
  unlock_at?: string;
  reason?: string;
}

export interface RecentRequestLike {
  model?: string;
  status?: number | string;
  outcome?: string;
}

export type AvailabilityTone = 'ok' | 'warn' | 'bad' | 'mute';

export interface ModelAvailability {
  key: string;
  label: string;
  tone: AvailabilityTone;
  tip: string;
}

// mdAvailability 合成 global 模型的可用性标签：**只用两路本地证据**，不新增上游
// 推理探测。优先级（与上游同口径）：
//  1. model_locks（锁在选号层，最权威）：
//     - locked  → 无权限(11158) / 上游无此模型(11102) / 限流不可用 细分；
//     - starved → 账号暂不可用（不是模型自身问题）；
//     - partial → 部分可用（N/M 号可服务）。
//  2. request_metrics.recent 里该模型最近一次真实请求结果（成功 / 429 / ≥500 / 其他失败）；
//  3. 两路都无 → 「待验证」。
// 非 global 模型返回 null（本标签只对 global 域有意义）。
export function mdAvailability(
  modelId: string,
  locks: ModelLockLike[] | null | undefined,
  recent: RecentRequestLike[] | null | undefined,
): ModelAvailability | null {
  const id = String(modelId || '');
  if (!id.startsWith('global:')) return null;
  const model = id.slice('global:'.length);

  const lock = (locks || []).find((l) => l && l.model === model && (!l.realm || l.realm === 'global'));
  if (lock) {
    const state = String(lock.state || '');
    const reason = String(lock.reason || '');
    const servable = Number(lock.servable || 0);
    const total = Number(lock.total || 0);
    // 时间统一按本地时区渲染（与面板其它时间列同口径）：后端给的是 RFC3339 UTC，
    // 直接切片会把 UTC 当本地时间显示，且零值时间会渲染成 0001-01-01。
    const unlockMs = parseAPITime(lock.unlock_at);
    const unlock = unlockMs ? '；最早 ' + fmtLocalDateTime(unlockMs) + ' 解封' : '';
    if (state === 'partial') {
      return {
        key: 'partial',
        label: '部分可用',
        tone: 'warn',
        tip: '锁池：' + servable + '/' + total + ' 个 global 号可服务该模型' + unlock,
      };
    }
    if (state === 'starved') {
      return {
        key: 'starved',
        label: '账号暂不可用',
        tone: 'warn',
        tip: '锁池：此刻没有号能服务（账号级冷却/熔断/在途占满，与模型无关，等一下会好）' + unlock,
      };
    }
    // locked：按上游原因细分。
    if (reason.includes('11158')) {
      return { key: 'forbidden', label: '无权限', tone: 'bad', tip: '锁池：账号类型不允许该模型（11158）' + unlock };
    }
    if (reason.includes('11102')) {
      return { key: 'no_model', label: '上游无此模型', tone: 'bad', tip: '锁池：上游目录没有该模型（11102）' + unlock };
    }
    return { key: 'rate', label: '限流不可用', tone: 'bad', tip: '锁池：该模型被限流/冷却，全部账号不可用' + unlock };
  }

  const latest = (recent || []).find((r) => r && r.model === model);
  if (latest) {
    const status = Number(latest.status || 0);
    const outcome = String(latest.outcome || '');
    if (outcome === 'success') {
      return { key: 'recent_ok', label: '近期可用', tone: 'ok', tip: '依据：最近一次真实请求成功' };
    }
    if (status === 429) {
      return { key: 'recent_429', label: '近期限流', tone: 'warn', tip: '依据：最近一次真实请求 429' };
    }
    if (status >= 500) {
      return { key: 'recent_5xx', label: '近期异常', tone: 'bad', tip: '依据：最近一次真实请求 ' + status };
    }
    return { key: 'recent_fail', label: '近期失败', tone: 'warn', tip: '依据：最近一次真实请求失败' };
  }

  return { key: 'unknown', label: '待验证', tone: 'mute', tip: '暂无本地证据（既无锁也无可用的最近请求），可点「探测」确认' };
}
