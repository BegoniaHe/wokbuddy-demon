// 配置页字段映射与收集逻辑（纯函数）。

export type FieldKind = 'text' | 'number' | 'password' | 'select' | 'checkbox' | 'hours' | 'list';

export interface FieldDef {
  name: string;
  path: string[];
  kind: FieldKind;
  // secret 字段加载时不回填已保存值（输入框留空 = 保持原值），避免敏感值进入 DOM。
  secret?: boolean;
}

export const CFG_FIELDS: FieldDef[] = [
  { name: 'listen', path: ['listen'], kind: 'text' },
  { name: 'api_key', path: ['api_key'], kind: 'password' },
  { name: 'auth_dir', path: ['auth_dir'], kind: 'text' },
  { name: 'state_file', path: ['state_file'], kind: 'text' },
  { name: 'package_detail_limit', path: ['panel', 'package_detail_limit'], kind: 'number' },
  { name: 'max_body_bytes', path: ['server', 'max_body_bytes'], kind: 'number' },
  { name: 'request_accept_grace_seconds', path: ['server', 'request_accept_grace_seconds'], kind: 'number' },
  { name: 'shutdown_grace_seconds', path: ['server', 'shutdown_grace_seconds'], kind: 'number' },
  { name: 'checkin_enabled', path: ['schedule', 'checkin_enabled'], kind: 'checkbox' },
  { name: 'checkin_hours', path: ['schedule', 'checkin_hours'], kind: 'hours' },
  { name: 'keepalive_enabled', path: ['schedule', 'keepalive_enabled'], kind: 'checkbox' },
  { name: 'keepalive_hours', path: ['schedule', 'keepalive_hours'], kind: 'hours' },
  { name: 'travel_enabled', path: ['schedule', 'travel_enabled'], kind: 'checkbox' },
  { name: 'travel_hours', path: ['schedule', 'travel_hours'], kind: 'hours' },
  { name: 'activity_enabled', path: ['schedule', 'activity_enabled'], kind: 'checkbox' },
  { name: 'activity_hours', path: ['schedule', 'activity_hours'], kind: 'hours' },
  { name: 'balance_refresh_enabled', path: ['schedule', 'balance_refresh_enabled'], kind: 'checkbox' },
  { name: 'balance_refresh_minutes', path: ['schedule', 'balance_refresh_minutes'], kind: 'number' },
  { name: 'task_sweep_enabled', path: ['schedule', 'task_sweep_enabled'], kind: 'checkbox' },
  { name: 'task_sweep_minutes', path: ['schedule', 'task_sweep_minutes'], kind: 'number' },
  { name: 'checkin_account_delay', path: ['schedule', 'checkin_account_delay'], kind: 'text' },
  { name: 'growth_enabled', path: ['schedule', 'growth_enabled'], kind: 'checkbox' },
  { name: 'growth_hours', path: ['schedule', 'growth_hours'], kind: 'hours' },
  { name: 'include_disabled_in_tasks', path: ['schedule', 'include_disabled_in_tasks'], kind: 'checkbox' },
  { name: 'max_in_flight', path: ['pool', 'max_in_flight'], kind: 'number' },
  { name: 'max_in_flight_global', path: ['pool', 'max_in_flight_global'], kind: 'number' },
  { name: 'max_in_flight_total', path: ['pool', 'max_in_flight_total'], kind: 'number' },
  { name: 'breaker_threshold', path: ['pool', 'breaker_threshold'], kind: 'number' },
  { name: 'soft_rate', path: ['cooldown', 'soft_rate'], kind: 'text' },
  { name: 'soft_rate_max', path: ['cooldown', 'soft_rate_max'], kind: 'text' },
  { name: 'breaker_cooldown', path: ['pool', 'breaker_cooldown'], kind: 'text' },
  { name: 'breaker_cooldown_max', path: ['pool', 'breaker_cooldown_max'], kind: 'text' },
  { name: 'degrade_threshold', path: ['pool', 'degrade_threshold'], kind: 'number' },
  { name: 'degrade_cooldown', path: ['pool', 'degrade_cooldown'], kind: 'text' },
  { name: 'degrade_cooldown_max', path: ['pool', 'degrade_cooldown_max'], kind: 'text' },
  { name: 'idle_weight_per_hour', path: ['pool', 'idle_weight_per_hour'], kind: 'number' },
  { name: 'idle_weight_max', path: ['pool', 'idle_weight_max'], kind: 'number' },
  { name: 'cost_explore_interval', path: ['pool', 'cost_explore_interval'], kind: 'text' },
  { name: 'credit_floor', path: ['pool', 'credit_floor'], kind: 'number' },
  { name: 'daily_tokens', path: ['limits', 'daily_tokens'], kind: 'number' },
  { name: 'daily_credits', path: ['limits', 'daily_credits'], kind: 'number' },
  { name: 'model_daily_tokens', path: ['limits', 'model_daily_tokens'], kind: 'number' },
  { name: 'expiring_soon', path: ['pool', 'expiring_soon'], kind: 'text' },
  { name: 'ttl', path: ['session_sticky', 'ttl'], kind: 'text' },
  { name: 'prefer_expiring', path: ['pool', 'prefer_expiring'], kind: 'checkbox' },
  { name: 'cross_domain_fallback', path: ['pool', 'cross_domain_fallback'], kind: 'checkbox' },
  { name: 'timeout_seconds', path: ['upstream', 'timeout_seconds'], kind: 'number' },
  { name: 'header_timeout_seconds', path: ['upstream', 'header_timeout_seconds'], kind: 'number' },
  { name: 'idle_timeout_seconds', path: ['upstream', 'idle_timeout_seconds'], kind: 'number' },
  { name: 'user_agent', path: ['upstream', 'user_agent'], kind: 'text' },
  { name: 'background_names', path: ['models', 'background_names'], kind: 'list' },
  { name: 'background_target', path: ['models', 'background_target'], kind: 'text' },
  { name: 'hidden_models', path: ['models', 'hidden'], kind: 'list' },
  { name: 'client_version', path: ['upstream', 'client_version'], kind: 'text' },
  { name: 'cli_version', path: ['upstream', 'cli_version'], kind: 'text' },
  { name: 'client_name', path: ['upstream', 'client_name'], kind: 'text' },
  { name: 'device_token', path: ['upstream', 'device_token'], kind: 'password', secret: true },
  { name: 'device_token_file', path: ['upstream', 'device_token_file'], kind: 'text' },
  { name: 'passthrough_ip', path: ['upstream', 'passthrough_ip'], kind: 'checkbox' },
  { name: 'rotate_max', path: ['upstream', 'rotate_max'], kind: 'number' },
  { name: 'rotate_budget', path: ['upstream', 'rotate_budget'], kind: 'text' },
  { name: 'rotate_status_codes', path: ['upstream', 'rotate_status_codes'], kind: 'hours' },
  { name: 'proxies', path: ['upstream', 'proxies'], kind: 'list' },
  { name: 'global_enabled', path: ['global', 'enabled'], kind: 'checkbox' },
  { name: 'global_chat_base', path: ['global', 'chat_base'], kind: 'text' },
  { name: 'global_billing_base', path: ['global', 'billing_base'], kind: 'text' },
  { name: 'cn_chat_base', path: ['cn', 'chat_base'], kind: 'text' },
  { name: 'upstash_url', path: ['upstash', 'url'], kind: 'text' },
  { name: 'upstash_token', path: ['upstash', 'token'], kind: 'password', secret: true },
  { name: 'prompt_mode', path: ['prompt', 'mode'], kind: 'select' },
  { name: 'prompt_file', path: ['prompt', 'file'], kind: 'text' },
  { name: 'sanitize_blacklist_fingerprints', path: ['features', 'sanitize_blacklist_fingerprints'], kind: 'checkbox' },
  { name: 'multimodal_routing', path: ['features', 'multimodal_routing'], kind: 'checkbox' },
  { name: 'local_web_tools', path: ['features', 'local_web_tools'], kind: 'checkbox' },
  { name: 'local_web_tool_rounds', path: ['features', 'local_web_tool_rounds'], kind: 'number' },
  { name: 'search_endpoint', path: ['features', 'search_endpoint'], kind: 'checkbox' },
  { name: 'session_sticky_enabled', path: ['session_sticky', 'enabled'], kind: 'checkbox' },
  { name: 'request_client_info', path: ['logging', 'request_client_info'], kind: 'checkbox' },
];

export const CFG_FIELD_BY_NAME: Record<string, FieldDef> = Object.fromEntries(
  CFG_FIELDS.map((f) => [f.name, f]),
);

// 覆盖型文本字段：空串本身有意义（= 回落内置默认），必须照发。不含 api_key。
export const CLEARABLE_CFG = new Set(['user_agent', 'prompt_file']);

export const DURATION_RE = /^(\d+(\.\d+)?(ns|us|µs|ms|s|m|h))+$/;
export const DURATION_FIELDS = new Set([
  'soft_rate',
  'soft_rate_max',
  'breaker_cooldown',
  'breaker_cooldown_max',
  'degrade_cooldown',
  'degrade_cooldown_max',
  'cost_explore_interval',
  'expiring_soon',
  'ttl',
  'checkin_account_delay',
  'rotate_budget',
]);
export const DURATION_TIP = '格式应为 Go 时长：30m / 2h / 600s / 1h30m';

export type FieldValue = string | boolean;

export function dig(obj: unknown, path: string[]): unknown {
  return path.reduce<unknown>(
    (o, k) => (o == null ? undefined : (o as Record<string, unknown>)[k]),
    obj,
  );
}

export function put(obj: Record<string, unknown>, path: string[], val: unknown): void {
  let o = obj;
  for (let i = 0; i < path.length - 1; i++) {
    const key = path[i]!;
    if (typeof o[key] !== 'object' || o[key] === null) o[key] = {};
    o = o[key] as Record<string, unknown>;
  }
  o[path[path.length - 1]!] = val;
}

export function durationBad(value: string): boolean {
  const v = value.trim();
  return v !== '' && !DURATION_RE.test(v);
}

// configToValues 把服务端配置映射为表单初值。secret 字段已有值时留空，
// 让「空 = 不修改」与 collectConfig 的跳过语义配合，避免敏感值进入 DOM。
export function configToValues(cfg: unknown): Record<string, FieldValue> {
  const next: Record<string, FieldValue> = {};
  for (const f of CFG_FIELDS) {
    const v = dig(cfg, f.path);
    if (f.kind === 'checkbox') next[f.name] = !!v;
    else if (f.secret && v != null && v !== '') next[f.name] = '';
    else if (Array.isArray(v)) next[f.name] = v.join(', ');
    else next[f.name] = v == null ? '' : String(v);
  }
  return next;
}

// collectConfig 从表单值组装提交对象：空 = 不下发（覆盖型字段除外）。
export function collectConfig(values: Record<string, FieldValue>): Record<string, unknown> {
  const out: Record<string, unknown> = {};
  for (const field of CFG_FIELDS) {
    if (!(field.name in values)) continue;
    const raw = values[field.name];
    let v: unknown;
    if (field.kind === 'checkbox') {
      v = !!raw;
    } else {
      const trimmed = String(raw).trim();
      if (trimmed === '') {
        if (field.kind === 'list' || field.kind === 'hours') v = [];
        else v = CLEARABLE_CFG.has(field.name) ? '' : undefined;
      } else if (field.kind === 'hours') {
        v = trimmed.split(/[,，\s]+/).filter(Boolean).map(Number);
      } else if (field.kind === 'list') {
        v = trimmed.split(/[,，\s]+/).filter(Boolean);
      } else if (field.kind === 'number') {
        v = Number(trimmed);
      } else {
        v = trimmed;
      }
    }
    if (v !== undefined) put(out, field.path, v);
  }
  return out;
}
