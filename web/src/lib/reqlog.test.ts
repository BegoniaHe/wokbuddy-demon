import { describe, expect, it } from 'vitest';
import { cacheRateText, fmtBytes, reqMatch, reqTokenCell, type RequestEntry } from './reqlog';

describe('reqMatch', () => {
  const e: RequestEntry = {
    client_ip: '1.2.3.4',
    user_agent: 'CodeBuddy/2.63.2',
    model: 'glm-5.2',
    account: 'alice',
    request_id: 'req-123',
    outcome: 'success',
  };
  it('多关键词 AND 且大小写不敏感', () => {
    expect(reqMatch(e, { q: 'alice glm', outcome: '' })).toBe(true);
    expect(reqMatch(e, { q: 'alice 不存在', outcome: '' })).toBe(false);
  });
  it('outcome 精确匹配', () => {
    expect(reqMatch(e, { q: '', outcome: 'success' })).toBe(true);
    expect(reqMatch(e, { q: '', outcome: 'http_error' })).toBe(false);
  });
});

describe('reqTokenCell', () => {
  it('total 优先，否则 prompt+completion', () => {
    expect(reqTokenCell({ total_tokens: 1500 })).toBe('1.5k');
    expect(reqTokenCell({ prompt_tokens: 1000, completion_tokens: 500 })).toBe('1.5k');
    expect(reqTokenCell({})).toBe('—');
  });
});

describe('cacheRateText', () => {
  it('无样本 —', () => {
    expect(cacheRateText(0, 0)).toBe('—');
  });
  it('百分比一位小数', () => {
    expect(cacheRateText(9, 1)).toBe('90%');
    expect(cacheRateText(1, 2)).toBe('33.3%');
  });
});

describe('fmtBytes', () => {
  it('单位切换', () => {
    expect(fmtBytes(512)).toBe('512 B');
    expect(fmtBytes(2048)).toBe('2.0 KB');
    expect(fmtBytes(2 * 1024 * 1024)).toBe('2.0 MB');
  });
});
