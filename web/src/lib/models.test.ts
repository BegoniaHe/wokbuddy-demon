import { describe, expect, it } from 'vitest';
import { DEFAULT_MODEL_FILTER, mdAvailability, mdMatch, mdSortList, type ModelEntry } from './models';
import { fmtLocalDateTime } from './format';

const base: ModelEntry = { id: 'cn:glm-5.2', name: 'GLM 5.2', vendor: 'zhipu' };

describe('mdMatch', () => {
  it('关键字多词 AND', () => {
    const m: ModelEntry = { ...base, description: '视觉 工具' };
    expect(mdMatch(m, { ...DEFAULT_MODEL_FILTER, q: 'glm 视觉' })).toBe(true);
    expect(mdMatch(m, { ...DEFAULT_MODEL_FILTER, q: 'glm 视觉 不存在' })).toBe(false);
  });
  it('realm 前缀匹配', () => {
    expect(mdMatch(base, { ...DEFAULT_MODEL_FILTER, realm: 'cn' })).toBe(true);
    expect(mdMatch(base, { ...DEFAULT_MODEL_FILTER, realm: 'global' })).toBe(false);
  });
  it('能力筛选', () => {
    const m: ModelEntry = { ...base, supports_tool_call: true, is_default: true };
    expect(mdMatch(m, { ...DEFAULT_MODEL_FILTER, cap: 'tool' })).toBe(true);
    expect(mdMatch(m, { ...DEFAULT_MODEL_FILTER, cap: 'vision' })).toBe(false);
    expect(mdMatch(m, { ...DEFAULT_MODEL_FILTER, cap: 'default' })).toBe(true);
  });
  it('思考档位 off / 具体档', () => {
    const m: ModelEntry = { ...base, can_disable_thinking: true, supported_efforts: ['low', 'high'] };
    expect(mdMatch(m, { ...DEFAULT_MODEL_FILTER, effort: 'off' })).toBe(true);
    expect(mdMatch(m, { ...DEFAULT_MODEL_FILTER, effort: 'high' })).toBe(true);
    expect(mdMatch(m, { ...DEFAULT_MODEL_FILTER, effort: 'max' })).toBe(false);
  });
  it('促销筛选', () => {
    const free: ModelEntry = { ...base, promo_factor: 0, promo_credits: 0 };
    expect(mdMatch(free, { ...DEFAULT_MODEL_FILTER, promo: 'free' })).toBe(true);
    expect(mdMatch(free, { ...DEFAULT_MODEL_FILTER, promo: 'discount' })).toBe(false);
    const disc: ModelEntry = { ...base, promo_factor: 0.5, promo_label: '夜间五折' };
    expect(mdMatch(disc, { ...DEFAULT_MODEL_FILTER, promo: 'discount' })).toBe(true);
    expect(mdMatch(disc, { ...DEFAULT_MODEL_FILTER, promo: 'promo' })).toBe(true);
  });
});

describe('mdSortList', () => {
  const list: ModelEntry[] = [
    { id: 'b', context_length: 100, max_output_tokens: 10, credits: 2 },
    { id: 'a', context_length: 200, max_output_tokens: 5, credits: 1 },
    { id: 'c', context_length: 50, max_output_tokens: 20 },
  ];
  it('不改动入参', () => {
    const copy = list.slice();
    mdSortList(list, { ...DEFAULT_MODEL_FILTER, sort: 'rate' });
    expect(list).toEqual(copy);
  });
  it('rate 升序，无价排最后', () => {
    const out = mdSortList(list, { ...DEFAULT_MODEL_FILTER, sort: 'rate' }).map((m) => m.id);
    expect(out).toEqual(['a', 'b', 'c']);
  });
  it('context 降序', () => {
    expect(mdSortList(list, { ...DEFAULT_MODEL_FILTER, sort: 'context' }).map((m) => m.id)).toEqual(['a', 'b', 'c']);
  });
  it('output 降序', () => {
    expect(mdSortList(list, { ...DEFAULT_MODEL_FILTER, sort: 'output' }).map((m) => m.id)).toEqual(['c', 'b', 'a']);
  });
  it('name A→Z', () => {
    expect(mdSortList(list, { ...DEFAULT_MODEL_FILTER, sort: 'name' }).map((m) => m.id)).toEqual(['a', 'b', 'c']);
  });
});

describe('mdAvailability（global 模型可用性）', () => {
  it('cn 模型返回 null（该标签只对 global 域有意义）', () => {
    expect(mdAvailability('cn:glm-5.2', [], [])).toBeNull();
    expect(mdAvailability('glm-5.2', [], [])).toBeNull();
  });

  it('两路证据都没有 → 待验证', () => {
    const a = mdAvailability('global:kimi-k3', [], []);
    expect(a?.key).toBe('unknown');
    expect(a?.label).toBe('待验证');
    expect(a?.tone).toBe('mute');
    expect(a?.tip).toContain('探测');
  });

  it('锁优先：partial → 部分可用', () => {
    const a = mdAvailability('global:kimi-k3', [{ model: 'kimi-k3', realm: 'global', state: 'partial', servable: 2, total: 3 }], []);
    expect(a?.key).toBe('partial');
    expect(a?.label).toBe('部分可用');
    expect(a?.tone).toBe('warn');
    expect(a?.tip).toContain('2/3');
  });

  it('锁优先：starved → 账号暂不可用', () => {
    const a = mdAvailability('global:kimi-k3', [{ model: 'kimi-k3', realm: 'global', state: 'starved', total: 2 }], []);
    expect(a?.key).toBe('starved');
    expect(a?.tone).toBe('warn');
  });

  it('锁优先：locked + 11158 → 无权限', () => {
    const a = mdAvailability('global:kimi-k3', [{ model: 'kimi-k3', realm: 'global', state: 'locked', reason: '11158 model not allowed' }], []);
    expect(a?.label).toBe('无权限');
    expect(a?.tone).toBe('bad');
  });

  it('锁优先：locked + 11102 → 上游无此模型', () => {
    const a = mdAvailability('global:kimi-k3', [{ model: 'kimi-k3', realm: 'global', state: 'locked', reason: '11102 service info not found' }], []);
    expect(a?.label).toBe('上游无此模型');
  });

  it('锁优先：locked 无具体码 → 限流不可用', () => {
    const a = mdAvailability('global:kimi-k3', [{ model: 'kimi-k3', realm: 'global', state: 'locked', reason: '6004 model rate limit', unlock_at: '2026-10-21T10:00:00Z' }], []);
    expect(a?.label).toBe('限流不可用');
    expect(a?.tip).toContain('解封');
  });

  it('无锁时看最近真实请求：成功/429/≥500/其他失败', () => {
    expect(
      mdAvailability('global:kimi-k3', [], [{ model: 'kimi-k3', status: 200, outcome: 'success' }])?.key,
    ).toBe('recent_ok');
    expect(mdAvailability('global:kimi-k3', [], [{ model: 'kimi-k3', status: 429 }])?.key).toBe('recent_429');
    expect(mdAvailability('global:kimi-k3', [], [{ model: 'kimi-k3', status: 503 }])?.key).toBe('recent_5xx');
    expect(mdAvailability('global:kimi-k3', [], [{ model: 'kimi-k3', status: 400 }])?.key).toBe('recent_fail');
  });

  it('锁优先于最近请求：有锁时忽略 recent', () => {
    const a = mdAvailability(
      'global:kimi-k3',
      [{ model: 'kimi-k3', realm: 'global', state: 'locked', reason: '6004 model rate limit' }],
      [{ model: 'kimi-k3', status: 200, outcome: 'success' }],
    );
    expect(a?.key).toBe('rate');
  });

  it('unlock_at 按本地时间渲染，而不是 UTC 原始串切片', () => {
    const iso = '2026-10-21T10:00:00Z';
    const a = mdAvailability(
      'global:kimi-k3',
      [{ model: 'kimi-k3', realm: 'global', state: 'locked', reason: '6004', unlock_at: iso }],
      [],
    );
    expect(a?.tip).toContain(fmtLocalDateTime(Date.parse(iso)));
  });

  it('unlock_at 零值时间不渲染 0001 日期', () => {
    const a = mdAvailability(
      'global:kimi-k3',
      [{ model: 'kimi-k3', realm: 'global', state: 'locked', reason: '6004', unlock_at: '0001-01-01T00:00:00Z' }],
      [],
    );
    expect(a?.tip).not.toContain('0001');
    expect(a?.tip).not.toContain('解封');
  });

  it('忽略其他域或其他模型的锁', () => {
    expect(
      mdAvailability('global:kimi-k3', [{ model: 'kimi-k3', realm: 'cn', state: 'locked' }], [])?.key,
    ).toBe('unknown');
    expect(
      mdAvailability('global:kimi-k3', [{ model: 'other', realm: 'global', state: 'locked' }], [])?.key,
    ).toBe('unknown');
  });
});
