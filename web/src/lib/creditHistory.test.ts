import { describe, expect, it } from 'vitest';
import { chDelta, chName, chTime, chTone } from './creditHistory';

describe('chName', () => {
  it('昵称优先', () => {
    expect(chName({ uid: '0123456789', nickname: '主力号' })).toBe('主力号');
  });
  it('无昵称回落短 uid', () => {
    expect(chName({ uid: '0123456789' })).toBe('01234567');
  });
  it('全空 → —', () => {
    expect(chName(undefined)).toBe('—');
    expect(chName({})).toBe('—');
  });
});

describe('chDelta / chTone', () => {
  it('带符号展示', () => {
    expect(chDelta(120)).toBe('+120');
    expect(chDelta(-30)).toBe('-30');
    expect(chDelta(0)).toBe('0');
    expect(chDelta(undefined)).toBe('0');
  });
  it('增=ok、减=warn、零=mute', () => {
    expect(chTone(5)).toBe('ok');
    expect(chTone(-5)).toBe('warn');
    expect(chTone(0)).toBe('mute');
  });
});

describe('chTime', () => {
  it('ISO → 本地 HH:MM:SS', () => {
    expect(chTime('2026-10-21T10:00:00Z')).toMatch(/^\d{2}:\d{2}:\d{2}$/);
  });
  it('空/非法 → 空串', () => {
    expect(chTime('')).toBe('');
    expect(chTime('not-a-time')).toBe('');
  });
});
