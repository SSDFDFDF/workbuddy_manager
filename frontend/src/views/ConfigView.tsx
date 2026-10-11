/* 配置视图：7 个分区（侧栏 Tab）+ 回填/收集/校验 + 提示词预览 + 指纹规则。 */
import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { api, setApiKey } from '../api';
import { toast } from '../toast';
import { useData } from '../data';
import type { CatalogField, PromptPresetInfo, PromptPreviewResponse, VersionInfo } from '../types';
import {
  CFG_MAP, DURATION_FIELDS, DURATION_RE, DURATION_TIP,
  dig, collectConfig, formatRules,
} from '../configSchema';
import { Field, Switch, type FieldSpec } from './configWidgets';
import { btnPrimary, btnGhost } from '../components/buttons';

const SECTIONS = [
  { id: 'sec-svc', name: '服务与网络', sub: '端口 · 鉴权 · 超时 · 代理' },
  { id: 'sec-tasks', name: '定时任务', sub: '签到 · 保活 · 旅行 · 余额' },
  { id: 'sec-pool', name: '账号池与治理', sub: '并发 · 熔断 · 降权 · 权重' },
  { id: 'sec-upstream', name: '客户端仿真', sub: 'UA · CLI版本 · 域名映射' },
  { id: 'sec-prompt', name: '网关提示词', sub: '组合模式 · 预设 · 覆盖' },
  { id: 'sec-media', name: '多模态与图片', sub: '工具图片 · 转码 · 压缩' },
  { id: 'sec-fp-log', name: '指纹与日志', sub: '指纹脱敏 · 规则 · 来源记录' },
] as const;

const Grid = ({ cols, children }: { cols: 2 | 3; children: React.ReactNode }) => (
  <div className={'grid gap-x-4 gap-y-3 ' + (cols === 2 ? 'grid-cols-2' : 'grid-cols-3')}>{children}</div>
);

function SubHead({ children }: { children: React.ReactNode }) {
  return <div className="mt-1 mb-0.5 text-[12.5px] font-semibold text-[var(--ink-2)]">{children}</div>;
}

export function ConfigView() {
  const { refresh } = useData();
  const [values, setValues] = useState<Record<string, string | boolean>>({});
  const [path, setPath] = useState('');
  const [note, setNote] = useState('');
  const [sec, setSec] = useState<string>('sec-svc');
  const [catalog, setCatalog] = useState<CatalogField[] | null>(null);
  const [presets, setPresets] = useState<PromptPresetInfo[]>([]);
  const [versionInfo, setVersionInfo] = useState<VersionInfo | null>(null);
  const [preview, setPreview] = useState<PromptPreviewResponse | null>(null);
  const [previewing, setPreviewing] = useState(false);
  const [saving, setSaving] = useState(false);
  /* 内置自动化动作清单（后端注册表下发，前端不硬编码任务码）。 */
  const [taskActions, setTaskActions] = useState<{ code: string; desc?: string; enabled: boolean; mp?: boolean }[]>([]);
  /* 逐任务参数覆盖的只读预览（手工编辑 config.json；见表单提示）。 */
  const [growthTasks, setGrowthTasks] = useState('');
  const formRef = useRef<HTMLFormElement>(null);

  /* catalog 匹配（与 Go 侧 config.MatchPath 同语义）。 */
  const matchPath = (pattern: string, p: string) => (pattern.endsWith('.*') ? p.startsWith(pattern.slice(0, -2) + '.') : pattern === p);
  const needsRestart = (name: string): string | null => {
    if (!catalog) return null;
    const p = CFG_MAP[name]?.join('.') || '';
    for (const f of catalog) if (matchPath(f.path, p)) return f.mode === 'restart' ? f.why || '改动需重启进程生效' : null;
    return null;
  };

  const load = useCallback(async () => {
    try {
      const d = await api<{ config: Record<string, any>; path?: string; version_info?: VersionInfo }>('config');
      const cfg = d.config || {};
      const v: Record<string, string | boolean> = {};
      for (const [name, p] of Object.entries(CFG_MAP)) {
        const val = dig(cfg, p);
        if (typeof val === 'boolean') v[name] = val;
        else if (Array.isArray(val)) v[name] = val.join(', ');
        else v[name] = val == null ? '' : String(val);
      }
      v['fingerprint_rules_text'] = formatRules(cfg.fingerprint_rules);
      setValues(v);
      const gt = cfg.growth?.autotasks?.tasks;
      setGrowthTasks(gt && Object.keys(gt).length ? JSON.stringify(gt, null, 2) : '');
      setPath(d.path || '');
      setVersionInfo(d.version_info || null);
      const bits: string[] = [];
      const warns = Array.isArray(cfg._warnings) ? cfg._warnings : [];
      if (warns.length) bits.push(`配置告警：${warns.join('；')}`);
      setNote(bits.join(' ｜ '));
    } catch (e) {
      toast('读取配置失败：' + (e as Error).message, 'err');
    }
    try {
      const c = await api<{ fields?: CatalogField[] }>('config/catalog');
      setCatalog(c.fields || []);
    } catch {
      setCatalog([]); // 失败保持沉默：徽标是提示不是功能
    }
    try {
      const p = await api<PromptPreviewResponse>('prompt/preview', { method: 'POST', body: JSON.stringify({ prompt: {} }) });
      setPresets(p.presets || []);
    } catch {
      setPresets([]);
    }
    try {
      const a = await api<{ actions?: { code: string; desc?: string; enabled: boolean; mp?: boolean }[] }>('tasks/actions');
      setTaskActions(a.actions || []);
    } catch {
      setTaskActions([]); // 失败沉默：提示性信息，不是功能
    }
  }, []);

  useEffect(() => {
    void load();
  }, [load]);

  const set = (name: string) => (val: string | boolean) => setValues((s) => ({ ...s, [name]: val }));

  /* 表单元素形态（collectConfig 需要）。 */
  const formData = useMemo(() => {
    const out: Record<string, { type?: string; value?: string; checked?: boolean }> = {};
    for (const [k, v] of Object.entries(values)) {
      if (typeof v === 'boolean') out[k] = { type: 'checkbox', checked: v };
      else out[k] = { value: v };
    }
    return out;
  }, [values]);

  const promptDraft = () => ({ prompt: collectConfig(formData).prompt || {} });

  const doPreview = async () => {
    setPreviewing(true);
    try {
      const d = await api<PromptPreviewResponse>('prompt/preview', { method: 'POST', body: JSON.stringify(promptDraft()) });
      setPreview(d);
    } catch (e) {
      toast('预览失败：' + (e as Error).message, 'err');
    } finally {
      setPreviewing(false);
    }
  };

  const save = async (ev: React.FormEvent) => {
    ev.preventDefault();
    /* 时长字段校验。 */
    for (const name of DURATION_FIELDS) {
      const v = String(values[name] ?? '').trim();
      if (v && !DURATION_RE.test(v)) {
        toast(DURATION_TIP, 'err');
        return;
      }
    }
    setSaving(true);
    try {
      const r = await api<{ restart_required?: string[] }>('config', { method: 'POST', body: JSON.stringify(collectConfig(formData)) });
      const n = (r.restart_required || []).length;
      toast(n ? `配置已保存，其中 ${n} 项需重启进程生效` : '配置已保存并立即生效', 'ok');
      const k = String(values['api_key'] ?? '').trim();
      if (k) setApiKey(k); // 密钥改了：本会话沿用新值
      void load();
      void refresh(true);
    } catch (e) {
      toast('保存失败：' + (e as Error).message, 'err');
    } finally {
      setSaving(false);
    }
  };

  const fld = (spec: FieldSpec): React.ReactNode => (
    <Field key={spec.name} spec={{ ...spec, badge: undefined }} value={values[spec.name] ?? ''} onChange={set(spec.name)} bad={DURATION_FIELDS.includes(spec.name) && !!String(values[spec.name] ?? '').trim() && !DURATION_RE.test(String(values[spec.name]))} />
  );

  const restartTag = (name: string) => (needsRestart(name) ? <span className="rounded bg-[var(--warn-soft)] px-1 py-px text-[10.5px] font-medium text-[var(--warn)]" title={needsRestart(name)!}>需重启</span> : null);

  const sw = (name: string, label: string) => (
    <Switch label={<>{label} {restartTag(name)}</>} name={name} checked={!!values[name]} onChange={set(name)} />
  );

  const versionNotes = (realm: 'cn' | 'global'): string[] => {
    const vi = versionInfo;
    if (!vi) return [];
    const cn = realm === 'cn';
    const L = cn ? 'CN' : 'Global';
    const client = String(values[realm + '_client_version'] ?? '').trim() || (cn ? vi.builtin_cn_client : vi.builtin_global_client) || '';
    const cli = String(values[realm + '_cli_version'] ?? '').trim() || (cn ? vi.builtin_cn_cli : vi.builtin_global_cli) || '';
    const latest = (cn ? vi.latest_cn : vi.latest_global) || '';
    const bClient = (cn ? vi.builtin_cn_client : vi.builtin_global_client) || '';
    const bCli = (cn ? vi.builtin_cn_cli : vi.builtin_global_cli) || '';
    const notes: string[] = [];
    if (latest && client && client !== latest) notes.push(`${L} 客户端版本 ${client} 落后于已拉取的最新 ${latest}（可一键填入）`);
    if (bClient && client !== bClient && cli === bCli) notes.push(`${L} 已改客户端版本但 CLI 版本仍是内置 ${bCli}，请填该构建捆绑的 CLI 号`);
    return notes;
  };

  const promptPresetHint = () => {
    const cur = presets.find((p) => p.name === (values['prompt_preset'] || 'default'));
    return cur ? `${cur.description}（CN ${cur.chars_cn} 字${cur.realms ? ' / Global ' + cur.chars_global + ' 字' : ''}）` : '预设列表由后端提供';
  };

  const presetOptions = (withInherit: boolean): [string, string][] => [
    ...(withInherit ? [['', '继承顶层']] : [['', 'default — 内置默认']]) as [string, string][],
    ...presets.map((p) => [p.name, `${p.name} — ${p.label || ''} ${p.chars_cn || 0}字${p.realms ? '（分域）' : ''}`] as [string, string]),
  ];

  const MODES: [string, string][] = [
    ['inject', 'inject — 客户端指令并入网关正文（默认）'],
    ['none', 'none — 不改写客户端指令（透传）'],
    ['replace', 'replace — 只留网关提示词（指纹面最小）'],
    ['after', 'after — 网关在前 + 客户端在后'],
    ['append', 'append — 客户端在前 + 网关在后'],
  ];
  const modeMap = (mode: string) => {
    const m = mode || 'none';
    if (m === 'replace') return { segs: [['gw', '网关提示词'], ['drop', '客户端 system（丢弃）'], ['usr', '对话']], cap: '客户端 system 被丢弃（指纹面最小）' };
    if (m === 'after') return { segs: [['gw', '网关提示词'], ['cl', '客户端 system'], ['usr', '对话']], cap: '网关在前，客户端内容紧随其后' };
    if (m === 'append') return { segs: [['cl', '客户端 system'], ['gw', '网关提示词'], ['usr', '对话']], cap: '客户端在前，网关提示词插在其后' };
    return { segs: [['cl', '客户端 system'], ['usr', '对话']], cap: '逐字透传：一个字节都不动' };
  };

  return (
    <form ref={formRef} onSubmit={save} className="grid grid-cols-[240px_1fr] gap-4">
      {/* 侧栏 */}
      <aside className="flex h-fit flex-col gap-3 self-start rounded-xl border border-[var(--line)] bg-[var(--surface)] p-3.5">
        {SECTIONS.map((s) => (
          <button
            key={s.id}
            type="button"
            onClick={() => setSec(s.id)}
            className={
              'flex flex-col rounded-lg px-3 py-2 text-left text-[13px] transition-colors ' +
              (sec === s.id ? 'bg-[var(--accent-soft)] text-[var(--accent)]' : 'text-[var(--ink-2)] hover:bg-[var(--surface-2)]')
            }
          >
            <span className="font-medium">{s.name}</span>
            <span className="text-[11px] opacity-70">{s.sub}</span>
          </button>
        ))}
        <div className="mt-1 border-t border-[var(--line-soft)] pt-3 text-[11px] text-[var(--ink-3)]">
          <div>配置文件路径</div>
          <div className="mt-0.5 break-all font-[family-name:var(--mono)]">{path || '-'}</div>
        </div>
      </aside>

      {/* 分区面板 */}
      <div className="flex flex-col gap-4">
        {sec === 'sec-svc' && (
          <div className="flex flex-col gap-4 rounded-xl border border-[var(--line)] bg-[var(--surface)] p-4">
            <h3 className="text-[14px] font-semibold">
              服务与网络 <span className="ml-1 text-[12px] font-normal text-[var(--ink-3)]">网关监听、API 鉴权、超时控制与代理接入</span>
            </h3>
            <SubHead>基础服务</SubHead>
            <Grid cols={2}>
              {fld({ name: 'listen', label: '监听地址', placeholder: ':7863', hint: '改动需重启进程生效' })}
              {fld({ name: 'api_key', label: 'API 密钥（即时生效）', type: 'password', placeholder: '留空 = 不鉴权', hint: '立即生效（含面板自身鉴权）', eye: true })}
            </Grid>
            {fld({ name: 'package_detail_limit', label: '单账号明细默认展示条数', type: 'number', min: 1, placeholder: '5', hint: '积分构成页按最早到期展示，其余未用完包聚合' })}
            <SubHead>入站准入（内存保护）</SubHead>
            <Grid cols={3}>
              {fld({ name: 'max_inflight_requests', label: '并发准入上限', type: 'number', min: 0, placeholder: '64', hint: '0 = 不限制' })}
              {fld({ name: 'max_inflight_bytes_mb', label: '并发字节预算', type: 'number', min: 0, placeholder: '256', hint: 'MiB；0 = 不限制' })}
              {fld({ name: 'ingress_wait', label: '满载等待', placeholder: '5s', hint: '满载时等待多久再回 503；0 = 立即拒绝' })}
            </Grid>
            {fld({ name: 'read_timeout', label: '入站读取超时', placeholder: '300s', hint: '含 body 上传的总时长上限；0 = 不限制' })}
            <SubHead>超时控制</SubHead>
            <Grid cols={3}>
              {fld({ name: 'timeout_seconds', label: '短请求超时', type: 'number', min: 1, placeholder: '120', hint: '非流式与元数据请求（秒）' })}
              {fld({ name: 'header_timeout_seconds', label: '聊天首字节超时', type: 'number', min: 1, placeholder: '120', hint: '等待上游响应头（秒）' })}
              {fld({ name: 'idle_timeout_seconds', label: '流空闲超时', type: 'number', min: 1, placeholder: '300', hint: '流式无数据包最大间隔（秒）' })}
            </Grid>
            <SubHead>代理接入</SubHead>
            <Grid cols={2}>
              {fld({ name: 'proxy_url', label: '普通代理 URL', placeholder: 'http://user:pass@127.0.0.1:8080 或 socks5://…', hint: 'http/https/socks5(s)；与 Resin 互斥' })}
            </Grid>
            <Grid cols={2}>
              {fld({ name: 'resin_url', label: 'Resin 代理 URL', placeholder: 'http://127.0.0.1:2260/my-token', hint: '含基址与 Token；清空即关闭' })}
              {fld({ name: 'resin_platform_name', label: 'Resin Platform', placeholder: '如 Default', hint: '不能含 . / :' })}
            </Grid>
            <Grid cols={2}>
              {fld({ name: 'resin_mode', label: 'Resin 接入模式', options: [['reverse', 'reverse — 反向代理（推荐）'], ['forward', 'forward — 正向代理']] })}
              {fld({ name: 'resin_auth_version', label: 'Resin 认证版本', options: [['V1', 'V1 — Platform.Account:Token（推荐）'], ['LEGACY_V0', 'LEGACY_V0 — Token:Platform:Account']] })}
            </Grid>
          </div>
        )}

        {sec === 'sec-tasks' && (
          <div className="flex flex-col gap-4 rounded-xl border border-[var(--line)] bg-[var(--surface)] p-4">
            <h3 className="text-[14px] font-semibold">
              定时任务 <span className="ml-1 text-[12px] font-normal text-[var(--ink-3)]">自动化保号、签到、活动推进与余额刷新</span>
            </h3>
            <div className="grid grid-cols-[repeat(auto-fill,minmax(280px,1fr))] gap-3">
              {(
                [
                  ['checkin_enabled', 'checkin_hours', '自动签到', '签到时点（小时，逗号分隔）', '9, 21', '每日指定整点自动签到获取积分'],
                  ['keepalive_enabled', 'keepalive_hours', 'Token 保活', '保活时点（小时）', '22', '刷新认证 Token 防过期掉线'],
                  ['travel_enabled', 'travel_hours', '猫猫旅行', '旅行时点（小时）', '9, 21', '一趟派出 + 一趟领奖闭环巡检'],
                  ['activity_enabled', 'activity_hours', '活跃上报', '上报时点（小时）', '10', '点亮连续登录并解锁领养前置'],
                  ['balance_refresh_enabled', 'balance_refresh_minutes', '后台刷新余额', '刷新间隔（分钟）', '5', '周期性拉取各账号最新余额'],
                  ['growth_enabled', 'growth_hours', '成长任务自动执行', '执行时点（小时）', '1', '每日到点自动扫描 + 执行全部待办（建议零点后）'],
                ] as const
              ).map(([en, hours, name, hLabel, ph, hint]) => (
                <div key={en} className="rounded-lg border border-[var(--line-soft)] bg-[var(--surface-2)] p-3.5">
                  {sw(en, name)}
                  <div className="mt-2.5">
                    <Field spec={{ name: hours, label: hLabel, placeholder: ph, hint }} value={values[hours] ?? ''} onChange={set(hours)} />
                  </div>
                </div>
              ))}
            </div>
            <div className="rounded-lg border border-[var(--line-soft)] bg-[var(--surface-2)] p-3.5">
              {sw('include_disabled_in_tasks', '保号任务覆盖已禁用账号')}
              <div className="mt-1.5 text-[11.5px] leading-relaxed text-[var(--ink-3)]">
                打开后已禁用的账号仍会签到 / 活跃上报 / 保活 / 刷新余额（依旧不参与选号）。适合用禁用做流量开关的轮换养号用法，默认关闭。若只想让单个账号临时退出选号但保留保号，用账号行的「暂停选号」。
              </div>
            </div>

            <div className="rounded-lg border border-[var(--line-soft)] bg-[var(--surface-2)] p-3.5">
              <div className="text-[12.5px] font-semibold text-[var(--ink-2)]">成长任务自动化（任务中心 / 一键完成）</div>
              <div className="mt-1.5 text-[11.5px] leading-relaxed text-[var(--ink-3)]">
                控制哪些成长/活动任务参与自动扫描与执行。判据事件形状是内置实现（代码），这里只能选择启用集合、顺序与参数；全部留空 = 既有行为（内置动作全启用）。
              </div>
              <div className="mt-2.5">
                <Grid cols={2}>
                  {fld({ name: 'growth_autotasks_disabled', label: '屏蔽的任务码（黑名单）', placeholder: '如 skill_1, black_cat', hint: '逗号分隔；同时命中白名单时以黑名单为准' })}
                  {fld({ name: 'growth_autotasks_only', label: '仅启用这些任务码（白名单）', placeholder: '留空 = 不启用白名单', hint: '非空时只有列出的任务参与自动化' })}
                  {fld({ name: 'growth_autotasks_order', label: '执行顺序覆盖', placeholder: '如 chat_5, first_buddy', hint: '列出的按本序在前（用于依赖前置）；未列出的按内置依赖序排后' })}
                  {fld({ name: 'growth_autotasks_mp_codes', label: '小程序口径任务码', placeholder: '如 +New_MP_Task', hint: '裸码 = 覆盖内置表；+ 前缀 = 追加；运行时差集探测到的码自动并入' })}
                </Grid>
              </div>
              <div className="mt-2.5">
                {sw('growth_autotasks_allow_unknown_claim', '未内置判据的任务只做「接受 + 达标领奖」')}
                <div className="mt-1.5 text-[11.5px] leading-relaxed text-[var(--ink-3)]">
                  打开后，上游已下发但尚无判据实现的任务不会被忽略：进度达标时自动领奖（绝不伪造事件）；未达标不排队。默认关闭。
                </div>
              </div>
              {taskActions.length > 0 && (
                <div className="mt-3 flex flex-col gap-1.5">
                  <div className="text-[11.5px] text-[var(--ink-3)]">内置动作码（共 {taskActions.length} 个，灰显 = 当前被策略屏蔽或不在白名单）：</div>
                  <div className="flex flex-wrap gap-1.5">
                    {taskActions.map((a) => (
                      <span
                        key={a.code}
                        title={a.desc || ''}
                        className={
                          'rounded px-1.5 py-0.5 font-[family-name:var(--mono)] text-[11px] ' +
                          (a.enabled ? 'bg-[var(--accent-soft)] text-[var(--accent)]' : 'bg-[var(--surface)] text-[var(--ink-3)] line-through opacity-70')
                        }
                      >
                        {a.code}{a.mp ? ' · mp' : ''}
                      </span>
                    ))}
                  </div>
                </div>
              )}
              <div className="mt-3 text-[11.5px] leading-relaxed text-[var(--ink-3)]">
                逐任务参数覆盖与动作模板已支持在「任务中心 → 活动自动化配置」可视化编辑；此处展示当前 <code>growth.autotasks.tasks</code> 快照，保存全局配置时原样保留。
              </div>
              {growthTasks && (
                <pre className="mt-1.5 max-h-[160px] overflow-auto whitespace-pre-wrap rounded bg-[var(--surface)] p-2.5 font-[family-name:var(--mono)] text-[11.5px] leading-relaxed">
                  {growthTasks}
                </pre>
              )}
            </div>
          </div>
        )}

        {sec === 'sec-pool' && (
          <div className="flex flex-col gap-4 rounded-xl border border-[var(--line)] bg-[var(--surface)] p-4">
            <h3 className="text-[14px] font-semibold">
              账号池与流量治理 <span className="ml-1 text-[12px] font-normal text-[var(--ink-3)]">并发上限、智能熔断、连败降权与积分路由</span>
            </h3>
            <SubHead>在途并发限制</SubHead>
            <Grid cols={3}>
              {fld({ name: 'max_in_flight', label: '单账号最大在途', type: 'number', min: 0, placeholder: '3', hint: '0 = 不限制' })}
              {fld({ name: 'max_in_flight_global', label: '国际版在途上限', type: 'number', min: 1, placeholder: '2', hint: 'global 域风控更紧' })}
              {fld({ name: 'ttl', label: '会话粘性 TTL', placeholder: '30m', hint: '相同会话固定账号的最长保留' })}
            </Grid>
            <SubHead>智能熔断与冷却退避</SubHead>
            <Grid cols={3}>
              {fld({ name: 'breaker_threshold', label: '连续失败熔断阈值', type: 'number', min: 1, placeholder: '3' })}
              {fld({ name: 'breaker_cooldown', label: '熔断基础时长', placeholder: '30m' })}
              {fld({ name: 'breaker_cooldown_max', label: '熔断退避上限', placeholder: '6h' })}
            </Grid>
            <Grid cols={2}>
              {fld({ name: 'soft_rate', label: '软限流冷却基数', placeholder: '600s' })}
              {fld({ name: 'soft_rate_max', label: '软冷却退避上限', placeholder: '2h' })}
            </Grid>
            <SubHead>连败降权保护</SubHead>
            <Grid cols={3}>
              {fld({ name: 'degrade_threshold', label: '连败降权阈值', type: 'number', min: 1, placeholder: '5' })}
              {fld({ name: 'degrade_cooldown', label: '连败降权时长', placeholder: '10m' })}
              {fld({ name: 'degrade_cooldown_max', label: '连败降权上限', placeholder: '2h' })}
            </Grid>
            <SubHead>权重补偿与积分路由</SubHead>
            <Grid cols={3}>
              {fld({ name: 'idle_weight_per_hour', label: '闲置补偿 / 小时', type: 'number', step: '0.1', placeholder: '0.5' })}
              {fld({ name: 'idle_weight_max', label: '闲置补偿上限', type: 'number', step: '0.1', placeholder: '5' })}
              {fld({ name: 'cost_explore_interval', label: '成本探索窗口', placeholder: '30m', hint: '0 关闭' })}
            </Grid>
            <Grid cols={2}>
              {fld({ name: 'credit_floor', label: '积分保底', type: 'number', min: 0, placeholder: '100', hint: '低于此值不再接收费模型；0 关闭' })}
              {fld({ name: 'expiring_soon', label: '快过期路由窗口', placeholder: '168h', hint: '窗口内权重 ×3；0 关闭' })}
            </Grid>
            <div className="flex flex-wrap gap-6 pt-1">
              {sw('prefer_expiring', '快过期积分优先')}
              {sw('session_sticky_enabled', '会话粘性路由')}
            </div>
          </div>
        )}

        {sec === 'sec-upstream' && (
          <div className="flex flex-col gap-4 rounded-xl border border-[var(--line)] bg-[var(--surface)] p-4">
            <h3 className="text-[14px] font-semibold">
              客户端仿真与上游 <span className="ml-1 text-[12px] font-normal text-[var(--ink-3)]">客户端/CLI 版本伪装与域名映射</span>
            </h3>
            <SubHead>客户端与 CLI 版本号伪装</SubHead>
            <Grid cols={2}>
              {fld({ name: 'cn_client_version', label: 'CN 客户端版本', placeholder: versionInfo?.builtin_cn_client || '留空 = 内置默认', hint: '' })}
              {fld({ name: 'cn_cli_version', label: 'CN CLI 版本', placeholder: versionInfo?.builtin_cn_cli || '留空 = 内置默认' })}
            </Grid>
            <Grid cols={2}>
              {fld({ name: 'global_client_version', label: 'Global 客户端版本', placeholder: versionInfo?.builtin_global_client || '留空 = 内置默认' })}
              {fld({ name: 'global_cli_version', label: 'Global CLI 版本', placeholder: versionInfo?.builtin_global_cli || '留空 = 内置默认' })}
            </Grid>
            <div className="flex items-end gap-3">
              <button
                type="button"
                className={btnGhost}
                onClick={() => {
                  const vi = versionInfo;
                  if (!vi) return toast('尚未读取到版本信息，请刷新配置后重试', 'err');
                  const filled: string[] = [];
                  if (vi.latest_cn) {
                    setValues((s) => ({ ...s, cn_client_version: vi.latest_cn! }));
                    filled.push('CN ' + vi.latest_cn);
                  }
                  if (vi.latest_global) {
                    setValues((s) => ({ ...s, global_client_version: vi.latest_global! }));
                    filled.push('Global ' + vi.latest_global);
                  }
                  if (!filled.length) return toast('没有可填入的版本（探测未成功）', 'err');
                  const notes = [...versionNotes('cn'), ...versionNotes('global')];
                  toast('已填入 ' + filled.join('、') + (notes.length ? '；' + notes.join('；') : ''), notes.length ? 'err' : 'ok');
                }}
              >
                一键填入已拉取版本
              </button>
              <span className="text-[11.5px] leading-relaxed text-[var(--ink-3)]">
                {versionInfo
                  ? `已拉取版本（${versionInfo.checked ? '已探测' : '未探测成功，显示内置基线'}）：国内 ${versionInfo.latest_cn || '-'} | 海外 ${versionInfo.latest_global || '-'}`
                  : ''}
                {versionInfo && [...versionNotes('cn'), ...versionNotes('global')].length ? '；' + [...versionNotes('cn'), ...versionNotes('global')].join('；') : ''}
              </span>
            </div>
            <div className="text-[11.5px] leading-relaxed text-[var(--ink-3)]">
              UA 与 X-IDE-Version 按「账号域 + 请求用途」生成，改动即时生效。产品名、Origin 及各用途 UA 覆盖需手工编辑 config.json 的 upstream.profiles。
            </div>
            <SubHead>模型默认域名</SubHead>
            <Grid cols={2}>
              {fld({
                name: 'model_default_realm',
                label: '裸模型名默认域',
                options: [
                  ['cn', 'cn — 裸名归国内版（默认）'],
                  ['global', 'global — 裸名归国际版'],
                  ['auto', 'auto — 自动判定（CN 优先）'],
                  ['auto:global,cn', 'auto:global,cn — 自动判定（Global 优先）'],
                ],
                hint: '显式前缀恒优先；改动即时生效',
              })}
            </Grid>
          </div>
        )}

        {sec === 'sec-prompt' && (
          <div className="flex flex-col gap-4 rounded-xl border border-[var(--line)] bg-[var(--surface)] p-4">
            <h3 className="text-[14px] font-semibold">
              网关提示词工程 <span className="ml-1 text-[12px] font-normal text-[var(--ink-3)]">系统级 System Prompt 注入与分域控制</span>
            </h3>
            <SubHead>1 · 怎么组合</SubHead>
            {/* 组合位置示意图 */}
            <div className="flex flex-wrap items-center gap-1.5 rounded-lg bg-[var(--surface-2)] px-3 py-2.5 text-[12px]">
              {(() => {
                const m = modeMap(String(values['prompt_mode'] ?? 'inject'));
                const CLS: Record<string, string> = {
                  gw: 'bg-[var(--accent-soft)] text-[var(--accent)]',
                  cl: 'bg-[var(--ok-soft)] text-[var(--ok)]',
                  drop: 'bg-[var(--surface)] text-[var(--ink-3)] line-through opacity-60',
                  usr: 'bg-[var(--surface)] text-[var(--ink-2)]',
                };
                return (
                  <>
                    {m.segs.map(([cls, label], i) => (
                      <span key={i} className="flex items-center gap-1.5">
                        {i > 0 && <span className="text-[var(--ink-3)]">→</span>}
                        <span className={'rounded px-2 py-0.5 ' + CLS[cls]}>{label}</span>
                      </span>
                    ))}
                    <span className="ml-2 text-[11.5px] text-[var(--ink-3)]">{m.cap}</span>
                  </>
                );
              })()}
            </div>
            {fld({ name: 'prompt_mode', label: '组合位置', options: MODES, hint: '改动即时生效；分域覆盖可给 CN / Global 各设一套' })}

            <SubHead>2 · 用什么内容（内联正文 &gt; 文件 &gt; 预设）</SubHead>
            <Grid cols={2}>
              {fld({ name: 'prompt_preset', label: '内置预设', options: presetOptions(false), hint: promptPresetHint() })}
              {fld({ name: 'prompt_file', label: '提示词文件（可选）', placeholder: '留空 = 用预设', hint: '路径不可读会在启动时报错' })}
            </Grid>
            {fld({ name: 'prompt_text', label: '内联正文（可选）', type: 'textarea', placeholder: '留空 = 用文件/预设。填写后优先生效。', hint: '填写即覆盖上方预设与文件' })}

            <SubHead>3 · 按域覆盖（可选，留空 = 继承顶层）</SubHead>
            <Grid cols={2}>
              {fld({ name: 'prompt_cn_mode', label: 'CN · 组合位置', options: [['', '继承顶层'], ...MODES] })}
              {fld({ name: 'prompt_global_mode', label: 'Global · 组合位置', options: [['', '继承顶层'], ...MODES] })}
            </Grid>
            <Grid cols={2}>
              {fld({ name: 'prompt_cn_preset', label: 'CN · 预设', options: presetOptions(true) })}
              {fld({ name: 'prompt_global_preset', label: 'Global · 预设', options: presetOptions(true) })}
            </Grid>
            <Grid cols={2}>
              {fld({ name: 'prompt_cn_text', label: 'CN · 内联正文', placeholder: '留空 = 用顶层素材' })}
              {fld({ name: 'prompt_global_text', label: 'Global · 内联正文', placeholder: '留空 = 用顶层素材' })}
            </Grid>
            <div className="text-[11.5px] text-[var(--ink-3)]">素材规则：任一域显式设了预设/文件/正文，该域素材完全由自己的设置决定；组合位置逐项回落顶层。</div>

            <SubHead>预览生效正文</SubHead>
            <div className="flex items-center gap-2">
              <button type="button" className={btnGhost} disabled={previewing} onClick={() => void doPreview()}>
                {previewing ? '预览中…' : '立即预览'}
              </button>
              <span className="text-[11.5px] text-[var(--ink-3)]">保存前先看 CN / Global 实际会发出去的内容、来源与字数。</span>
            </div>
            {preview && (
              <div className="flex flex-col gap-2.5">
                {preview.note && <div className="text-[12px] text-[var(--ink-3)]">{preview.note}</div>}
                {([['default', '默认（未知域兜底）'], ['cn', 'CN 账号'], ['global', 'Global 账号']] as const).map(([k, label]) => {
                  const r = preview[k];
                  if (!r) return null;
                  const bits = [`${label}：mode=${r.mode}`, `来源=${r.source}`, `预设=${r.preset || 'default'}`, r.inherited ? '继承顶层' : '本域覆盖'];
                  if (!r.error) bits.push(`${r.chars} 字 / ${r.bytes} 字节${r.truncated ? '（已截断）' : ''}`);
                  return (
                    <div key={k} className="rounded-lg border border-[var(--line-soft)] bg-[var(--surface-2)] p-3">
                      <div className="text-[12px] text-[var(--ink-2)]">{bits.join(' · ')}</div>
                      <pre className="mt-2 max-h-[160px] overflow-auto whitespace-pre-wrap rounded bg-[var(--surface)] p-2.5 font-[family-name:var(--mono)] text-[12px] leading-relaxed">
                        {r.error ? '解析失败：' + r.error : r.text || '（该模式不注入正文）'}
                      </pre>
                    </div>
                  );
                })}
                {preview.warnings && preview.warnings.length > 0 && (
                  <div className="text-[12px] text-[var(--warn)]">告警：{preview.warnings.join('；')}</div>
                )}
              </div>
            )}
          </div>
        )}

        {sec === 'sec-media' && (
          <div className="flex flex-col gap-4 rounded-xl border border-[var(--line)] bg-[var(--surface)] p-4">
            <h3 className="text-[14px] font-semibold">
              多模态与图片 <span className="ml-1 text-[12px] font-normal text-[var(--ink-3)]">工具结果图片 · 格式转码 · 体积压缩</span>
            </h3>
            <SubHead>工具结果图片</SubHead>
            <Grid cols={2}>
              {fld({
                name: 'media_tool_images',
                label: 'tool 消息图片处理',
                options: [
                  ['auto', 'auto — 原生 Chat 透传 / 跨协议抬升（默认）'],
                  ['passthrough', 'passthrough — 全部原样转发'],
                  ['hoist', 'hoist — 全部抬升为后续 user 图片消息'],
                  ['reject', 'reject — 明确 400'],
                ],
                hint: 'auto 下原生 Chat 保持透传，跨协议入口默认抬升（会改变图片角色与位置，非无损）',
              })}
            </Grid>
            <SubHead>图片转码与压缩（默认全关）</SubHead>
            {sw('media_image_transcode', '格式转码（gif / bmp / tiff → PNG）')}
            <div className="text-[11.5px] leading-relaxed text-[var(--ink-3)]">
              上游只原生接受 jpeg / png / webp。开启后 GIF 只保留首帧，重编码后仍超 5 MiB 会明确报错。
            </div>
            <Grid cols={2}>
              {fld({
                name: 'media_image_max_dimension',
                label: '压缩档位',
                options: [
                  ['0', '关闭 — 不缩放不压缩（默认）'],
                  ['1080', '1080 — 边长≤1080、base64≤500KB'],
                  ['2000', '2000 — 边长≤2000、base64≤5MB'],
                ],
                hint: '开启后超档图片等比缩放 + JPEG 质量阶梯重编码（有损）',
              })}
            </Grid>
            <div className="rounded-lg border-l-[3px] border-[var(--warn)] bg-[var(--warn-soft)] px-3.5 py-2.5 text-[12px] leading-relaxed">
              以上两项是显式声明的兼容变换：会改变出站图片字节（有损）。默认全关时只做形状与 5 MiB 硬限制校验。
            </div>
          </div>
        )}

        {sec === 'sec-fp-log' && (
          <div className="flex flex-col gap-4 rounded-xl border border-[var(--line)] bg-[var(--surface)] p-4">
            <h3 className="text-[14px] font-semibold">
              敏感指纹改写与日志 <span className="ml-1 text-[12px] font-normal text-[var(--ink-3)]">对话正文指纹脱敏与访问来源记录</span>
            </h3>
            <SubHead>指纹改写脱敏</SubHead>
            {sw('fingerprint_rewrite', '启用指纹改写')}
            <div className="text-[11.5px] leading-relaxed text-[var(--ink-3)]">
              上游按逐字拒绝的已知串（Claude Code / Codex 身份句等）藏在 user/assistant/tool 消息里时只能靠本项逐字改写。会修改用户可见内容，只用于绕过上游字面拒绝；开关与规则即时生效。
            </div>
            <div className="flex items-center gap-2 text-[12.5px] font-medium text-[var(--ink-2)]">
              自定义规则
              <span className="text-[11.5px] font-normal text-[var(--ink-3)]">
                {(() => {
                  const n = String(values['fingerprint_rules_text'] ?? '').split(/\r?\n/).filter((l) => {
                    const t = l.trim();
                    return t && !t.startsWith('#');
                  }).length;
                  const on = !!values['fingerprint_rewrite'];
                  return n + ' 条' + (n > 0 && !on ? '（开关未开，当前不生效）' : '');
                })()}
              </span>
            </div>
            <div className="flex flex-wrap gap-x-4 gap-y-1 text-[11.5px] text-[var(--ink-3)]">
              <span><code>词 =&gt; 新词</code> 逐字替换</span>
              <span><code>/词 =&gt; 新词</code> 忽略大小写</span>
              <span><code>!词</code> 整段删除</span>
              <span><code>/!词</code> 忽略大小写 + 删除</span>
              <span><code># …</code> 注释</span>
            </div>
            <Field
              spec={{ name: 'fingerprint_rules_text', label: '规则文本', type: 'textarea', rows: 5, placeholder: '内部代号X => 项目A\n/SecretSauce => sauce\n!x-legacy-tag' }}
              value={values['fingerprint_rules_text'] ?? ''}
              onChange={set('fingerprint_rules_text')}
            />
            <SubHead>访问日志记录</SubHead>
            {sw('request_client_info', '记录调用来源（客户端 IP / User-Agent）')}
            <div className="text-[11.5px] leading-relaxed text-[var(--ink-3)]">
              开启后「运行日志」页的请求记录会显示每次调用来自哪个 IP、用什么客户端。归档开关、保留天数与容量上限需手工编辑配置文件。
            </div>
          </div>
        )}

        {/* 保存条 */}
        <div className="sticky bottom-4 flex items-center gap-2.5 rounded-xl border border-[var(--line)] bg-[var(--raise)] px-4 py-3 shadow-lg">
          <span className="flex-1 truncate text-[12px] text-[var(--ink-3)]" title={note}>
            {note}
          </span>
          <button type="button" className={btnGhost} onClick={() => void load()}>
            放弃修改
          </button>
          <button type="submit" className={btnPrimary} disabled={saving}>
            {saving ? '保存中…' : '保存配置'}
          </button>
        </div>
      </div>
    </form>
  );
}
