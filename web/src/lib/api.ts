// 面板 API 客户端。鉴权走 Bearer；401 时回调密钥门（由 UI 层注入）。
// 管理面密钥（/panel/api/*）。数据面 /v1/* 使用独立的 LS_GATEWAY_KEY，
// 避免把 admin/readonly 凭据发给网关数据面。
export const LS_KEY = 'wb2api.key';
export const LS_GATEWAY_KEY = 'wb2api.gateway_key';
export const LS_THEME = 'wb2api.theme';

let onUnauthorized: (() => void) | null = null;

export function setUnauthorizedHandler(fn: (() => void) | null): void {
  onUnauthorized = fn;
}

// notifyUnauthorized 由非 api() 的请求（如试聊直连 /v1）在 401 时触发密钥门。
export function notifyUnauthorized(): void {
  onUnauthorized?.();
}

export function getKey(): string | null {
  return localStorage.getItem(LS_KEY);
}

export function setKey(k: string): void {
  localStorage.setItem(LS_KEY, k);
}

// getGatewayKey 返回试聊/直连 /v1/* 使用的网关密钥（默认 api_key 或命名密钥）。
export function getGatewayKey(): string | null {
  return localStorage.getItem(LS_GATEWAY_KEY);
}

// setGatewayKey 保存/清除网关密钥；空串表示清除。
export function setGatewayKey(k: string): void {
  if (k) localStorage.setItem(LS_GATEWAY_KEY, k);
  else localStorage.removeItem(LS_GATEWAY_KEY);
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
