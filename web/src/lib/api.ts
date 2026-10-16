// 面板 API 客户端。鉴权走 Bearer；401 时回调密钥门（由 UI 层注入）。
export const LS_KEY = 'wb2api.key';
export const LS_THEME = 'wb2api.theme';

let onUnauthorized: (() => void) | null = null;

export function setUnauthorizedHandler(fn: (() => void) | null): void {
  onUnauthorized = fn;
}

export function getKey(): string | null {
  return localStorage.getItem(LS_KEY);
}

export function setKey(k: string): void {
  localStorage.setItem(LS_KEY, k);
}

export interface ApiError extends Error {
  status?: number;
}

// api 发起 /panel/api/<path> 请求；非 2xx 抛 Error（message 取后端 error 字段）。
export async function api<T = unknown>(path: string, opts: RequestInit = {}): Promise<T> {
  const h: Record<string, string> = { ...(opts.headers as Record<string, string> | undefined) };
  const k = getKey();
  if (k) h['Authorization'] = 'Bearer ' + k;
  if (opts.body) h['Content-Type'] = 'application/json';
  const r = await fetch('/panel/api/' + path, { ...opts, headers: h });
  if (r.status === 401) {
    onUnauthorized?.();
    const err: ApiError = new Error('密钥无效或未填写');
    err.status = 401;
    throw err;
  }
  const d = (await r.json().catch(() => ({}))) as { error?: string };
  if (!r.ok) {
    const err: ApiError = new Error(d.error || 'HTTP ' + r.status);
    err.status = r.status;
    throw err;
  }
  return d as T;
}

export function postJSON<T = unknown>(path: string, body?: unknown): Promise<T> {
  return api<T>(path, { method: 'POST', body: JSON.stringify(body ?? {}) });
}
