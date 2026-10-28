import { describe, expect, it } from 'vitest';
import { MAX_IMPORT_MODELS, countByRealm, modelRealm, selectModelIDs } from './agents';

const entries = [
  { id: 'cn:glm-5.2' },
  { id: 'cn:glm-5.3' },
  { id: 'global:kimi-k3' },
  { id: 'deepseek-v4.1-flash' },
  { id: 'cn:glm-5.2' },
  { id: '   ' },
];

describe('modelRealm', () => {
  it('显式 global: 才算国际版，裸名按国内版', () => {
    expect(modelRealm('global:kimi-k3')).toBe('global');
    expect(modelRealm('cn:glm-5.2')).toBe('cn');
    expect(modelRealm('deepseek-v4.1-flash')).toBe('cn');
  });
});

describe('selectModelIDs', () => {
  it('全部：去重保序并丢弃空 id', () => {
    expect(selectModelIDs(entries, 'all')).toEqual([
      'cn:glm-5.2',
      'cn:glm-5.3',
      'global:kimi-k3',
      'deepseek-v4.1-flash',
    ]);
  });

  it('按域过滤', () => {
    expect(selectModelIDs(entries, 'cn')).toEqual(['cn:glm-5.2', 'cn:glm-5.3', 'deepseek-v4.1-flash']);
    expect(selectModelIDs(entries, 'global')).toEqual(['global:kimi-k3']);
  });

  it('上限截断（与后端同口径 200）', () => {
    const many = Array.from({ length: MAX_IMPORT_MODELS + 50 }, (_, i) => ({ id: 'cn:m-' + i }));
    expect(selectModelIDs(many, 'all')).toHaveLength(MAX_IMPORT_MODELS);
  });

  it('空输入 → 空列表', () => {
    expect(selectModelIDs(undefined, 'all')).toEqual([]);
    expect(selectModelIDs([], 'cn')).toEqual([]);
  });
});

describe('countByRealm', () => {
  it('按域计数', () => {
    expect(countByRealm(['cn:a', 'cn:b', 'global:c'])).toEqual({ cn: 2, global: 1 });
    expect(countByRealm([])).toEqual({ cn: 0, global: 0 });
  });
});
