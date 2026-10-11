/* 积分任务弹窗：查上游任务进度，接受（报名）/ 领取 / 一键完成可自动任务。 */
import { useCallback, useEffect, useState } from 'react';
import { api } from '../api';
import { toast, ask } from '../toast';
import { useData } from '../data';
import type { GrowthTask, TaskActionItem } from '../types';
import { Dialog } from '../components/Dialog';
import { Tag, Dots, State, Empty, Loading } from '../components/ui';
import { btnXsPrimary, btnXs, btnPrimary, btnGhost } from '../components/buttons';

/* 可自动完成的任务（与后端 autoActions 表一致）。 */
const AUTO_TASKS: Record<string, string> = {
  chat_5: '上报 5 条对话活跃事件（自动补足差额）',
  first_buddy: '上报解锁 → 同意协议 → 领取第一只 Buddy',
  'Model_chat_GLM5.2': '接受任务 → glm-5.2 真实对话一次 → 对齐模型上报',
  RichMeow_Chat: '桌面指纹事件链上报（已验证可点亮）',
  Buddy_App: '上报「进入 Buddy 应用」事件链（已验证可点亮）',
  Buddy_App_QQ: '上报「进入企鹅教师助手」事件链（已验证可点亮）',
  automation_1: '上报「定时任务创建」事件（已验证可点亮）',
  Library_read: '上报「读资料库介绍」事件（已验证可点亮）',
  template_5: '上报「使用模板创建任务」事件组 ×5',
  playbook_prompt: '上报「灵感案例做同款发送 Prompt」事件组',
  create_canvas: '上报「设计创意画布创建」事件组（+300 分）',
  expert_5: '真实专家召唤+使用链 ×5',
  Expert_team_use_3: '真实专家团召唤+使用链 ×3',
  Hp_Appearance: '设置主题 API + 皮肤生效事件',
  black_cat: '夜猫子：23:00–08:00 窗口内 glm-5.2 对话补足',
  Expert_lighthouse: '真实轻量云专家召唤+使用链',
  skill_1: '真实对话 + skill_info 技能加载事件',
  school_season: '校园日（小程序口径）：accept → mini 对话 → 领奖',
  Sequential_Tasks_1: '小程序首对话：accept → mini 对话上报 → 领奖',
  Sequential_Tasks_2: '小程序选专家对话：市场专家 id → expert_actual_use 上报 → 领奖',
  Sequential_Tasks_3: '小程序五次对话：accept → mini 对话上报 ×5 → 领奖',
  Sequential_Tasks_4: '小程序定时任务（预留）：accept → 定时任务创建事件 → 领奖',
  Sequential_Tasks_5: '小程序使用 GLM5.2（预留）：带模型字段的 mini 对话上报 → 领奖',
  Sequential_Tasks_6: '小程序十次对话（预留）：mini 对话上报 ×target → 领奖',
  Sequential_Tasks_7: '体验灵感功能（预留）：灵感事件组 → 领奖',
};

function TaskRow({
  t,
  onAct,
  busyCode,
  dynamicActions,
}: {
  t: GrowthTask;
  onAct: (kind: 'accept' | 'claim' | 'auto', code: string) => void;
  busyCode: string | null;
  dynamicActions: Record<string, { desc: string; enabled: boolean }>;
}) {
  const cur = t.current ?? 0;
  const tgt = t.target ?? 0;
  const prog = tgt ? `${cur} / ${tgt}` : tgt === 0 && cur > 0 ? String(cur) : '—';
  const parts: string[] = [];
  if (t.credit) parts.push('+' + t.credit + ' 分');
  if (t.energy) parts.push('+' + t.energy + ' 能');
  if (t.reward_buddy) parts.push('Buddy');
  const reward = parts.length ? parts.join(' ') : '—';

  const badge = t.claimed ? (
    <Tag tone="ok">已领取</Tag>
  ) : t.claimable ? (
    <Tag tone="warn">可领取</Tag>
  ) : t.locked ? (
    <Tag tone="mute">未解锁</Tag>
  ) : t.accept_status === 'accepted' ? (
    <Tag tone="mute">进行中</Tag>
  ) : (
    <Tag tone="mute">未接受</Tag>
  );

  const busy = busyCode === t.task_code;
  const tip = [t.title, t.task_desc || t.description, t.jump_url ? '跳转：' + t.jump_url : ''].filter(Boolean).join('\n');
  const dyn = dynamicActions[t.task_code];
  const autoDesc = AUTO_TASKS[t.task_code] || (dyn?.enabled ? dyn.desc : undefined);

  return (
    <tr title={tip}>
      <td>
        <div className="font-medium">{t.title || t.task_code}</div>
        <div className="font-[family-name:var(--mono)] text-[11px] text-[var(--ink-3)]">
          {t.task_code}
          {t.tag ? ' · ' + t.tag : ''}
        </div>
      </td>
      <td className="tabular">{prog}</td>
      <td className="tabular">{reward}</td>
      <td>{badge}</td>
      <td>
        {t.claimed || t.locked ? null : t.claimable ? (
          <button className={btnXsPrimary} disabled={busy} onClick={() => onAct('claim', t.task_code)}>
            {busy ? '执行中…' : '领取'}
          </button>
        ) : autoDesc ? (
          <button className={btnXsPrimary} disabled={busy} title={autoDesc} onClick={() => onAct('auto', t.task_code)}>
            {busy ? '执行中…' : '一键完成'}
          </button>
        ) : t.accept_status === 'accepted' ? null : (
          <button className={btnXs} disabled={busy} onClick={() => onAct('accept', t.task_code)}>
            接受
          </button>
        )}
      </td>
    </tr>
  );
}

export function TasksDialog({ uid, onClose }: { uid: string; onClose: () => void }) {
  const { refresh } = useData();
  const [tasks, setTasks] = useState<GrowthTask[] | null>(null);
  const [error, setError] = useState('');
  const [busyCode, setBusyCode] = useState<string | null>(null);
  const [acceptingAll, setAcceptingAll] = useState(false);
  const [autoAll, setAutoAll] = useState(false);
  const [dynamicActions, setDynamicActions] = useState<Record<string, { desc: string; enabled: boolean }>>({});

  useEffect(() => {
    (async () => {
      try {
        const d = await api<{ actions: TaskActionItem[] }>('tasks/actions');
        const m: Record<string, { desc: string; enabled: boolean }> = {};
        for (const a of d.actions || []) {
          m[a.code] = { desc: a.desc || '', enabled: a.enabled };
        }
        setDynamicActions(m);
      } catch {
        /* 静默 */
      }
    })();
  }, []);

  const load = useCallback(async () => {
    setError('');
    try {
      const d = await api<{ tasks: GrowthTask[] }>('accounts/' + encodeURIComponent(uid) + '/tasks');
      const list = d.tasks || [];
      list.sort((a, b) => Number(b.claimed) - Number(a.claimed) || Number(b.claimable) - Number(a.claimable) || String(a.task_code).localeCompare(String(b.task_code)));
      setTasks(list);
    } catch (e) {
      setError((e as Error).message);
      setTasks([]);
    }
  }, [uid]);

  useEffect(() => {
    void load();
  }, [load]);

  const base = 'accounts/' + encodeURIComponent(uid) + '/tasks';

  const onAct = async (kind: 'accept' | 'claim' | 'auto', code: string) => {
    setBusyCode(code);
    try {
      if (kind === 'auto') {
        const r = await api<{ skipped?: boolean; message?: string; progress_before?: number; progress_after?: number; claimed?: boolean; claimable?: boolean; claim_error?: string; attempt?: boolean }>(base + '/auto', {
          method: 'POST',
          body: JSON.stringify({ task_code: code }),
        });
        if (r.skipped) {
          toast(r.message || '已跳过', 'ok');
        } else {
          const advanced = r.progress_before !== r.progress_after;
          let msg = r.message || '已执行';
          if (r.progress_after) msg += `（进度 ${r.progress_before} → ${r.progress_after}）`;
          if (r.claimed) msg += '，奖励已自动到账';
          else if (r.attempt && !advanced) msg += '；进度未动，该任务可能需要官方客户端';
          toast(msg, r.claimed || advanced ? 'ok' : 'err');
        }
        void refresh(true);
      } else {
        const path = base + '/' + (kind === 'claim' ? 'claim' : 'accept');
        const body = kind === 'claim' ? { task_code: code } : { task_codes: [code] };
        await api(path, { method: 'POST', body: JSON.stringify(body) });
        toast(kind === 'claim' ? '已领取奖励' : '已接受任务', 'ok');
        if (kind === 'claim') void refresh(true);
      }
    } catch (e) {
      toast((e as Error).message, 'err');
    } finally {
      setBusyCode(null);
      void load();
    }
  };

  return (
    <Dialog
      title="积分任务"
      hint={uid.slice(0, 16)}
      width={940}
      onClose={onClose}
      footer={
        <>
          <span className="flex-1" />
          <button
            className={btnGhost}
            disabled={acceptingAll}
            onClick={async () => {
              setAcceptingAll(true);
              try {
                const r = await api<{ accepted?: number; failed?: unknown[]; message?: string }>(base + '/accept_all', { method: 'POST' });
                const n = r.accepted || 0;
                if (r.failed && r.failed.length) toast(`已接受 ${n} 个，${r.failed.length} 个被上游拒绝（可重试）`, 'err');
                else toast(n ? `已接受 ${n} 个任务` : r.message || '所有任务均已接受', 'ok');
              } catch (e) {
                toast((e as Error).message, 'err');
              } finally {
                setAcceptingAll(false);
                void load();
              }
            }}
          >
            {acceptingAll ? '接受中…' : '全部接受'}
          </button>
          <button
            className={btnPrimary}
            disabled={autoAll}
            onClick={async () => {
              if (!ask('将依次执行可自动任务（含真实对话，约 1-2 分钟），确认继续？')) return;
              setAutoAll(true);
              try {
                const r = await api<{ results?: { status?: string }[] }>(base + '/auto_all', { method: 'POST' });
                const rs = r.results || [];
                const okN = rs.filter((x) => x.status === 'done').length;
                const skipN = rs.filter((x) => x.status === 'skipped').length;
                const errN = rs.filter((x) => x.status === 'error').length;
                toast(`执行完成：成功 ${okN} 项，跳过 ${skipN} 项${errN ? '，失败 ' + errN + ' 项' : ''}`, errN ? 'err' : 'ok');
              } catch (e) {
                toast((e as Error).message, 'err');
              } finally {
                setAutoAll(false);
                void load();
              }
            }}
          >
            {autoAll ? '执行中…' : '一键完成可自动任务'}
          </button>
          <button className={btnGhost} onClick={() => void load()}>
            重新查询
          </button>
          <button className={btnGhost} onClick={onClose}>
            关闭
          </button>
        </>
      }
    >
      <div className="mb-3 text-[12px] text-[var(--ink-3)]">
        查询上游任务进度；「接受」为报名（幂等），「领取」在进度达标后可用。所有操作走网关，无需外部脚本。
      </div>
      {error ? (
        <State kind="err">{error}</State>
      ) : tasks == null ? (
        <Loading>
          <Dots>查询中</Dots>
        </Loading>
      ) : tasks.length === 0 ? (
        <Empty>该账号暂无任务</Empty>
      ) : (
        <table className="w-full border-collapse text-[13px]">
          <thead>
            <tr className="border-b border-[var(--line-soft)] text-left text-[11.5px] text-[var(--ink-3)]">
              <th className="px-2 py-2 font-medium">任务</th>
              <th className="px-2 py-2 font-medium">进度</th>
              <th className="px-2 py-2 font-medium">奖励</th>
              <th className="px-2 py-2 font-medium">状态</th>
              <th className="px-2 py-2" />
            </tr>
          </thead>
          <tbody>
            {tasks.map((t) => (
              <TaskRow key={t.task_code} t={t} onAct={onAct} busyCode={busyCode} dynamicActions={dynamicActions} />
            ))}
          </tbody>
        </table>
      )}
    </Dialog>
  );
}
