import { describe, expect, it } from 'vitest';
import { pkBySource, pkDetailGroups, pkAccountSegments, pkExpiryMs, type Pkg } from './packages';

describe('pkBySource', () => {
  it('按 code+name 归并并累加面额', () => {
    const packs: Pkg[] = [
      { package_code: 'A', name: '拉新包', size: 100, remain: 50, used: 50 },
      { package_code: 'A', name: '拉新包', size: 200, remain: 200, used: 0 },
      { package_code: 'B', name: '体验版', size: 6, remain: 6, used: 0 },
    ];
    const out = pkBySource(packs);
    expect(out[0]!.name).toBe('拉新包');
    expect(out[0]!.size).toBe(300);
    expect(out[0]!.n).toBe(2);
    expect(out[0]!.remain).toBe(250);
  });
});

describe('pkExpiryMs', () => {
  it('优先用 expires_at', () => {
    expect(pkExpiryMs({ expires_at: 1234 })).toBe(1234);
  });
  it('解析 end_time（补 +08:00）', () => {
    const ms = pkExpiryMs({ end_time: '2026-11-01 00:00:00' });
    expect(ms).toBe(Date.parse('2026-11-01T00:00:00+08:00'));
  });
  it('空 → null', () => {
    expect(pkExpiryMs({})).toBeNull();
  });
});

describe('pkDetailGroups', () => {
  const packs: Pkg[] = [
    { name: 'a', size: 100, remain: 50, end_time: '2026-12-01 00:00:00' },
    { name: 'b', size: 80, remain: 40, end_time: '2026-11-01 00:00:00' },
    { name: 'c', size: 60, remain: 30, end_time: '2026-10-01 00:00:00' },
    { name: 'd', size: 10, remain: 0 },
    { name: 'e', size: 20, remain: 0 },
  ];
  it('默认展示最早到期 N 条，其余折叠', () => {
    const g = pkDetailGroups(packs, 2, 'end_asc');
    expect(g.visible.map((p) => p.name)).toEqual(['c', 'b']);
    expect(g.rest.map((p) => p.name)).toEqual(['a']);
    expect(g.used.map((p) => p.name)).toEqual(['e', 'd']);
    expect(g.usedSize).toBe(30);
    expect(g.restRemain).toBe(50);
  });
  it('size_desc 面额降序', () => {
    const g = pkDetailGroups(packs, 5, 'size_desc');
    expect(g.visible.map((p) => p.name)).toEqual(['a', 'b', 'c']);
  });
});

describe('pkAccountSegments', () => {
  it('按余额约束逐包切分并升序排列', () => {
    const a = {
      uid: 'u1',
      remain: 100,
      packages: [
        { name: 'late', remain: 100, end_time: '2026-12-01 00:00:00' },
        { name: 'early', remain: 100, end_time: '2026-11-01 00:00:00' },
      ],
    };
    const now = Date.parse('2026-10-16T00:00:00+08:00');
    const segs = pkAccountSegments(a, now);
    // 余额 100 先满足第一个包（late，100），后续包余额耗尽
    expect(segs).toHaveLength(1);
    expect(segs[0]!.amount).toBe(100);
  });
});
