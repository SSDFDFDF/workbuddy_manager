/* 配置字段目录：表单名 → config.json 路径。
   「需重启」徽标由后端 catalog 驱动（唯一真相），前端不硬编码。 */

export const CFG_MAP: Record<string, string[]> = {
  listen: ['listen'],
  api_key: ['api_key'],
  package_detail_limit: ['panel', 'package_detail_limit'],
  checkin_hours: ['schedule', 'checkin_hours'],
  checkin_enabled: ['schedule', 'checkin_enabled'],
  growth_hours: ['schedule', 'growth_hours'],
  growth_enabled: ['schedule', 'growth_enabled'],
  growth_autotasks_disabled: ['growth', 'autotasks', 'disabled'],
  growth_autotasks_only: ['growth', 'autotasks', 'only'],
  growth_autotasks_order: ['growth', 'autotasks', 'order'],
  growth_autotasks_mp_codes: ['growth', 'autotasks', 'mp_codes'],
  growth_autotasks_allow_unknown_claim: ['growth', 'autotasks', 'allow_unknown_claim'],
  travel_hours: ['schedule', 'travel_hours'],
  travel_enabled: ['schedule', 'travel_enabled'],
  activity_hours: ['schedule', 'activity_hours'],
  activity_enabled: ['schedule', 'activity_enabled'],
  keepalive_hours: ['schedule', 'keepalive_hours'],
  keepalive_enabled: ['schedule', 'keepalive_enabled'],
  balance_refresh_enabled: ['schedule', 'balance_refresh_enabled'],
  balance_refresh_minutes: ['schedule', 'balance_refresh_minutes'],
  include_disabled_in_tasks: ['schedule', 'include_disabled_in_tasks'],
  max_in_flight: ['pool', 'max_in_flight'],
  max_in_flight_global: ['pool', 'max_in_flight_global'],
  breaker_threshold: ['pool', 'breaker_threshold'],
  degrade_threshold: ['pool', 'degrade_threshold'],
  degrade_cooldown: ['pool', 'degrade_cooldown'],
  degrade_cooldown_max: ['pool', 'degrade_cooldown_max'],
  cost_explore_interval: ['pool', 'cost_explore_interval'],
  credit_floor: ['pool', 'credit_floor'],
  prefer_expiring: ['pool', 'prefer_expiring'],
  expiring_soon: ['pool', 'expiring_soon'],
  soft_rate: ['cooldown', 'soft_rate'],
  soft_rate_max: ['cooldown', 'soft_rate_max'],
  breaker_cooldown: ['pool', 'breaker_cooldown'],
  breaker_cooldown_max: ['pool', 'breaker_cooldown_max'],
  idle_weight_per_hour: ['pool', 'idle_weight_per_hour'],
  idle_weight_max: ['pool', 'idle_weight_max'],
  ttl: ['session_sticky', 'ttl'],
  timeout_seconds: ['upstream', 'timeout_seconds'],
  header_timeout_seconds: ['upstream', 'header_timeout_seconds'],
  idle_timeout_seconds: ['upstream', 'idle_timeout_seconds'],
  cn_client_version: ['upstream', 'profiles', 'cn', 'client_version'],
  cn_cli_version: ['upstream', 'profiles', 'cn', 'cli_version'],
  global_client_version: ['upstream', 'profiles', 'global', 'client_version'],
  global_cli_version: ['upstream', 'profiles', 'global', 'cli_version'],
  prompt_mode: ['prompt', 'mode'],
  prompt_preset: ['prompt', 'preset'],
  prompt_file: ['prompt', 'file'],
  prompt_text: ['prompt', 'text'],
  prompt_cn_mode: ['prompt', 'profiles', 'cn', 'mode'],
  prompt_cn_preset: ['prompt', 'profiles', 'cn', 'preset'],
  prompt_cn_text: ['prompt', 'profiles', 'cn', 'text'],
  prompt_global_mode: ['prompt', 'profiles', 'global', 'mode'],
  prompt_global_preset: ['prompt', 'profiles', 'global', 'preset'],
  prompt_global_text: ['prompt', 'profiles', 'global', 'text'],
  fingerprint_rewrite: ['fingerprint_rewrite'],
  media_tool_images: ['media', 'tool_images'],
  media_image_transcode: ['media', 'image_transcode'],
  media_image_max_dimension: ['media', 'image_max_dimension'],
  fingerprint_rules_text: ['fingerprint_rules'],
  session_sticky_enabled: ['session_sticky', 'enabled'],
  model_default_realm: ['model_default_realm'],
  proxy_url: ['proxy_url'],
  resin_url: ['resin_url'],
  resin_platform_name: ['resin_platform_name'],
  resin_mode: ['resin_mode'],
  resin_auth_version: ['resin_auth_version'],
  request_client_info: ['logging', 'request_client_info'],
  max_inflight_requests: ['server', 'max_inflight_requests'],
  max_inflight_bytes_mb: ['server', 'max_inflight_bytes_mb'],
  ingress_wait: ['server', 'ingress_wait'],
  read_timeout: ['server', 'read_timeout'],
};

/* 覆盖型字段：空串也有意义（= 回落内置默认），必须照发。 */
export const CLEARABLE_CFG = new Set([
  // 顶层提示词：选「default — 内置默认」= 空串，必需照发——提交里没出现的键会
  // 保持磁盘现值，不发就等于"没改"，面板保存后回显成旧值（看起来像"选了 default
  // 却跳回 craft"）。
  'prompt_preset',
  'prompt_file', 'prompt_text',
  // 分域提示词：空串 = 继承顶层（该域三项都空时，后端把这段覆盖整体剪掉）。
  'prompt_cn_text', 'prompt_global_text',
  'prompt_cn_mode', 'prompt_global_mode', 'prompt_cn_preset', 'prompt_global_preset',
  'proxy_url', 'resin_url', 'resin_platform_name',
  'cn_client_version', 'cn_cli_version', 'global_client_version', 'global_cli_version',
]);

/* 逗号分隔的字符串列表字段（Go 侧 []string）。空串有意：清空表单 = 下发 []（清掉
   清单），因为数组在保存时是整体替换；不提交的话磁盘上的旧值永远不会消失。 */
export const STR_LIST_CFG = new Set([
  'growth_autotasks_disabled', 'growth_autotasks_only', 'growth_autotasks_order', 'growth_autotasks_mp_codes',
]);

/* Go 侧是数字的字段（int / int64 / float64）：DOM 的 input.value 永远是字符串，
   不一转 Number 就下发，Go 的 json.Unmarshal 会报「cannot unmarshal string into
   Go struct field .xxx of type int」。而面板保存是**整张表单一起提交**，任一项
   类型错就全盘 400（用户只改了一个开关也会看到某条无关字段的报错）。
   守护：internal/panel/config_schema_test.go 按 Go 侧真实类型核对本集合。 */
export const NUMERIC_CFG = new Set([
  // 数字输入框
  'package_detail_limit',
  'balance_refresh_minutes',
  'max_in_flight',
  'max_in_flight_global',
  'breaker_threshold',
  'degrade_threshold',
  'credit_floor',
  'idle_weight_per_hour',
  'idle_weight_max',
  'timeout_seconds',
  'header_timeout_seconds',
  'idle_timeout_seconds',
  'max_inflight_requests',
  'max_inflight_bytes_mb',
  // 值是数字的下拉框
  'media_image_max_dimension',
]);

/* 手工处理字段（结构化，不走通用回填/收集；只对 CFG_MAP 里的条目生效）。 */
export const MANUAL_CFG = new Set(['fingerprint_rules_text']);

/* Go 时长字段：非空必须 ParseDuration 语法（裸 0 合法）。 */
export const DURATION_FIELDS = [
  'soft_rate', 'soft_rate_max', 'breaker_cooldown', 'breaker_cooldown_max',
  'degrade_cooldown', 'degrade_cooldown_max', 'cost_explore_interval', 'expiring_soon', 'ttl',
  'read_timeout', 'ingress_wait',
];
export const DURATION_RE = /^0$|^(\d+(\.\d+)?(ns|us|µs|ms|s|m|h))+$/;
export const DURATION_TIP = '格式应为 Go 时长：30m / 2h / 600s / 1h30m（0 = 不限制）';

export function dig(obj: any, path: string[]): any {
  return path.reduce((o, k) => (o == null ? undefined : o[k]), obj);
}

export function put(obj: any, path: string[], val: unknown) {
  let o = obj;
  for (let i = 0; i < path.length - 1; i++) {
    if (typeof o[path[i]] !== 'object' || o[path[i]] === null) o[path[i]] = {};
    o = o[path[i]];
  }
  o[path[path.length - 1]] = val;
}

/* 指纹规则文本编解码（一行一条）。 */
export interface FpRule {
  match: string;
  replace?: string;
  mode: string;
  action: string;
}

export function parseRules(text: string): FpRule[] {
  const out: FpRule[] = [];
  for (const raw of String(text ?? '').split(/\r?\n/)) {
    let line = raw.trim();
    if (!line || line.startsWith('#')) continue;
    let mode = 'literal';
    let action = 'replace';
    while (line[0] === '/' || line[0] === '!') {
      if (line[0] === '/') mode = 'fold';
      else action = 'remove';
      line = line.slice(1).trim();
    }
    let match = line;
    let replace = '';
    const at = line.indexOf('=>');
    if (at >= 0) {
      match = line.slice(0, at).trim();
      replace = line.slice(at + 2).trim();
    }
    if (!match) continue;
    out.push(action === 'remove' ? { match, mode, action } : { match, replace, mode, action });
  }
  return out;
}

export function formatRules(rules: FpRule[] | undefined): string {
  return (rules || [])
    .map((r) => {
      const p = (r.mode === 'fold' ? '/' : '') + (r.action === 'remove' ? '!' : '');
      return r.action === 'remove' ? p + r.match : p + r.match + ' => ' + (r.replace ?? '');
    })
    .join('\n');
}

/* 表单收集：表单值 → 提交对象。

   空串的两种语义（这是本文件最容易改错的地方）：
   - 默认不下发（undefined）：留空 = 不提交该键，磁盘现值不变；
   - CLEARABLE_CFG / STR_LIST_CFG 里的字段照发空值：留空本身就是一个选择
     （回落内置默认 / 清空清单），必须让后端看到。
   后端保存是「全量覆盖当前 schema」：提交里出现的键才改，其余保持磁盘值
   （部分提交不会把别的键重置），写出去的文件总是完整当前结构。
   分域提示词三项全空 = 继承顶层：照发空值即可，后端归一化时会把这段空壳剪掉，
   前端不需要（也不该）自己判断「磁盘上原本有没有覆盖」。 */
export function collectConfig(
  form: Record<string, { type?: string; value?: string; checked?: boolean }>,
): Record<string, any> {
  const out: Record<string, any> = {};
  for (const [name, path] of Object.entries(CFG_MAP)) {
    if (MANUAL_CFG.has(name)) continue;
    const el = form[name];
    if (!el) continue;
    let v: unknown;
    if (el.type === 'checkbox') v = !!el.checked;
    else {
      const raw = String(el.value ?? '').trim();
      if (raw === '') v = STR_LIST_CFG.has(name) ? [] : (CLEARABLE_CFG.has(name) ? '' : undefined);
      else if (NUMERIC_CFG.has(name)) v = Number(raw);
      else if (name.endsWith('_hours')) v = raw.split(/[,，\s]+/).filter(Boolean).map(Number);
      else if (STR_LIST_CFG.has(name)) v = raw.split(/[,，\s]+/).filter(Boolean);
      else v = raw;
    }
    if (v !== undefined) put(out, path, v);
  }
  const rulesEl = form['fingerprint_rules_text'];
  if (rulesEl) out.fingerprint_rules = parseRules(String(rulesEl.value ?? ''));

  const disEl = form['fingerprint_disabled_json'];
  if (disEl && disEl.value) {
    try {
      const d = JSON.parse(String(disEl.value));
      if (Array.isArray(d)) out.fingerprint_disabled = d;
    } catch {}
  }

  const ovEl = form['fingerprint_overrides_json'];
  if (ovEl && ovEl.value) {
    try {
      const o = JSON.parse(String(ovEl.value));
      if (typeof o === 'object' && o) out.fingerprint_overrides = o;
    } catch {}
  }
  return out;
}
