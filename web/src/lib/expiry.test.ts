import { describe, expect, it } from 'vitest';
import { expBatches, expBucketsFromSnapshot, expTotals } from './expiry';

const today = new Date('2026-10-28T00:00:00');

describe('expBucketsFromSnapshot（快照批次与「检查」同口径）', () => {
  it('过滤空日期与零剩余，按日期升序', () => {
    const out = expBucketsFromSnapshot([
      { date: '2026-11-02', remain: 120 },
      { date: '', remain: 50 },
      { date: '2026-10-28', remain: 0 },
      { date: '2026-10-30', remain: 80 },
    ]);
    expect(out).toEqual([
      { date: '2026-10-30', remain: 80 },
      { date: '2026-11-02', remain: 120 },
    ]);
  });

  it('expire_at 存在时以 date 为准（同口径只看自然日）', () => {
    const out = expBucketsFromSnapshot([{ date: '2026-10-28', expire_at: '2026-10-28T12:00:00Z', remain: 30 }]);
    expect(out).toEqual([{ date: '2026-10-28', remain: 30 }]);
  });

  it('空/缺失 → 空列表（调用方回落「最早一批」）', () => {
    expect(expBucketsFromSnapshot(undefined)).toEqual([]);
    expect(expBucketsFromSnapshot([])).toEqual([]);
  });

  it('与 expBatches 对同一数据输出一致', () => {
    const buckets = [
      { date: '2026-10-28', remain: 30 },
      { date: '2026-10-28', remain: 20 },
      { date: '2026-11-01', remain: 50 },
    ];
    const asPackages = buckets.map((b) => ({ remain: b.remain, end_time: b.date }));
    expect(expBucketsFromSnapshot(buckets)).toEqual(expBatches(asPackages));
  });
});

describe('expTotals（跨账号自然日合计）', () => {
  it('两账号同日到期相加，accounts 计 2', () => {
    const out = expTotals(
      [
        { realm: 'cn', packages: [{ remain: 100, end_time: '2026-10-30' }] },
        { realm: 'cn', packages: [{ remain: 23, end_time: '2026-10-30' }] },
      ],
      today,
    );
    expect(out).toHaveLength(1);
    expect(out[0]!.realm).toBe('cn');
    expect(out[0]!.accounts).toBe(2);
    expect(out[0]!.rows).toEqual([{ date: '2026-10-30', remain: 123, accounts: 2 }]);
  });

  it('同账号同日两批只计 1 个账号', () => {
    const out = expTotals(
      [
        {
          realm: 'cn',
          packages: [
            { remain: 40, end_time: '2026-10-30T08:00:00Z' },
            { remain: 60, end_time: '2026-10-30T20:00:00Z' },
          ],
        },
      ],
      today,
    );
    expect(out[0]!.accounts).toBe(1);
    expect(out[0]!.rows).toEqual([{ date: '2026-10-30', remain: 100, accounts: 1 }]);
  });

  it('剔除过期日、零剩余与空日期', () => {
    const out = expTotals(
      [
        {
          realm: 'cn',
          packages: [
            { remain: 500, end_time: '2026-10-27' },
            { remain: 0, end_time: '2026-10-31' },
            { remain: 70, end_time: '' },
            { remain: 30, end_time: '2026-10-28' },
          ],
        },
      ],
      today,
    );
    expect(out[0]!.rows).toEqual([{ date: '2026-10-28', remain: 30, accounts: 1 }]);
  });

  it('realm 缺失按 cn；cn 与 global 分块、块内升序、cn 在前', () => {
    const out = expTotals(
      [
        { realm: 'global', packages: [{ remain: 10, end_time: '2026-11-02' }] },
        { packages: [{ remain: 20, end_time: '2026-10-30' }] },
        { realm: 'global', packages: [{ remain: 5, end_time: '2026-10-30' }] },
        { realm: 'cn', packages: [{ remain: 7, end_time: '2026-11-02' }] },
      ],
      today,
    );
    expect(out.map((r) => r.realm)).toEqual(['cn', 'global']);
    expect(out[0]!.rows).toEqual([
      { date: '2026-10-30', remain: 20, accounts: 1 },
      { date: '2026-11-02', remain: 7, accounts: 1 },
    ]);
    expect(out[1]!.rows).toEqual([
      { date: '2026-10-30', remain: 5, accounts: 1 },
      { date: '2026-11-02', remain: 10, accounts: 1 },
    ]);
    expect(out[0]!.accounts).toBe(2);
    expect(out[1]!.accounts).toBe(2);
  });

  it('error 账号被跳过', () => {
    const out = expTotals(
      [
        { realm: 'cn', error: '查询失败', packages: [{ remain: 999, end_time: '2026-10-30' }] },
        { realm: 'cn', packages: [{ remain: 10, end_time: '2026-10-30' }] },
      ],
      today,
    );
    expect(out[0]!.rows).toEqual([{ date: '2026-10-30', remain: 10, accounts: 1 }]);
    expect(out[0]!.accounts).toBe(1);
  });

  it('空输入 → 空列表（调用方据此判定无数据）', () => {
    expect(expTotals([], today)).toEqual([]);
    expect(expTotals([{ realm: 'cn', packages: [] }], today)).toEqual([]);
  });

  it('单一 realm 时只返回一块（调用方据此隐藏域标题）', () => {
    const out = expTotals(
      [
        { realm: 'cn', packages: [{ remain: 10, end_time: '2026-10-30' }] },
        { realm: 'cn', packages: [{ remain: 10, end_time: '2026-11-02' }] },
      ],
      today,
    );
    expect(out).toHaveLength(1);
  });
});
