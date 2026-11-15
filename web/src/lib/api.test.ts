// @vitest-environment jsdom
// api() 要读写 localStorage 并用 fetch：默认 environment 是 node，逐文件切到
// jsdom（同 useLocalFlag.test.ts 的做法，不改全局配置以免拖慢纯函数用例）。
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { api, LS_KEY, panelFetch, panelKeyValid, setKey, setUnauthorizedHandler } from './api';

const originalFetch = globalThis.fetch;

// mockFetch 用给定状态码与 JSON 体替换全局 fetch，并返回这个 spy 供断言调用参数。
function mockFetch(status: number, body: unknown = {}): ReturnType<typeof vi.fn> {
  const spy = vi.fn(async () => new Response(JSON.stringify(body), { status }));
  globalThis.fetch = spy as unknown as typeof fetch;

  return spy;
}

// calledHeaders 取出第 n 次 fetch 调用的 headers。
function calledHeaders(spy: ReturnType<typeof vi.fn>, n = 0): Record<string, string> {
  return (spy.mock.calls[n]?.[1] as RequestInit | undefined)?.headers as Record<string, string>;
}

describe('api', () => {
  beforeEach(() => localStorage.clear());

  afterEach(() => {
    globalThis.fetch = originalFetch;
    setUnauthorizedHandler(null);
  });

  it('带上管理面密钥', async () => {
    setKey('panel-key');
    const spy = mockFetch(200, { ok: true });

    await expect(api('overview')).resolves.toEqual({ ok: true });
    expect(spy.mock.calls[0]?.[0]).toBe('/panel/api/overview');
    expect(calledHeaders(spy)['Authorization']).toBe('Bearer panel-key');
  });

  it('无密钥时不发 Authorization', async () => {
    const spy = mockFetch(200, {});

    await api('overview');
    expect(calledHeaders(spy)['Authorization']).toBeUndefined();
  });

  it('有 body 时补 Content-Type: application/json', async () => {
    const spy = mockFetch(200, {});

    await api('config', { method: 'POST', body: '{"a":1}' });
    expect(calledHeaders(spy)['Content-Type']).toBe('application/json');
  });

  it('无 body 时不补 Content-Type', async () => {
    const spy = mockFetch(200, {});

    await api('overview');
    expect(calledHeaders(spy)['Content-Type']).toBeUndefined();
  });

  it('非 2xx 抛后端 error 文案', async () => {
    mockFetch(500, { error: '后端炸了' });

    await expect(api('overview')).rejects.toThrow('后端炸了');
  });

  it('非 2xx 且无 error 字段时抛 HTTP 状态', async () => {
    mockFetch(503, {});

    await expect(api('overview')).rejects.toThrow('HTTP 503');
  });

  it('401 触发密钥门并抛 status=401', async () => {
    const gate = vi.fn();
    setUnauthorizedHandler(gate);
    mockFetch(401, {});

    await expect(api('overview')).rejects.toMatchObject({ status: 401 });
    expect(gate).toHaveBeenCalledTimes(1);
    expect(localStorage.getItem(LS_KEY)).toBeNull();
  });

  it('2xx 但响应体不是 JSON 时返回空对象', async () => {
    globalThis.fetch = vi.fn(async () => new Response('not json', { status: 200 })) as unknown as typeof fetch;

    await expect(api('overview')).resolves.toEqual({});
  });
});

describe('panelFetch', () => {
  beforeEach(() => localStorage.clear());

  afterEach(() => {
    globalThis.fetch = originalFetch;
    setUnauthorizedHandler(null);
  });

  it('与 api() 同口径带管理面密钥', async () => {
    setKey('panel-key');
    const spy = mockFetch(200, {});

    await panelFetch('import/cockpit', { method: 'POST' });
    expect(spy.mock.calls[0]?.[0]).toBe('/panel/api/import/cockpit');
    expect(calledHeaders(spy)['Authorization']).toBe('Bearer panel-key');
  });

  it('不覆盖调用方给的 Content-Type（FormData 的 multipart 边界不能被改写）', async () => {
    const spy = mockFetch(200, {});

    await panelFetch('import/cockpit', { method: 'POST', headers: { 'Content-Type': 'multipart/form-data; boundary=x' } });
    expect(calledHeaders(spy)['Content-Type']).toBe('multipart/form-data; boundary=x');
  });

  it('不带 body 时也不补 Content-Type', async () => {
    const spy = mockFetch(200, {});

    await panelFetch('overview');
    expect(calledHeaders(spy)['Content-Type']).toBeUndefined();
  });

  it('401 触发密钥门并抛 status=401', async () => {
    const gate = vi.fn();
    setUnauthorizedHandler(gate);
    mockFetch(401, {});

    await expect(panelFetch('import/cockpit', { method: 'POST' })).rejects.toMatchObject({ status: 401 });
    expect(gate).toHaveBeenCalledTimes(1);
  });

  it('非 401 的失败原样返回 Response，由调用方决定错误文案', async () => {
    mockFetch(413, { error: 'too large' });

    const r = await panelFetch('import/cockpit', { method: 'POST' });
    expect(r.status).toBe(413);
    await expect(r.json()).resolves.toEqual({ error: 'too large' });
  });
});

describe('panelKeyValid', () => {
  beforeEach(() => localStorage.clear());

  afterEach(() => {
    globalThis.fetch = originalFetch;
    setUnauthorizedHandler(null);
  });

  it('密钥有效时返回 true', async () => {
    mockFetch(200, {});

    await expect(panelKeyValid('old-key')).resolves.toBe(true);
  });

  it('401 时返回 false 且不弹密钥门', async () => {
    const gate = vi.fn();
    setUnauthorizedHandler(gate);
    mockFetch(401, {});

    await expect(panelKeyValid('stale-key')).resolves.toBe(false);
    expect(gate).not.toHaveBeenCalled();
  });

  it('网络异常时返回 false', async () => {
    globalThis.fetch = vi.fn(async () => {
      throw new Error('boom');
    }) as unknown as typeof fetch;

    await expect(panelKeyValid('any')).resolves.toBe(false);
  });

  it('不带 Authorization 之外的额外请求头', async () => {
    const spy = mockFetch(200, {});

    await panelKeyValid('probe-key');
    expect(calledHeaders(spy)['Authorization']).toBe('Bearer probe-key');
    expect(calledHeaders(spy)['Content-Type']).toBeUndefined();
  });
});
