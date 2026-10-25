import { describe, expect, it } from 'vitest';
import {
  collectConfig,
  configToValues,
  durationBad,
  durationFieldInvalid,
  durationIsZero,
  durationTip,
  dig,
  put,
} from './config';

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
    // time.ParseDuration("0") 合法；时序整形把 "0" 作为关闭回滚值。
    expect(durationBad('0')).toBe(false);
  });
  it('脏值标红', () => {
    expect(durationBad('abc')).toBe(true);
    expect(durationBad('30 minutes')).toBe(true);
  });
});

describe('durationIsZero', () => {
  it('识别零值 Go 时长', () => {
    expect(durationIsZero('0')).toBe(true);
    expect(durationIsZero('0s')).toBe(true);
    expect(durationIsZero('0h0m')).toBe(true);
    expect(durationIsZero('0.0h')).toBe(true);
  });
  it('非零与空值', () => {
    expect(durationIsZero('')).toBe(false);
    expect(durationIsZero('0h30m')).toBe(false);
    expect(durationIsZero('1h')).toBe(false);
  });
});

describe('durationFieldInvalid', () => {
  it('普通时长字段允许 0', () => {
    expect(durationFieldInvalid('slot_jitter', '0')).toBe(false);
  });
  it('临期窗口必须大于 0', () => {
    expect(durationFieldInvalid('keepalive_refresh_window', '0')).toBe(true);
    expect(durationFieldInvalid('keepalive_refresh_window', '0h0m')).toBe(true);
    expect(durationFieldInvalid('keepalive_refresh_window', '24h')).toBe(false);
    expect(durationFieldInvalid('keepalive_refresh_window', '')).toBe(false);
  });
  it('正时长字段仍校验格式', () => {
    expect(durationFieldInvalid('keepalive_refresh_window', 'abc')).toBe(true);
  });
});

describe('durationTip', () => {
  it('正时长字段不复用「0 = 关闭」提示', () => {
    expect(durationTip('keepalive_refresh_window')).not.toContain('0 = 关闭');
    expect(durationTip('slot_jitter')).toContain('0 = 关闭');
  });
});

describe('collectConfig', () => {
  it('空文本不下发（非覆盖型）', () => {
    const out = collectConfig({ listen: '' });
    expect('listen' in out).toBe(false);
  });
  it('覆盖型字段空串照发', () => {
    expect(collectConfig({ user_agent: '' })).toEqual({ upstream: { user_agent: '' } });
    expect(collectConfig({ timing_salt: '' })).toEqual({ schedule: { timing_salt: '' } });
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
  it('list 字段拆成字符串数组', () => {
    expect(collectConfig({ proxies: 'http://a, socks5://b' })).toEqual({
      upstream: { proxies: ['http://a', 'socks5://b'] },
    });
  });
  it('数字列表字段复用 hours 拆分', () => {
    expect(collectConfig({ rotate_status_codes: '429, 500' })).toEqual({
      upstream: { rotate_status_codes: [429, 500] },
    });
  });
  it('时序整形字段拆分正确', () => {
    expect(
      collectConfig({
        slot_jitter: '20m',
        account_pacing_sigma: '0.4',
        account_pacing_floor: '250ms',
        account_pacing_ceiling: '6s',
        account_phase_window: '10m',
        timing_salt: 'abc',
      }),
    ).toEqual({
      schedule: {
        slot_jitter: '20m',
        account_pacing_sigma: 0.4,
        account_pacing_floor: '250ms',
        account_pacing_ceiling: '6s',
        account_phase_window: '10m',
        timing_salt: 'abc',
      },
    });
  });
  it('secret 字段空值不下发（保留磁盘原值）', () => {
    expect('device_token' in collectConfig({ device_token: '' })).toBe(false);
    expect('upstash_token' in collectConfig({ upstash_token: '' })).toBe(false);
  });
});

describe('configToValues', () => {
  it('secret 字段已有值时留空，普通字段照常回填', () => {
    const v = configToValues({
      listen: ':7863',
      upstream: { device_token: 'tok' },
      upstash: { token: 'redis-token' },
    });
    expect(v.listen).toBe(':7863');
    expect(v.device_token).toBe('');
    expect(v.upstash_token).toBe('');
  });
});
