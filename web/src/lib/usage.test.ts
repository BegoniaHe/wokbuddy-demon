import { describe, expect, it } from 'vitest';
import { chartLayout, fmtTokTimeLabel, parsePointTime, usDayHeatmap, usTrendPoints, type SeriesPoint } from './usage';

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


describe('usDayHeatmap（issue #111）', () => {
  const series: SeriesPoint[] = [
    { t: '2026-10-04', scope: 'day', requests: 2, total_tokens: 100, credits: 1.5 },
    { t: '2026-10-05', scope: 'day', requests: 3, total_tokens: 250, credits: 0 },
    { t: '2026-10-07', scope: 'day', requests: 1, total_tokens: 50, credits: 2 },
    // 小时桶必须被忽略（避免同一数据在日/小时两个维度重复计入）。
    { t: '2026-10-05T10', scope: 'hour', requests: 99, total_tokens: 999 },
  ];

  it('按日聚合、连续范围、缺失日不补零', () => {
    const days = usDayHeatmap(series, 'total_tokens');
    expect(days.map((d) => d.date)).toEqual(['2026-10-04', '2026-10-05', '2026-10-06', '2026-10-07']);
    expect(days.map((d) => d.value)).toEqual([100, 250, undefined, 50]);
  });

  it('切换指标（请求数 / 积分）', () => {
    expect(usDayHeatmap(series, 'requests').map((d) => d.value)).toEqual([2, 3, undefined, 1]);
    const credits = usDayHeatmap(series, 'credits');
    expect(credits[0]!.value).toBe(1.5);
    expect(credits[1]!.value).toBe(0); // 0 是真实值，不是无数据
    expect(credits[2]!.value).toBeUndefined();
  });

  it('空数据返回空数组（不伪造日历）', () => {
    expect(usDayHeatmap([], 'requests')).toEqual([]);
    expect(usDayHeatmap([{ t: '2026-10-05T10', scope: 'hour', requests: 1 }], 'requests')).toEqual([]);
  });

  it('范围超过 maxDays 时只保留最近 N 天', () => {
    const long: SeriesPoint[] = [
      { t: '2024-01-01', scope: 'day', requests: 1 },
      { t: '2026-10-10', scope: 'day', requests: 2 },
    ];
    const days = usDayHeatmap(long, 'requests', 30);
    expect(days.length).toBe(30);
    expect(days[days.length - 1]!.date).toBe('2026-10-10');
  });

  it('跨时区不重算日键：服务端日键原样保留', () => {
    const days = usDayHeatmap([{ t: '2026-03-08', scope: 'day', requests: 1 }], 'requests');
    expect(days).toEqual([{ date: '2026-03-08', value: 1 }]);
  });

  it('日期键非法时跳过', () => {
    expect(usDayHeatmap([{ t: 'bad', scope: 'day', requests: 1 }], 'requests')).toEqual([]);
  });
});

describe('usTrendPoints（issue #111）', () => {
  const series: SeriesPoint[] = [
    { t: '2026-10-05', scope: 'day', prompt_tokens: 10, completion_tokens: 5, requests: 2, errors: 1, avg_ttft_ms: 120, credits: 1 },
    { t: '2026-10-06', scope: 'day', total_tokens: 30, requests: 3, errors: 0, credits: 2 },
    { t: 'bad', scope: 'hour', requests: 9 },
  ];

  it('tokens 指标：total_tokens 优先，缺失时 prompt+completion', () => {
    expect(usTrendPoints(series, 'tokens').map((p) => p.value)).toEqual([15, 30]);
  });

  it('请求数 / 失败数 / 积分：缺失字段跳过而不是补零', () => {
    expect(usTrendPoints(series, 'requests').map((p) => p.value)).toEqual([2, 3]);
    expect(usTrendPoints(series, 'errors').map((p) => p.value)).toEqual([1, 0]);
    expect(usTrendPoints(series, 'credits').map((p) => p.value)).toEqual([1, 2]);
  });

  it('TTFT：无样本的点跳过', () => {
    const pts = usTrendPoints(series, 'ttft');
    expect(pts.map((p) => p.value)).toEqual([120]);
  });

  it('空数据返回空数组', () => {
    expect(usTrendPoints([], 'tokens')).toEqual([]);
  });
});

describe('chartLayout（窄屏时序图）', () => {
  it('宽屏保持既有 1200×200 / 6 刻度（零回归）', () => {
    const l = chartLayout(false);
    expect(l.W).toBe(1200);
    expect(l.H).toBe(200);
    expect(l.ticks).toBe(6);
    expect(l.PL).toBe(58);
    expect(l.PR).toBe(14);
    expect(l.iw).toBe(1200 - 58 - 14);
    expect(l.ih).toBe(200 - 18 - 30);
  });

  it('窄屏 440×230 / 4 刻度，绘制区仍为正', () => {
    const l = chartLayout(true);
    expect(l.W).toBe(440);
    expect(l.H).toBe(230);
    expect(l.ticks).toBe(4);
    expect(l.iw).toBeGreaterThan(0);
    expect(l.ih).toBeGreaterThan(0);
  });

  it('窄屏绘制区不被边距吃掉（iw/ih 与参数一致）', () => {
    const l = chartLayout(true);
    expect(l.iw).toBe(l.W - l.PL - l.PR);
    expect(l.ih).toBe(l.H - l.PT - l.PB);
  });
});
