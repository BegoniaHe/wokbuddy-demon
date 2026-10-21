import { describe, expect, it } from 'vitest';
import {
  ago,
  dur,
  fmtClockTime,
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
  it('无法解析的时间串不产生 NaN 文案', () => {
    expect(ago('not-a-date')).toBe('—');
  });
});

describe('fmtClockTime', () => {
  it('空值/非法值回落 —', () => {
    expect(fmtClockTime('')).toBe('—');
    expect(fmtClockTime(undefined)).toBe('—');
    expect(fmtClockTime('not-a-date')).toBe('—');
  });
  it('有效 ISO 渲染为本地 HH:MM:SS', () => {
    expect(fmtClockTime('2026-10-16T09:24:03+02:00')).toMatch(/^\d{2}:\d{2}:\d{2}$/);
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
  it('双时钟（until < resetAt）：主文字给出两个时刻、倒计时跟 until', () => {
    const until = new Date(now + 600_000).toISOString(); // 网关 10 分钟后放行
    const reset = new Date(now + 7200_000).toISOString(); // 上游称 2 小时后恢复
    const m = rateLimitMeta({ model: 'glm-5.2', kind: 'rate_limit', until, reset_at: reset }, now);
    // 主文字同时出现「网关…起重试」与「上游称…恢复」
    expect(m.detail).toContain('网关');
    expect(m.detail).toContain('起重试');
    expect(m.detail).toContain('上游称');
    // 倒计时按 until（600s → 10分00秒），不是 resetAt（2 小时）
    expect(m.detail).toContain('剩余 10分');
    expect(m.detail).not.toContain('2时00分');
    // title 保留两个完整时刻
    expect(m.title).toContain('网关最早重试：');
    expect(m.title).toContain('上游重置：');
  });
  it('无 reset_at 时倒计时跟 until', () => {
    const until = new Date(now + 300_000).toISOString();
    const m = rateLimitMeta({ model: 'glm-5.2', kind: 'rate_limit', until }, now);
    expect(m.detail).toContain('恢复');
    expect(m.detail).toContain('剩余 5分');
  });
  it('until >= reset_at 不是双时钟，回到既有文案', () => {
    const until = new Date(now + 7200_000).toISOString();
    const reset = new Date(now + 3600_000).toISOString();
    const m = rateLimitMeta({ model: 'glm-5.2', kind: 'rate_limit', until, reset_at: reset }, now);
    expect(m.detail).toContain('解封');
    expect(m.detail).toContain('剩余 1时00分');
    expect(m.detail).not.toContain('上游称');
  });
  it('model_unavailable 带 until 时倒计时跟 until', () => {
    const until = new Date(now + 600_000).toISOString();
    const m = rateLimitMeta({ model: 'glm-5.2', kind: 'model_unavailable', until }, now);
    expect(m.detail).toContain('预计');
    expect(m.detail).toContain('10分');
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
