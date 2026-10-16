import { describe, expect, it } from 'vitest';
import {
  ago,
  dur,
  fmtCredit,
  fmtCreditRatio,
  fmtModelRate,
  fmtTok,
  formatLatency,
  formatRate,
  formatTokenCount,
  parseAPITime,
  rateLimitMeta,
  trimFixed,
} from './format';

describe('formatTokenCount', () => {
  it('小值原样、千分位升级单位', () => {
    expect(formatTokenCount(0)).toBe('0');
    expect(formatTokenCount(999)).toBe('999');
    expect(formatTokenCount(1500)).toBe('1.5k');
    expect(formatTokenCount(1_500_000)).toBe('1.5m');
  });
  it('四舍五入后自动升级单位（999999 → 1m）', () => {
    expect(formatTokenCount(999_999)).toBe('1m');
  });
  it('空值/负数 → —', () => {
    expect(formatTokenCount(null)).toBe('—');
    expect(formatTokenCount(-1)).toBe('—');
  });
});

describe('formatLatency / formatRate', () => {
  it('延迟 ms/s 切换', () => {
    expect(formatLatency(500)).toBe('500ms');
    expect(formatLatency(1500)).toBe('1.5s');
    expect(formatLatency(2000)).toBe('2s');
    expect(formatLatency(0)).toBe('—');
  });
  it('速率', () => {
    expect(formatRate(12.34)).toBe('12.3tok/s');
    expect(formatRate(null)).toBe('—');
  });
});

describe('dur', () => {
  it('时分秒', () => {
    expect(dur(45)).toBe('45秒');
    expect(dur(90)).toBe('1分30秒');
    expect(dur(3661)).toBe('1时01分');
  });
});

describe('parseAPITime / ago', () => {
  it('零值时间视作无', () => {
    expect(parseAPITime('0001-01-01T00:00:00Z')).toBe(0);
    expect(parseAPITime('')).toBe(0);
    expect(ago('0001-01-01T00:00:00Z')).toBe('—');
  });
});

describe('rateLimitMeta', () => {
  const now = Date.parse('2026-10-16T00:00:00Z');
  it('model_unavailable 走探测提示', () => {
    const m = rateLimitMeta({ model: 'glm-5.2', kind: 'model_unavailable' }, now);
    expect(m.model).toBe('glm-5.2');
    expect(m.detail).toBe('等待重新探测');
  });
  it('rate_limit 带 reset_at 给出解封时间', () => {
    const reset = new Date(now + 3600_000).toISOString();
    const m = rateLimitMeta({ model: 'glm-5.2', kind: 'rate_limit', reset_at: reset }, now);
    expect(m.detail).toContain('解封');
    expect(m.detail).toContain('剩余 1时00分');
  });
});

describe('用量页格式化', () => {
  it('fmtTok', () => {
    expect(fmtTok(999)).toBe('999');
    expect(fmtTok(1500)).toBe('1.5k');
    expect(fmtTok(2_000_000)).toBe('2.00M');
  });
  it('trimFixed/fmtCredit/fmtCreditRatio/fmtModelRate', () => {
    expect(trimFixed('1.5000')).toBe('1.5');
    expect(trimFixed('2')).toBe('2');
    expect(fmtCredit(3.1)).toBe('3.1');
    expect(fmtCreditRatio(0.5, 10, 1000)).toBe('0.5 / 1M');
    expect(fmtCreditRatio(0.5, 0, 1000)).toBe('—');
    expect(fmtModelRate('1.5')).toBe('x1.5');
    expect(fmtModelRate('')).toBe('—');
  });
});
