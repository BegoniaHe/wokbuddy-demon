import { describe, expect, it } from 'vitest';
import { expBatches, expBucketsFromSnapshot } from './expiry';


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
