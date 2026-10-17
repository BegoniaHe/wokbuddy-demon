import { describe, expect, it } from 'vitest';
import { collectConfig, durationBad, dig, put } from './config';

describe('dig / put', () => {
  it('嵌套读写', () => {
    const o: Record<string, unknown> = {};
    put(o, ['schedule', 'checkin_hours'], [9, 21]);
    expect(dig(o, ['schedule', 'checkin_hours'])).toEqual([9, 21]);
    expect(dig(o, ['missing', 'x'])).toBeUndefined();
  });
});

describe('durationBad', () => {
  it('空值合法（沿用现值）', () => {
    expect(durationBad('')).toBe(false);
  });
  it('合法 Go 时长', () => {
    expect(durationBad('30m')).toBe(false);
    expect(durationBad('1h30m')).toBe(false);
    expect(durationBad('600s')).toBe(false);
  });
  it('脏值标红', () => {
    expect(durationBad('abc')).toBe(true);
    expect(durationBad('30 minutes')).toBe(true);
  });
});

describe('collectConfig', () => {
  it('空文本不下发（非覆盖型）', () => {
    const out = collectConfig({ listen: '' });
    expect('listen' in out).toBe(false);
  });
  it('覆盖型字段空串照发', () => {
    expect(collectConfig({ user_agent: '' })).toEqual({ upstream: { user_agent: '' } });
  });
  it('hours 字段拆分成数字数组', () => {
    expect(collectConfig({ checkin_hours: '9, 21' })).toEqual({ schedule: { checkin_hours: [9, 21] } });
  });
  it('number 字段转数值', () => {
    expect(collectConfig({ max_in_flight: '3' })).toEqual({ pool: { max_in_flight: 3 } });
    expect(collectConfig({ max_body_bytes: '0' })).toEqual({ server: { max_body_bytes: 0 } });
  });
  it('checkbox 转布尔', () => {
    expect(collectConfig({ prefer_expiring: true })).toEqual({ pool: { prefer_expiring: true } });
  });
  it('api_key 空不下发（避免误关鉴权）', () => {
    expect('api_key' in collectConfig({ api_key: '' })).toBe(false);
  });
});
