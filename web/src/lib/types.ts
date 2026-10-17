// 面板 API 数据结构。

export interface RateLimitRow {
  model?: string;
  kind?: string;
  reset_at?: string;
  until?: string;
}

export interface ModelCost {
  model?: string;
  cost_per_1k?: number;
}

export interface AccountTokenUsage {
  request_count?: number;
  total_tokens?: number;
  last_latency_ms?: number;
  last_tokens_per_second?: number;
}

export interface Account {
  uid: string;
  nickname?: string;
  realm?: string;
  enterprise?: boolean;
  disabled?: boolean;
  paused?: boolean;
  checkin_done?: boolean;
  breaker_until?: string;
  degrade_until?: string;
  cool_remaining_sec?: number;
  cool_kind?: string;
  reason?: string;
  credits?: number;
  credits_total?: number;
  success_count?: number;
  err_total?: number;
  in_flight?: number;
  last_success?: string;
  rate_limited_models?: RateLimitRow[];
  model_costs?: ModelCost[];
  token_usage?: AccountTokenUsage;
  credits_earliest_expiry?: string;
  credits_earliest_remaining?: number;
}

// 批量任务运行状态（GET /panel/api/tasks/runs，issue #112）。
export interface TaskRun {
  task: string;
  trigger?: string;
  running?: boolean;
  started_at?: string;
  finished_at?: string;
  total?: number;
  success?: number;
  failure?: number;
}

export interface ModelLock {
  model?: string;
  realm?: string;
  state?: string;
  servable?: number;
  total?: number;
  locked?: number;
  unlock_at?: string;
  fully_unlock_at?: string;
  reason?: string;
}

export interface ApiKeyLimits {
  rpm?: number;
  tpm?: number;
  max_concurrent?: number;
  models?: string[];
}

export interface ApiKey {
  id: string;
  name: string;
  prefix: string;
  created_at: string;
  last_used_at?: string;
  revoked?: boolean;
  requests?: number;
  prompt_tokens?: number;
  completion_tokens?: number;
  total_tokens?: number;
  rejected?: number;
  limits?: ApiKeyLimits;
}

export interface Overview {
  total: number;
  healthy: number;
  cooling: number;
  disabled: number;
  paused?: number;
  sticky_sessions: number;
  version: string;
  redis_mode?: string;
  in_flight_full?: number;
  uptime_sec: number;
  accounts?: Account[];
  model_locks?: ModelLock[];
}
