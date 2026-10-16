import { describe, expect, it } from 'vitest';
import { defaultTimeRangeState, trangeQuery } from './timeRange';

const st = (preset: string) => defaultTimeRangeState(preset);

describe('trangeQuery', () => {
  it('滚动预设发 hours', () => {
    expect(trangeQuery(st('24'), true).toString()).toBe('hours=24');
    expect(trangeQuery(st('72'), true).toString()).toBe('hours=72');
  });
  it('全部历史滚动端点显式 hours=0（避免被当成近 3 天）', () => {
    expect(trangeQuery(st('0'), true).toString()).toBe('hours=0');
  });
  it('全部历史非滚动端点不带参数', () => {
    expect(trangeQuery(st('0'), false).toString()).toBe('');
  });
  it('非滚动预设折算成 from', () => {
    const q = trangeQuery(st('24'), false);
    expect(q.has('from')).toBe(true);
    expect(q.has('hours')).toBe(false);
  });
  it('今天发本地 00:00 的 from', () => {
    const q = trangeQuery(st('today'), true);
    const from = Number(q.get('from'));
    const d = new Date(from * 1000);
    expect(d.getHours()).toBe(0);
    expect(d.getMinutes()).toBe(0);
  });
  it('自定义发 from/to', () => {
    const s = st('custom');
    s.from = new Date(2026, 0, 1, 0, 0);
    s.to = new Date(2026, 0, 2, 0, 0);
    const q = trangeQuery(s, true);
    expect(Number(q.get('to')) - Number(q.get('from'))).toBe(86400);
  });
});
