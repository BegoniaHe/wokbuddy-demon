import { describe, expect, it } from 'vitest';
import { DEFAULT_MODEL_FILTER, mdMatch, mdSortList, type ModelEntry } from './models';

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
