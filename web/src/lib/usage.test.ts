import { describe, expect, it } from 'vitest';
import { fmtTokTimeLabel, parsePointTime, type SeriesPoint } from './usage';

describe('parsePointTime / fmtTokTimeLabel', () => {
  it('小时桶（13 字符）正确解析，不产生 NaN:00', () => {
    const p: SeriesPoint = { t: '2026-10-16T06', scope: 'hour' };
    expect(parsePointTime(p)).not.toBeNull();
    expect(fmtTokTimeLabel(p)).toBe('06:00');
  });

  it('日桶渲染为 M-DD', () => {
    expect(fmtTokTimeLabel({ t: '2026-10-05', scope: 'day' })).toBe('10-05');
  });

  it('无法解析时回落原始串', () => {
    expect(fmtTokTimeLabel({ t: 'bad', scope: 'hour' })).toBe('bad');
  });
});
