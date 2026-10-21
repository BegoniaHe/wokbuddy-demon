import { describe, expect, it } from 'vitest';
import { compDeadlineHours, compDeadlineTone, compName, compNote, compPending } from './compensation';

describe('compPending', () => {
  it('三与门：active 且未领且额度 > 0', () => {
    expect(compPending({ active: true, claimed: false, credit_num: 500 })).toBe(true);
    expect(compPending({ active: false, credit_num: 500 })).toBe(false);
    expect(compPending({ active: true, claimed: true, credit_num: 500 })).toBe(false);
    expect(compPending({ active: true, credit_num: 0 })).toBe(false);
  });
  it('跳过或查询失败一律不算待领（失败要显形，不能混进待领）', () => {
    expect(compPending({ active: true, credit_num: 100, skipped: true })).toBe(false);
    expect(compPending({ active: true, credit_num: 100, error: 'boom' })).toBe(false);
  });
  it('空值安全', () => {
    expect(compPending(undefined)).toBe(false);
  });
});

describe('compName', () => {
  it('昵称优先、其次短 uid、最后 —', () => {
    expect(compName({ uid: '0123456789', nickname: '主力号' })).toBe('主力号');
    expect(compName({ uid: '0123456789' })).toBe('01234567');
    expect(compName({})).toBe('—');
  });
});

describe('compDeadlineHours / tone', () => {
  // 用显式 +08:00 的「现在」，避免断言依赖测试机本地时区。
  const now = Date.parse('2026-10-21T00:00:00+08:00');

  it('解析上游无时区时间（按 UTC+8 墙钟）', () => {
    expect(compDeadlineHours('2026-10-22 00:00:00', now)).toBeCloseTo(24, 1);
  });

  it('无时区字符串按 +08:00 解释，不随浏览器时区漂移', () => {
    const utcMidnight = Date.parse('2026-10-21T00:00:00Z');
    // 2026-10-22 00:00 +08:00 = 2026-10-21 16:00Z。
    expect(compDeadlineHours('2026-10-22 00:00:00', utcMidnight)).toBeCloseTo(16, 1);
  });

  it('已带时区的 ISO 原样解析', () => {
    expect(compDeadlineHours('2026-10-22T00:00:00+08:00', now)).toBeCloseTo(24, 1);
  });
  it('无截止/非法 → null 且 tone=mute', () => {
    expect(compDeadlineHours('', now)).toBeNull();
    expect(compDeadlineHours('not-a-time', now)).toBeNull();
    expect(compDeadlineTone(null)).toBe('mute');
  });
  it('≤48h 标红、≤96h 警告', () => {
    expect(compDeadlineTone(24)).toBe('bad');
    expect(compDeadlineTone(48)).toBe('bad');
    expect(compDeadlineTone(72)).toBe('warn');
    expect(compDeadlineTone(200)).toBe('mute');
  });
});

describe('compNote', () => {
  it('失败必须先显形（全部失败时不得说成没活动）', () => {
    expect(compNote([{ error: 'down' }, { error: 'down' }])).toContain('2 个账号查询失败');
  });
  it('混合态同时说明待领与失败', () => {
    const note = compNote([
      { active: true, credit_num: 100 },
      { error: 'boom' },
      { skipped: true },
    ]);
    expect(note).toContain('1 个账号有待领补偿');
    expect(note).toContain('1 个账号查询失败');
    expect(note).toContain('1 个账号已跳过');
  });
  it('全空 → 当前没有待领补偿', () => {
    expect(compNote([])).toBe('当前没有待领补偿');
  });
});
