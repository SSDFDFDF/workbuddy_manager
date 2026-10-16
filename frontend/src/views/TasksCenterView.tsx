/* 任务中心视图：成长任务队列（扫描 / 排队执行 / 进度轮询）+ 活动自动化规则配置 + 券码查询弹窗。 */
import { useEffect, useMemo, useRef, useState } from 'react';
import { api } from '../api';
import { toast, ask } from '../toast';
import type { QueueItem, QueueStatus, ScanAccount, VoucherAccount, Voucher, TaskActionItem } from '../types';
import { Dialog } from '../components/Dialog';
import { State, Dots, Empty, Tag } from '../components/ui';
import { btnPrimary, btnGhost, btnXs, btnXsPrimary } from '../components/buttons';
import { qrSVG, qrMatrix } from '../qr';
import { copyText } from '../clipboard';
import { ago } from '../fmt';

interface Group {
  uid: string;
  nick?: string;
  rows: { code: string; kind?: string; prog?: string; status?: string; message?: string; title?: string }[];
}

const ST_WORDS: Record<string, string> = { done: '完成', running: '执行中', error: '失败', skipped: '跳过', pending: '排队', scan: '待执行' };

function groupsFromScan(d: { accounts?: ScanAccount[] }): Group[] {
  const groups: Group[] = [];
  for (const a of d.accounts || []) {
    const rows = (a.growth || []).map((t) => ({
      code: t.task_code,
      prog: t.target ? `${t.current ?? 0}/${t.target}` : '—',
      status: 'scan',
      title: t.title,
    }));
    if (rows.length) groups.push({ uid: a.uid, nick: a.nickname, rows });
  }
  return groups;
}

function groupsFromQueue(items: QueueItem[]): Group[] {
  const by = new Map<string, Group>();
  for (const it of items || []) {
    let g = by.get(it.uid);
    if (!g) {
      g = { uid: it.uid, nick: it.nickname, rows: [] };
      by.set(it.uid, g);
    }
    g.rows.push({ code: it.code || '', kind: it.kind, status: it.status, message: it.message });
  }
  return [...by.values()];
}

/* 券码卡。 */
function VoucherCard({ v }: { v: Voucher }) {
  const [showQr, setShowQr] = useState(false);
  const expired = v.valid_to && new Date(v.valid_to) < new Date();
  return (
    <div className="rounded-xl border border-[var(--line)] bg-[var(--surface-2)] p-3.5">
      <div className="flex items-center gap-2">
        <span className="font-medium">{v.prize_name || v.sku_code || '券'}</span>
        {expired ? <Tag tone="bad">已过期</Tag> : <Tag tone="ok">可使用</Tag>}
      </div>
      <div className="mt-1 text-[12px] text-[var(--ink-3)]">
        {v.valid_to ? '有效期至 ' + v.valid_to : '长期有效'}
        {v.granted_at ? ' · ' + v.granted_at.slice(0, 10) + ' 抽中' : ''}
      </div>
      <div className="mt-2.5 flex items-center gap-2">
        <span className="text-[12px] text-[var(--ink-3)]">券码</span>
        <code className="font-[family-name:var(--mono)] text-[13px] font-medium">{v.code || '-'}</code>
        <span className="flex flex-1 justify-end gap-1.5">
          {v.code && (
            <button className={btnXs} onClick={() => setShowQr(!showQr)}>
              二维码
            </button>
          )}
          <button
            className={btnXs}
            onClick={async () => {
              try {
                await copyText(v.code || '');
                toast('券码已复制', 'ok');
              } catch {
                toast('复制失败，请手动选择券码', 'err');
              }
            }}
          >
            复制
          </button>
        </span>
      </div>
      {showQr && v.code && (
        <div
          className="mt-2.5 flex justify-center rounded-lg bg-white p-2"
          dangerouslySetInnerHTML={{
            __html: (() => {
              try {
                return qrSVG(qrMatrix(v.code!), 148);
              } catch {
                return '<span style="color:#999">生成失败</span>';
              }
            })(),
          }}
        />
      )}
    </div>
  );
}

function VouchersDialog({ onClose }: { onClose: () => void }) {
  const [data, setData] = useState<VoucherAccount[] | null>(null);
  const [error, setError] = useState('');

  const load = async () => {
    setError('');
    try {
      const d = await api<{ accounts: VoucherAccount[] }>('school/vouchers');
      setData(d.accounts || []);
    } catch (e) {
      setError((e as Error).message);
    }
  };

  useEffect(() => {
    void load();
  }, []);

  const ok = (data || []).filter((a) => !a.error);
  const total = ok.reduce((n, a) => n + (a.vouchers || []).length, 0);
  const errs = (data || []).filter((a) => a.error);

  return (
    <Dialog
      title="开学季 · 我的券码"
      hint="抽奖抽中的第三方券（KFC / 瑞幸 / 酷狗等），券码到店到对应 app/小程序兑换。"
      onClose={onClose}
      footer={
        <>
          <span className="text-[12px] text-[var(--ink-3)]">
            {data ? (total ? `${total} 张券 · ` + ok.filter((a) => !(a.vouchers || []).length).length + ' 个账号未抽中' : '') : ''}
          </span>
          <span className="flex-1" />
          <button className={btnXs} onClick={() => void load()}>
            刷新
          </button>
          <button className={btnXs + ' !border-transparent !bg-[var(--accent)] !text-[var(--accent-ink)]'} onClick={onClose}>
            关闭
          </button>
        </>
      }
    >
      {error ? (
        <State kind="err">{error}</State>
      ) : data == null ? (
        <State>
          <Dots>查询中</Dots>
        </State>
      ) : (
        <div className="flex flex-col gap-4">
          {ok
            .filter((a) => (a.vouchers || []).length)
            .map((a) => (
              <div key={a.uid}>
                <div className="mb-2 flex items-center justify-between text-[13px]">
                  <span className="font-medium">{a.nickname || a.uid}</span>
                  <span className="text-[var(--ink-3)]">{a.vouchers!.length} 张</span>
                </div>
                <div className="grid grid-cols-[repeat(auto-fill,minmax(260px,1fr))] gap-2.5">
                  {(a.vouchers || []).map((v, i) => (
                    <VoucherCard key={i} v={v} />
                  ))}
                </div>
              </div>
            ))}
          {!total && <Empty big="🎟️">还没有抽到券</Empty>}
          {errs.length > 0 && (
            <div className="text-[12.5px] text-[var(--warn)]">
              查询失败：{errs.map((a) => `${a.nickname || a.uid.slice(0, 8)}（${a.error}）`).join('、')}
            </div>
          )}
        </div>
      )}
    </Dialog>
  );
}

/* 任务自动化规则配置弹窗。 */
function TaskConfigDialog({
  item,
  onClose,
  onSaved,
}: {
  item: TaskActionItem;
  onClose: () => void;
  onSaved: () => void;
}) {
  const [enabled, setEnabled] = useState(item.enabled);
  const [mp, setMp] = useState(!!item.mp);
  const [actionType, setActionType] = useState(item.action_type || (item.builtin ? 'builtin' : 'claim_only'));
  const [model, setModel] = useState(item.model || 'glm-5.2');
  const [target, setTarget] = useState(item.target ? String(item.target) : '0');
  const [gap, setGap] = useState(item.gap || '');
  const [window, setWindow] = useState(item.window || '');
  const [activityId, setActivityId] = useState(item.activity_id || '');
  const [saving, setSaving] = useState(false);

  const save = async () => {
    setSaving(true);
    try {
      await api('tasks/policy', {
        method: 'POST',
        body: JSON.stringify({
          code: item.code,
          enabled,
          mp,
          action_type: actionType,
          model: actionType === 'model_chat' ? model.trim() : undefined,
          target: Number(target) || 0,
          gap: gap.trim(),
          window: window.trim(),
          activity_id: activityId.trim(),
        }),
      });
      toast(`任务 ${item.code} 配置已保存并热生效`, 'ok');
      onSaved();
      onClose();
    } catch (e) {
      toast('保存失败：' + (e as Error).message, 'err');
    } finally {
      setSaving(false);
    }
  };

  return (
    <Dialog
      title={`配置任务自动化 · ${item.code}`}
      hint={item.title ? `${item.title} — ${item.desc || ''}` : item.desc}
      width={560}
      onClose={onClose}
      footer={
        <>
          <span className="flex-1" />
          <button className={btnGhost} onClick={onClose}>
            取消
          </button>
          <button className={btnPrimary} disabled={saving} onClick={() => void save()}>
            {saving ? '保存中…' : '保存并热生效'}
          </button>
        </>
      }
    >
      <div className="flex flex-col gap-4 text-[13px]">
        {/* 基础开关 */}
        <div className="flex items-center justify-between rounded-lg border border-[var(--line-soft)] bg-[var(--surface-2)]/40 p-3">
          <div>
            <div className="font-medium text-[var(--ink)]">参与自动化执行</div>
            <div className="text-[11.5px] text-[var(--ink-3)]">关闭后该任务将被策略屏蔽（加入黑名单）</div>
          </div>
          <input
            type="checkbox"
            className="wb-checkbox h-4 w-4 cursor-pointer"
            checked={enabled}
            onChange={(e) => setEnabled(e.target.checked)}
          />
        </div>

        <div className="flex items-center justify-between rounded-lg border border-[var(--line-soft)] bg-[var(--surface-2)]/40 p-3">
          <div>
            <div className="font-medium text-[var(--ink)]">小程序口径（miniprogram）</div>
            <div className="text-[11.5px] text-[var(--ink-3)]">开启后该任务的列表拉取、accept 和领奖均附带小程序专属请求头</div>
          </div>
          <input
            type="checkbox"
            className="wb-checkbox h-4 w-4 cursor-pointer"
            checked={mp}
            onChange={(e) => setMp(e.target.checked)}
          />
        </div>

        {/* 动作模板选择 */}
        <div className="flex flex-col gap-1.5">
          <label className="font-medium text-[var(--ink)]">动作模式 / 模板类型</label>
          <select
            className="wb-select px-2.5 py-1.5"
            value={actionType}
            onChange={(e) => setActionType(e.target.value)}
          >
            {item.builtin && <option value="builtin">内置原生动作（推荐内置预置任务使用）</option>}
            <option value="chat_count">通用对话活跃上报模板（补足 chat_request_send）</option>
            <option value="model_chat">指定模型对话打卡模板（真实对话 + 模型活跃上报）</option>
            <option value="mp_chat">通用小程序对话模板（mini 对话上报）</option>
            <option value="claim_only">仅接受 + 达标自动领奖（不伪造任何事件）</option>
          </select>
          <div className="text-[11.5px] leading-relaxed text-[var(--ink-3)]">
            {actionType === 'builtin' && '执行内置的高级逆向事件链与专有指纹。'}
            {actionType === 'chat_count' && '自动计算差额并逐条上报对话活跃事件，适合各类普通“完成 N 次对话”活动。'}
            {actionType === 'model_chat' && '调用指定模型发一条简短问候并上报模型活跃度，适合指定模型打卡任务。'}
            {actionType === 'mp_chat' && '以真人节奏（45s+抖动防反作弊）上报小程序对话事件并领奖。'}
            {actionType === 'claim_only' && '仅负责自动报名并在已达标时自动领取奖励，绝不伪造行为事件。'}
          </div>
        </div>

        {/* 针对 model_chat 的参数 */}
        {actionType === 'model_chat' && (
          <div className="flex flex-col gap-1">
            <label className="font-medium text-[var(--ink)]">对话模型标识 (model)</label>
            <input
              type="text"
              className="wb-input rounded-md border border-[var(--line)] bg-[var(--surface)] px-2.5 py-1.5"
              placeholder="如 glm-5.2"
              value={model}
              onChange={(e) => setModel(e.target.value)}
            />
            <div className="text-[11.5px] text-[var(--ink-3)]">上游任务要求的模型 ID，默认 glm-5.2。</div>
          </div>
        )}

        {/* 通用参数区 */}
        {(actionType === 'chat_count' || actionType === 'model_chat' || actionType === 'mp_chat' || actionType === 'builtin') && (
          <div className="grid grid-cols-2 gap-3">
            <div className="flex flex-col gap-1">
              <label className="font-medium text-[var(--ink)]">目标次数覆盖 (target)</label>
              <input
                type="number"
                min="0"
                className="wb-input rounded-md border border-[var(--line)] bg-[var(--surface)] px-2.5 py-1.5"
                placeholder="0 = 跟随上游"
                value={target}
                onChange={(e) => setTarget(e.target.value)}
              />
              <div className="text-[11px] text-[var(--ink-3)]">0 表示以上游下发为准（推荐）</div>
            </div>
            <div className="flex flex-col gap-1">
              <label className="font-medium text-[var(--ink)]">事件间隔 (gap)</label>
              <input
                type="text"
                className="wb-input rounded-md border border-[var(--line)] bg-[var(--surface)] px-2.5 py-1.5"
                placeholder="如 1.05s / 45s"
                value={gap}
                onChange={(e) => setGap(e.target.value)}
              />
              <div className="text-[11px] text-[var(--ink-3)]">Go 时长格式（如 1050ms, 45s），留空用默认</div>
            </div>
          </div>
        )}

        {/* 针对 mp_chat 的参数 */}
        {actionType === 'mp_chat' && (
          <div className="flex flex-col gap-1">
            <label className="font-medium text-[var(--ink)]">活动标识 (activity_id)</label>
            <input
              type="text"
              className="wb-input rounded-md border border-[var(--line)] bg-[var(--surface)] px-2.5 py-1.5"
              placeholder="如 school_open_day_2026（可留空）"
              value={activityId}
              onChange={(e) => setActivityId(e.target.value)}
            />
          </div>
        )}

        {/* 时间窗口 */}
        <div className="flex flex-col gap-1">
          <label className="font-medium text-[var(--ink)]">计数时间窗口 (window)</label>
          <input
            type="text"
            className="wb-input rounded-md border border-[var(--line)] bg-[var(--surface)] px-2.5 py-1.5"
            placeholder="如 23:00-08:00（支持跨零点，留空全天有效）"
            value={window}
            onChange={(e) => setWindow(e.target.value)}
          />
          <div className="text-[11.5px] text-[var(--ink-3)]">仅在该时段内执行行为上报（如夜猫子类任务）</div>
        </div>
      </div>
    </Dialog>
  );
}

export function TasksCenterView() {
  const [tab, setTab] = useState<'queue' | 'config'>('queue');
  const [groups, setGroups] = useState<Group[] | null>(null);
  const [progress, setProgress] = useState<QueueStatus | null>(null);
  const [summary, setSummary] = useState('');
  const [scanning, setScanning] = useState(false);
  const [starting, setStarting] = useState(false);
  const [conc, setConc] = useState(1);
  const [vouchers, setVouchers] = useState(false);
  const [emptyText, setEmptyText] = useState<{ t: string; d: string }>({
    t: '还没有扫描过',
    d: '扫描所有账号的成长任务与开学季待办，把没做的排成一列，一键执行。',
  });
  const timerRef = useRef<number | null>(null);
  const seqRef = useRef(0);

  /* 规则与动作配置列表 */
  const [actionsList, setActionsList] = useState<TaskActionItem[] | null>(null);
  const [actionsLoading, setActionsLoading] = useState(false);
  const [configItem, setConfigItem] = useState<TaskActionItem | null>(null);
  const [filterType, setFilterType] = useState<'all' | 'unconfigured' | 'builtin' | 'custom' | 'disabled'>('all');
  const [searchKw, setSearchKw] = useState('');
  /* 手动拉取上游活动：同步中 / 本次新发现的码（高亮）/ 最近一次拉取时刻 */
  const [syncing, setSyncing] = useState(false);
  const [newCodes, setNewCodes] = useState<string[]>([]);
  const [syncedAt, setSyncedAt] = useState('');

  const stopPoll = () => {
    if (timerRef.current) {
      clearInterval(timerRef.current);
      timerRef.current = null;
    }
  };
  useEffect(() => stopPoll, []);

  const render = (gs: Group[], q: QueueStatus | null) => {
    setGroups(gs);
    setProgress(q);
    setSummary(gs.reduce((n, g) => n + g.rows.length, 0) + ' 项');
  };

  const startPolling = () => {
    stopPoll();
    timerRef.current = setInterval(async () => {
      if (document.hidden) return; // 切后台/最小化没人看，别白打 tasks/queue
      try {
        const q = await api<QueueStatus>('tasks/queue');
        if (!q.started) return;
        if (seqRef.current && q.seq !== seqRef.current) return;
        render(groupsFromQueue(q.items || []), q);
        if (!q.running) {
          stopPoll();
          toast('任务队列执行结束', 'ok');
        }
      } catch {
        /* 轮询失败静默 */
      }
    }, 3000);
  };

  /* 切回视图时若本页启动的队列仍在跑则恢复轮询。 */
  useEffect(() => {
    (async () => {
      try {
        const q = await api<QueueStatus>('tasks/queue');
        if (timerRef.current) return;
        if (q.started && q.running && (!seqRef.current || q.seq === seqRef.current)) startPolling();
      } catch {
        /* 静默 */
      }
    })();
    return stopPoll;
  }, []);

  const loadActions = async () => {
    setActionsLoading(true);
    try {
      const d = await api<{ actions: TaskActionItem[]; synced_at?: string }>('tasks/actions');
      setActionsList(d.actions || []);
      if (d.synced_at) setSyncedAt(d.synced_at);
    } catch (e) {
      toast('拉取活动列表失败：' + (e as Error).message, 'err');
    } finally {
      setActionsLoading(false);
    }
  };

  /* 手动拉取上游活动目录（只读）：让上游新下发的活动不必先跑一遍待办扫描
     也能出现在配置页；本次新发现的码在表格里高亮。 */
  const syncActivities = async () => {
    setSyncing(true);
    try {
      const r = await api<{
        accounts: number;
        pulled: number;
        total: number;
        new: number;
        new_codes?: string[];
        errors?: { uid: string; nickname?: string; error: string }[];
        synced_at?: string;
      }>('tasks/sync_activities', { method: 'POST' });
      // 只有拉到东西才更新目录状态：失败不该抹掉上次成功拉取的“上次拉取/新发现”。
      if (r.pulled > 0) {
        setNewCodes(r.new_codes || []);
        if (r.synced_at) setSyncedAt(r.synced_at);
      }
      await loadActions();
      const errs = r.errors || [];
      if (!r.accounts) {
        toast('没有可拉取的账号：global / 企业版账号没有 CN 成长任务体系', 'err');
        return;
      }
      if (!r.pulled) {
        const e = errs[0];
        toast('拉取失败：' + (e ? `${e.nickname || e.uid.slice(0, 8)}（${e.error}）` : '上游无响应'), 'err');
        return;
      }
      if (r.new > 0) {
        setFilterType('unconfigured'); // 直接跳到新发现，省一次过滤点击
        setSearchKw(''); // 同时清掉搜索词：新发现被搜索词挡住就白跳了
      }
      toast(
        `已拉取 ${r.pulled}/${r.accounts} 个账号：上游共 ${r.total} 项活动` +
          (r.new ? `，新发现 ${r.new} 项` : '，无新活动') +
          (errs.length ? `；${errs.length} 个账号失败` : ''),
        'ok',
      );
    } catch (e) {
      toast('拉取上游活动失败：' + (e as Error).message, 'err');
    } finally {
      setSyncing(false);
    }
  };

  useEffect(() => {
    void loadActions();
  }, []);

  const scanAll = async () => {
    stopPoll();
    setScanning(true);
    try {
      const d = await api<{ accounts: ScanAccount[] }>('tasks/scan_all', { method: 'POST' });
      const gs = groupsFromScan(d);
      render(gs, null);
      if (!gs.length) setEmptyText({ t: '没有待办任务 🎉', d: '全部账号的成长任务与开学季活动都已完成，明日再来。' });
      void loadActions(); // 扫描后刷新活动全景目录（发现的新活动自动并入）
    } catch (e) {
      toast((e as Error).message, 'err');
    } finally {
      setScanning(false);
    }
  };

  const runQueue = async () => {
    const c = conc || 1;
    if (!ask(`扫描全部账号待办并排队执行（账号并发 ${c}，账号内串行）。\n含真实对话的任务耗时较长，确认继续？`)) return;
    setStarting(true);
    try {
      const r = await api<{ started?: boolean; message?: string; seq?: number; total?: number }>('tasks/run_queue', {
        method: 'POST',
        body: JSON.stringify({ concurrency: c }),
      });
      if (!r.started) {
        toast(r.message || '没有待办任务', 'ok');
        return;
      }
      seqRef.current = r.seq || 0;
      toast(`队列已启动：${r.total} 项（并发 ${c}）`, 'ok');
      startPolling();
    } catch (e) {
      toast((e as Error).message, 'err');
    } finally {
      setStarting(false);
    }
  };

  const items = progress?.items || [];
  const done = items.filter((it) => it.status === 'done' || it.status === 'error' || it.status === 'skipped').length;

  /* 统计未配置活动数量 */
  const unconfiguredCount = useMemo(() => {
    return (actionsList || []).filter((a) => a.unconfigured).length;
  }, [actionsList]);

  /* 过滤配置列表 */
  const filteredActions = useMemo(() => {
    let list = actionsList || [];
    if (filterType === 'unconfigured') list = list.filter((a) => a.unconfigured);
    else if (filterType === 'builtin') list = list.filter((a) => a.builtin);
    else if (filterType === 'custom') list = list.filter((a) => !a.builtin && !a.unconfigured);
    else if (filterType === 'disabled') list = list.filter((a) => !a.enabled && !a.unconfigured);

    if (searchKw.trim()) {
      const kw = searchKw.trim().toLowerCase();
      list = list.filter((a) => a.code.toLowerCase().includes(kw) || (a.title && a.title.toLowerCase().includes(kw)) || (a.desc && a.desc.toLowerCase().includes(kw)));
    }
    return list;
  }, [actionsList, filterType, searchKw]);

  return (
    <div className="grid grid-cols-[240px_1fr] gap-4">
      {/* 侧栏 */}
      <aside className="flex flex-col gap-3 self-start rounded-xl border border-[var(--line)] bg-[var(--surface)] p-4">
        <div className="flex items-center gap-1.5 text-[13px] font-medium text-[var(--ink)]">
          <svg viewBox="0 0 16 16" width="14" height="14" fill="none" stroke="currentColor" strokeWidth="1.6">
            <path d="M3 2.5h10v9H8l-3 3v-3H3zM6 6.5h4M6 9h2.5" />
          </svg>
          任务控制台
        </div>
        <button className={btnPrimary + ' w-full'} disabled={starting} onClick={() => void runQueue()}>
          {starting ? '启动中…' : '执行全部待办'}
        </button>
        <button className={btnGhost + ' w-full'} disabled={scanning} onClick={() => void scanAll()}>
          {scanning ? '扫描中…' : '扫描账号待办'}
        </button>
        <button
          className={(tab === 'config' ? btnPrimary : btnGhost) + ' w-full'}
          onClick={() => {
            setTab(tab === 'config' ? 'queue' : 'config');
            if (tab !== 'config') void loadActions();
          }}
        >
          {tab === 'config' ? '返回执行队列' : '活动与自动化配置'}
        </button>
        <button className={btnGhost + ' w-full'} title="开学季抽奖抽中的第三方券码" onClick={() => setVouchers(true)}>
          查询抽奖券码
        </button>
        <div className="mt-1 flex flex-col gap-1">
          <span className="text-[12px] text-[var(--ink-3)]">执行并发限制</span>
          <select className="wb-select px-2 py-1.5 text-[13px]" value={conc} onChange={(e) => setConc(Number(e.target.value) || 1)}>
            <option value={1}>1 个账号并发</option>
            <option value={2}>2 个账号并发</option>
            <option value={3}>3 个账号并发</option>
          </select>
          <span className="text-[11.5px] text-[var(--ink-3)]">单次推进的并发账号数</span>
        </div>
        <div className="mt-2 border-t border-[var(--line-soft)] pt-3 text-[11.5px] leading-relaxed text-[var(--ink-3)]">
          内置数十种逆向事件链与指纹；新活动可在「活动配置」中一键绑定动作模板，无需手写代码。
        </div>
      </aside>

      {/* 主展示区 */}
      <div className="flex flex-col gap-3">
        {/* 未配置新活动提醒横幅 */}
        {unconfiguredCount > 0 && tab === 'queue' && (
          <div className="flex items-center justify-between rounded-xl border border-[var(--warn)]/30 bg-[var(--warn-soft)]/20 px-4 py-2.5 text-[12.5px] text-[var(--ink)]">
            <div className="flex items-center gap-2">
              <span className="text-[14px]">💡</span>
              <span>
                检测到上游下发了 <b>{unconfiguredCount}</b> 项未配置自动化的新活动，绑定动作模板后即可全自动打卡！
              </span>
            </div>
            <button
              className={btnXsPrimary}
              onClick={() => {
                setTab('config');
                setFilterType('unconfigured');
              }}
            >
              前往配置规则
            </button>
          </div>
        )}

        <div className="rounded-xl border border-[var(--line)] bg-[var(--surface)]">
          <header className="flex items-center gap-3 border-b border-[var(--line-soft)] px-4 py-3">
            <div className="flex items-center gap-1 rounded-lg bg-[var(--surface-2)] p-1">
              <button
                type="button"
                className={'rounded-md px-3 py-1 text-[12.5px] font-medium transition-all ' + (tab === 'queue' ? 'bg-[var(--surface)] text-[var(--accent)] font-semibold shadow-xs' : 'text-[var(--ink-2)] hover:text-[var(--ink)]')}
                onClick={() => setTab('queue')}
              >
                成长任务队列
              </button>
              <button
                type="button"
                className={'flex items-center gap-1 rounded-md px-3 py-1 text-[12.5px] font-medium transition-all ' + (tab === 'config' ? 'bg-[var(--surface)] text-[var(--accent)] font-semibold shadow-xs' : 'text-[var(--ink-2)] hover:text-[var(--ink)]')}
                onClick={() => {
                  setTab('config');
                  void loadActions();
                }}
              >
                <span>活动自动化配置</span>
                {unconfiguredCount > 0 && (
                  <span className="rounded-full bg-[var(--bad)] px-1.5 py-px text-[10px] text-white font-bold leading-none">
                    {unconfiguredCount}
                  </span>
                )}
              </button>
            </div>
            <span className="flex-1" />
            <span className="text-[12px] text-[var(--ink-3)]">
              {tab === 'queue'
                ? summary
                : `共 ${(actionsList || []).length} 项活动` + (syncedAt ? ` · 上次拉取 ${ago(syncedAt)}` : ' · 未手动拉取过')}
            </span>
          </header>

          {/* Tab 1: 执行队列 */}
          {tab === 'queue' && (
            <div>
              {progress && items.length > 0 && (
                <div className="flex items-center gap-3 px-4 pt-3.5 pb-1">
                  <div className="h-2 flex-1 overflow-hidden rounded-full bg-[var(--surface-2)] p-0.5">
                    <i
                      className="block h-full rounded-full bg-[var(--accent)] transition-[width] duration-500 shadow-xs"
                      style={{ width: (items.length ? Math.round((done / items.length) * 100) : 0) + '%' }}
                    />
                  </div>
                  <span className="tabular text-[12px] font-medium text-[var(--ink-2)]">
                    {progress.running ? <span className="text-[var(--accent)]">执行中 </span> : '已结束 '}
                    {done} / {items.length} ({items.length ? Math.round((done / items.length) * 100) : 0}%)
                  </span>
                </div>
              )}

              {groups == null || !groups.length ? (
                <div className="flex flex-col items-center gap-1.5 px-4 py-10 text-center text-[var(--ink-3)]">
                  <svg viewBox="0 0 48 48" width="42" height="42" fill="none" stroke="currentColor" strokeWidth="2" className="mb-1.5 opacity-55">
                    <rect x="8" y="6" width="24" height="36" rx="3" />
                    <path d="M14 16h12M14 24h12M14 32h7" opacity=".45" />
                    <circle cx="35" cy="33" r="8" />
                    <path d="m40 38 5 5" />
                  </svg>
                  <div className="font-medium text-[var(--ink-2)]">{emptyText.t}</div>
                  <div className="max-w-[34em] text-[12.5px]">{emptyText.d}</div>
                </div>
              ) : (
                <div className="p-4 space-y-3">
                  {groups.map((g) => (
                    <div key={g.uid} className="rounded-lg border border-[var(--line-soft)] bg-[var(--surface-2)]/30 p-3.5 shadow-2xs">
                      <header className="mb-2.5 flex items-center justify-between">
                        <div className="flex items-center gap-2">
                          <span className="font-semibold text-[13.5px] text-[var(--ink)]">{g.nick || g.uid.slice(0, 12)}</span>
                          <span className="font-[family-name:var(--mono)] text-[11px] text-[var(--ink-3)]">{g.uid.slice(0, 8)}</span>
                        </div>
                        <span className="rounded-full bg-[var(--surface)] px-2 py-0.5 text-[11px] tabular text-[var(--ink-3)] border border-[var(--line-soft)]">
                          {g.rows.length} 项待办
                        </span>
                      </header>
                      <div className="divide-y divide-[var(--line-soft)]/50 rounded-md border border-[var(--line-soft)]/60 bg-[var(--surface)] overflow-hidden">
                        {g.rows.map((it, i) => {
                          const dotCls =
                            it.status === 'running'
                              ? 'bg-[var(--accent)] wb-pulse-anim'
                              : it.status === 'error'
                                ? 'bg-[var(--bad)]'
                                : it.status === 'skipped'
                                  ? 'bg-[var(--ink-3)]'
                                  : it.status === 'done'
                                    ? 'bg-[var(--ok)]'
                                    : 'bg-[var(--line)]';
                          return (
                            <div
                              key={i}
                              className="flex items-center gap-3 px-3 py-2 text-[12.5px] transition-colors hover:bg-[var(--surface-2)]/50"
                              title={it.message || ''}
                            >
                              <span className="font-[family-name:var(--mono)] text-[11px] text-[var(--ink-3)] min-w-[70px] truncate">{it.code}</span>
                              <span className="flex-1 font-medium text-[var(--ink)] truncate">
                                {it.title || (it.kind === 'school' ? <span>开学季闭环 <Tag tone="mute">开学季</Tag></span> : it.code)}
                              </span>
                              {it.prog && <span className="tabular text-[11.5px] text-[var(--ink-3)]">{it.prog}</span>}
                              <span className="flex items-center gap-1.5 text-[11.5px] min-w-[55px]">
                                <span className={'h-2 w-2 rounded-full shrink-0 ' + dotCls} />
                                <span className="font-medium text-[var(--ink-2)]">{ST_WORDS[it.status || ''] || it.status}</span>
                              </span>
                              {it.message && <span className="max-w-[200px] truncate text-[11px] text-[var(--ink-3)]">{it.message}</span>}
                            </div>
                          );
                        })}
                      </div>
                    </div>
                  ))}
                </div>
              )}
            </div>
          )}

          {/* Tab 2: 活动与自动化配置 */}
          {tab === 'config' && (
            <div className="p-4 space-y-3">
              {/* 搜索与过滤工具栏 */}
              <div className="flex flex-wrap items-center justify-between gap-2.5 pb-1 text-[12.5px]">
                <div className="flex flex-wrap items-center gap-1">
                  {[
                    ['all', '全部活动'],
                    ['unconfigured', `未配置新活动${unconfiguredCount ? ` (${unconfiguredCount})` : ''}`],
                    ['builtin', '内置预置'],
                    ['custom', '自定义模板'],
                    ['disabled', '已禁用'],
                  ].map(([k, label]) => (
                    <button
                      key={k}
                      className={
                        'rounded-md px-2.5 py-1 text-[12px] transition-colors ' +
                        (filterType === k
                          ? 'bg-[var(--accent-soft)] font-medium text-[var(--accent)] border border-[var(--accent)]/20'
                          : 'text-[var(--ink-3)] hover:text-[var(--ink)]')
                      }
                      onClick={() => setFilterType(k as any)}
                    >
                      {label}
                    </button>
                  ))}
                </div>
                <div className="flex items-center gap-2">
                  <input
                    type="text"
                    className="wb-input rounded-md border border-[var(--line)] bg-[var(--surface-2)] px-2.5 py-1 text-[12px]"
                    placeholder="按任务码/标题搜索…"
                    value={searchKw}
                    onChange={(e) => setSearchKw(e.target.value)}
                  />
                  <button
                    className={btnXsPrimary}
                    disabled={syncing}
                    title="按账号拉取上游成长任务清单（默认 + 小程序口径）并入活动目录；只读，不执行任何任务"
                    onClick={() => void syncActivities()}
                  >
                    {syncing ? '拉取中…' : '拉取上游活动'}
                  </button>
                  <button className={btnXs} onClick={() => void loadActions()}>
                    刷新
                  </button>
                </div>
              </div>

              {actionsLoading ? (
                <div className="py-12 text-center">
                  <Dots>加载活动清单中</Dots>
                </div>
              ) : !filteredActions.length ? (
                <div className="py-12 text-center text-[var(--ink-3)]">
                  <div>没有找到符合条件的活动</div>
                  {filterType === 'unconfigured' && (
                    <div className="mt-1 text-[12px]">点上方「拉取上游活动」可从上游同步最新活动目录（无需先扫描待办）</div>
                  )}
                </div>
              ) : (
                <div className="overflow-x-auto rounded-lg border border-[var(--line-soft)]">
                  <table className="w-full border-collapse text-[12.5px]">
                    <thead>
                      <tr className="border-b border-[var(--line-soft)] bg-[var(--surface-2)]/60 text-left text-[11.5px] font-semibold text-[var(--ink-2)]">
                        <th className="px-3.5 py-2 whitespace-nowrap">活动 / 任务码</th>
                        <th className="px-3 py-2 whitespace-nowrap">口径</th>
                        <th className="px-3 py-2 whitespace-nowrap">类型属性</th>
                        <th className="px-3 py-2 whitespace-nowrap">生效动作</th>
                        <th className="px-3 py-2 whitespace-nowrap">自动化状态</th>
                        <th className="px-3 py-2 text-right whitespace-nowrap">操作</th>
                      </tr>
                    </thead>
                    <tbody className="divide-y divide-[var(--line-soft)]/50">
                      {filteredActions.map((act) => {
                        const isNew = newCodes.includes(act.code);
                        return (
                          <tr
                            key={act.code}
                            className={
                              'transition-colors hover:bg-[var(--surface-2)]/40' + (isNew ? ' bg-[var(--ok-soft)]/25' : '')
                            }
                          >
                            <td className="px-3.5 py-2.5">
                              <div className="flex items-center gap-1.5">
                                <span className="font-medium text-[var(--ink)]">{act.title || act.code}</span>
                                {isNew && <Tag tone="ok">新发现</Tag>}
                              </div>
                              <div className="font-[family-name:var(--mono)] text-[11px] text-[var(--ink-3)]">
                                {act.code}
                              </div>
                            </td>
                            <td className="px-3 py-2.5 whitespace-nowrap">
                              {act.mp ? <Tag tone="accent">小程序</Tag> : <Tag tone="mute">PC/Web</Tag>}
                            </td>
                            <td className="px-3 py-2.5 whitespace-nowrap">
                              {act.builtin ? (
                                <Tag tone="ok">内置预置</Tag>
                              ) : act.unconfigured ? (
                                <Tag tone="warn">未配置</Tag>
                              ) : (
                                <Tag tone="accent">自定义模板</Tag>
                              )}
                            </td>
                            <td className="px-3 py-2.5">
                              <div className="text-[12px] font-medium text-[var(--ink-2)]">
                                {act.action_type === 'chat_count' ? (
                                  <span>对话补足 {act.target ? `(目标 ${act.target})` : ''}</span>
                                ) : act.action_type === 'model_chat' ? (
                                  <span>模型打卡 ({act.model || 'glm-5.2'})</span>
                                ) : act.action_type === 'mp_chat' ? (
                                  <span>小程序对话</span>
                                ) : act.action_type === 'claim_only' ? (
                                  <span className="text-[var(--ink-3)]">仅达标领奖</span>
                                ) : act.builtin ? (
                                  <span>原生事件链</span>
                                ) : (
                                  <span className="text-[var(--warn)]">未绑定动作</span>
                                )}
                              </div>
                              {act.desc && (
                                <div className="text-[11px] text-[var(--ink-3)] max-w-[280px] truncate" title={act.desc}>
                                  {act.desc}
                                </div>
                              )}
                            </td>
                            <td className="px-3 py-2.5 whitespace-nowrap">
                              {act.unconfigured ? (
                                <Tag tone="mute">未加入</Tag>
                              ) : act.enabled ? (
                                <Tag tone="ok">启用中</Tag>
                              ) : (
                                <Tag tone="bad">已禁用</Tag>
                              )}
                            </td>
                            <td className="px-3 py-2.5 text-right whitespace-nowrap">
                              <button
                                className={act.unconfigured ? btnXsPrimary : btnXs}
                                onClick={() => setConfigItem(act)}
                              >
                                {act.unconfigured ? '快速配置' : '配置规则'}
                              </button>
                            </td>
                          </tr>
                        );
                      })}
                    </tbody>
                  </table>
                </div>
              )}
            </div>
          )}
        </div>
      </div>

      {vouchers && <VouchersDialog onClose={() => setVouchers(false)} />}
      {configItem && (
        <TaskConfigDialog
          item={configItem}
          onClose={() => setConfigItem(null)}
          onSaved={() => {
            // 刚配置过的码不再是「新发现」（若它本在本次高亮列表里）
            setNewCodes((codes) => codes.filter((c) => c !== configItem.code));
            void loadActions();
          }}
        />
      )}
    </div>
  );
}
