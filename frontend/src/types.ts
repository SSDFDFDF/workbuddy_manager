/* 类型层：面板用到的后端 JSON 结构（/panel/api/*）。
   字段一律可选宽松（`?`），前端全部经 Number()/String() 收口——
   后端加字段不受影响，后端少字段不会让渲染抛错。 */

export interface OverviewAccount {
  uid: string;
  nickname?: string;
  realm?: string;
  enterprise?: boolean;
  disabled?: boolean;
  paused?: boolean;
  checkin_done?: boolean;
  proxy_enabled?: boolean;
  credits?: number;
  credits_total?: number;
  cooling?: boolean;
  cool_remaining_sec?: number;
  cool_kind?: string;
  breaker_until?: string;
  degrade_until?: string;
  reason?: string;
  success_count?: number;
  err_total?: number;
  in_flight?: number;
  last_success?: string;
  rate_limited_models?: RateLimitRow[];
  model_costs?: { model: string; cost_per_1k: number }[];
  token_usage?: {
    request_count?: number;
    total_tokens?: number;
    last_latency_ms?: number;
    last_tokens_per_second?: number;
  };
}

export interface RateLimitRow {
  model?: string;
  kind?: string;
  reset_at?: string;
  until?: string;
  reason?: string;
}

export interface ModelLockRow {
  model: string;
  realm?: string;
  state?: string;
  servable?: number;
  total?: number;
  locked?: number;
  unlock_at?: string;
  fully_unlock_at?: string;
  reason?: string;
}

export interface Overview {
  version?: string;
  uptime_sec?: number;
  auth_required?: boolean;
  redis_mode?: string;
  sticky_sessions?: number;
  total?: number;
  healthy?: number;
  cooling?: number;
  disabled?: number;
  paused?: number;
  in_flight_full?: number;
  proxy_configured?: boolean;
  proxy_mode?: string;
  accounts?: OverviewAccount[];
  model_locks?: ModelLockRow[] | null;
}

export interface LogEntry {
  ts?: string;
  ch?: string;
  text?: string;
}

export interface RequestEvent {
  time?: string;
  status?: number | string;
  outcome?: string;
  /** path：请求进来的端点（/v1/chat/completions、/v1/responses、/v1/messages）。
      归档开启后才有；旧归档记录（该字段上线前写入）缺失，前端按「—」显示而不是猜测默认端点。 */
  path?: string;
  model?: string;
  account?: string;
  client_ip?: string;
  user_agent?: string;
  duration_ms?: number;
  ttfb_ms?: number;
  attempts?: number;
  prompt_tokens?: number;
  completion_tokens?: number;
  total_tokens?: number;
  credit?: number;
  credit_known?: boolean;
  request_id?: string;
  prompt_preset?: string;
  prompt_mode?: string;
  prompt_chars?: number;
  prompt_sha256?: string;
  cache_hit_tokens?: number;
  cache_miss_tokens?: number;
  // dropped：跨协议入口（Responses / Messages）接受但无法表达、因此被忽略的客户端字段。
  dropped?: string[];
  // Messages history translation counters (no reasoning text/signatures).
  thinking_replay_enabled?: boolean;
  thinking_plain_blocks?: number;
  thinking_replayed_blocks?: number;
  thinking_replayed_bytes?: number;
  thinking_redacted_blocks?: number;
  // rewritten：网关主动改写的生成参数（key:原值→出站值），如越界 reasoning effort 归一。
  // 与 dropped 分开：改写是「换了值执行了」，且不会被 dropped 的数量上限挤掉。
  rewritten?: string[];
  // upstream_kind / upstream_code：本次失败的上游分类与业务码（如 model_queued / 6020）。
  // 只记码不记正文；用于回答「上游到底回了什么码」（队列 6020/6021/6022、11102、6004 …）。
  upstream_kind?: string;
  upstream_code?: string;
  // conversation_derived：本次出站的 X-Conversation-ID 是网关派生（客户端未给会话 id、
  // 且 upstream.derive_conversation_id 开启）。A/B 观测会话身份策略用。
  conversation_derived?: boolean;
}

export interface RequestMetrics {
  completed?: number;
  success_rate?: number | null;
  http_success_rate?: number | null;
  avg_duration_ms?: number;
  in_flight?: number;
  archive?: {
    enabled?: boolean;
    bytes?: number;
    dropped_writes?: number;
    last_error?: string;
  };
  recent?: RequestEvent[];
}

export interface ModelEntry {
  id: string;
  name?: string;
  default_effort?: string;
  supported_efforts?: string[];
  can_disable_thinking?: boolean;
  supports_reasoning?: boolean;
  supports_images?: boolean;
  supports_tool_call?: boolean;
  credits?: string | number;
  description?: string;
  vendor?: string;
  is_default?: boolean;
  context_length?: number;
  max_output_tokens?: number;
  promo_factor?: number;
  promo_credits?: string | number;
  promo_label?: string;
  promo_note?: string;
  tags?: string[];
}

export interface ProbeEntry {
  claimed?: number;
  measured?: number;
  verdict?: string;
  note?: string;
  tested_at?: string;
}

export interface CreditPackage {
  name?: string;
  package_code?: string;
  size?: number;
  remain?: number;
  used?: number;
  created_at?: string;
  end_time?: string;
  expires_at?: number;
}

export interface PackageAccount {
  uid: string;
  nickname?: string;
  realm?: string;
  remain?: number;
  size?: number;
  packages?: CreditPackage[];
  error?: string;
}

export interface Voucher {
  prize_name?: string;
  sku_code?: string;
  code?: string;
  valid_to?: string;
  granted_at?: string;
}

export interface VoucherAccount {
  uid: string;
  nickname?: string;
  vouchers?: Voucher[];
  error?: string;
}

export interface GrowthTask {
  task_code: string;
  title?: string;
  current?: number;
  target?: number;
  credit?: number;
  energy?: number;
  reward_buddy?: boolean;
  claimed?: boolean;
  claimable?: boolean;
  locked?: boolean;
  accept_status?: string;
  tag?: string;
  description?: string;
  task_desc?: string;
  jump_url?: string;
}

export interface QueueItem {
  uid: string;
  nickname?: string;
  kind?: string;
  code?: string;
  status?: string;
  message?: string;
}

export interface QueueStatus {
  started?: boolean;
  running?: boolean;
  seq?: number;
  items?: QueueItem[];
}

export interface TaskActionItem {
  code: string;
  title?: string;
  desc?: string;
  attempt?: boolean;
  enabled: boolean;
  mp?: boolean;
  builtin?: boolean;
  action_type?: string;
  model?: string;
  gap?: string;
  target?: number;
  window?: string;
  activity_id?: string;
  unconfigured?: boolean;
}

export interface TaskPolicyUpdate {
  code: string;
  enabled?: boolean;
  mp?: boolean;
  action_type?: string;
  model?: string;
  gap?: string;
  target?: number;
  window?: string;
  activity_id?: string;
}

export interface ScanAccount {
  uid: string;
  nickname?: string;
  growth?: { task_code: string; title?: string; current?: number; target?: number }[];
}

/* 用量（usage.Recorder.SnapshotWindow 输出，仅列前端消费的字段）。 */
export interface UsageRow {
  key?: string;
  nickname?: string;
  realm?: string;
  extra?: string;
  requests?: number;
  errors?: number;
  prompt_tokens?: number;
  completion_tokens?: number;
  total_tokens?: number;
  avg_latency_ms?: number;
  avg_tokens_per_second?: number;
  credits?: number;
  credit_tokens?: number;
  credit_samples?: number;
  credits_per_1m_tokens?: number;
  cache_hit_tokens?: number;
  cache_miss_tokens?: number;
  rate?: string;
}

export interface UsageSnapshot {
  totals?: UsageRow;
  by_account?: UsageRow[];
  by_model?: UsageRow[];
  by_realm?: UsageRow[];
  credit_by_account?: UsageRow[];
  credit_by_model?: UsageRow[];
  series?: { t: string; scope?: string; prompt_tokens?: number; completion_tokens?: number; total_tokens?: number; requests?: number }[];
  window_from?: string;
  window_to?: string;
  buckets?: number;
  since?: string;
  file_bytes?: number;
}

/* 配置目录（config/catalog）。 */
export interface CatalogField {
  path: string;
  mode?: string;
  why?: string;
}

export interface PromptPresetInfo {
  name: string;
  label?: string;
  description?: string;
  chars_cn?: number;
  chars_global?: number;
  realms?: boolean;
}

export interface PromptPreview {
  mode?: string;
  source?: string;
  preset?: string;
  inherited?: boolean;
  text?: string;
  error?: string;
  chars?: number;
  bytes?: number;
  truncated?: boolean;
}

export interface PromptPreviewResponse {
  presets?: PromptPresetInfo[];
  note?: string;
  default?: PromptPreview;
  cn?: PromptPreview;
  global?: PromptPreview;
  warnings?: string[];
}

export interface VersionInfo {
  checked?: boolean;
  latest_cn?: string;
  latest_global?: string;
  builtin_cn_client?: string;
  builtin_cn_cli?: string;
  builtin_global_client?: string;
  builtin_global_cli?: string;
}

/* 内置指纹表（/panel/api/scrub/builtin 下发）。 */
export interface BuiltinScrubRule {
  id: string;
  kind: 'literal' | 'pattern';
  match: string;
  replace?: string;
  note?: string;
}

export interface BuiltinScrubResponse {
  rules?: BuiltinScrubRule[];
}
