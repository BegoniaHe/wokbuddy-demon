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

// panelFetch 发起 /panel/api/<path> 原始请求并返回 Response，响应体交给调用方解析。
//
// api() 覆盖不了两类调用：带 FormData 的上传（api() 在有 body 时强制
// Content-Type: application/json，会打断 multipart 边界）与二进制下载（api() 只解析
// JSON）。这两类此前各自手拼 Authorization，绕开了 401 的密钥门——管理密钥失效时
// 用户只看到「HTTP 401」，不知道要重新填密钥。这里把鉴权与 401 口径收回同一处。
export async function panelFetch(path: string, init: RequestInit = {}): Promise<Response> {
  const h: Record<string, string> = { ...(init.headers as Record<string, string> | undefined) };
  const k = getKey();
  if (k) h['Authorization'] = 'Bearer ' + k;
  const r = await fetch('/panel/api/' + path, { ...init, headers: h });
  if (r.status === 401) {
    onUnauthorized?.();
    const err: ApiError = new Error('密钥无效或未填写');
    err.status = 401;
    throw err;
  }
  return r;
}

// panelKeyValid 探测某把管理面密钥是否仍被接受。
//
// 用于「保存配置后决定是否替换现有凭据」：这里的 401 是预期结果之一，不能弹密钥门，
// 所以刻意不走 panelFetch。
export async function panelKeyValid(k: string): Promise<boolean> {
  try {
    const r = await fetch('/panel/api/overview', { headers: { Authorization: 'Bearer ' + k } });
    return r.ok;
  } catch {
    return false;
  }
}

// api 发起 /panel/api/<path> 请求；非 2xx 抛 Error（message 取后端 error 字段）。
export async function api<T = unknown>(path: string, opts: RequestInit = {}): Promise<T> {
  const h: Record<string, string> = { ...(opts.headers as Record<string, string> | undefined) };
  if (opts.body) h['Content-Type'] = 'application/json';
  const r = await panelFetch(path, { ...opts, headers: h });
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
