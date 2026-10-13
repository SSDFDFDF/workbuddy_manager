/* 用量视图：KPI 卡（精简优化） + Token 时序图（紧凑视图） + 双 Tab 复用明细表（维度明细 / 积分扣除）+ 分页控制。 */
import { useCallback, useEffect, useState } from 'react';
import { api } from '../api';
import { useTimeRange, TimeRangeControl } from '../components/TimeRange';
import { Kpi, Empty, Loading, Pager } from '../components/ui';
import { StackedBars, toChartPoints } from '../components/StackedBars';
import { btnXs } from '../components/buttons';
import type { UsageRow, UsageSnapshot } from '../types';
import { fmtTok, fmtMs, fmtRate, fmtCredit, trimFixed, cacheRatePct } from '../fmt';

function sortRows(rows: UsageRow[], sort: string): UsageRow[] {
  const num = (v: unknown) => {
    const n = Number(v || 0);
    return Number.isFinite(n) ? n : 0;
  };
  const val = (a: UsageRow) =>
    sort === 'requests' ? num(a.requests) : sort === 'errors' ? num(a.errors) : sort === 'latency' ? num(a.avg_latency_ms) : num(a.total_tokens);
  return [...rows].sort((a, b) => val(b) - val(a));
}

/* 明细一行：名称 + 域（可选）+ 请求/失败/Token 构成徽章 + 性能（可选），单行紧凑布局。 */
function DimRow({ x, withRealm, withPerf }: { x: UsageRow; withRealm: boolean; withPerf: boolean }) {
  const tt = Number(x.total_tokens || 0);
  const pt = Number(x.prompt_tokens || 0);
  const ct = Number(x.completion_tokens || 0);
  const uid = String(x.key || '');
  const shortUid = uid.slice(0, 8);
  const nick = x.nickname || x.extra || '';
  return (
    <tr className="transition-colors hover:bg-[var(--surface-2)]/60 whitespace-nowrap">
      <td className="px-3.5 py-2 align-middle">
        {withRealm ? (
          <div className="flex items-center gap-2">
            <span className="font-medium text-[var(--ink)]">{nick || shortUid || '—'}</span>
            {shortUid && (
              <span
                className="rounded border border-[var(--line-soft)] bg-[var(--surface-2)] px-1.5 py-0.5 font-[family-name:var(--mono)] text-[11px] leading-tight text-[var(--ink-3)]"
                title={uid}
              >
                {shortUid}
              </span>
            )}
          </div>
        ) : (
          <span className="font-[family-name:var(--mono)] text-[12.5px] font-medium text-[var(--ink)]">{x.key || '—'}</span>
        )}
      </td>
      {withRealm && <td className="tabular px-3.5 py-2 align-middle text-[var(--ink-2)]">{x.realm || '—'}</td>}
      <td className="tabular px-3.5 py-2 align-middle text-[var(--ink)] font-medium">{fmtTok(x.requests)}</td>
      <td className="tabular px-3.5 py-2 align-middle">
        {x.errors ? <span className="text-[var(--warn)] font-medium">{fmtTok(x.errors)}</span> : <span className="text-[var(--ink-3)]">—</span>}
      </td>
      <td className="tabular px-3.5 py-2 align-middle">
        <div className="flex items-center gap-1.5">
          <span className="font-semibold text-[var(--ink)]">{fmtTok(tt)}</span>
          {tt > 0 && (
            <>
              <span
                className="inline-flex items-center rounded-md border border-[var(--accent)]/20 bg-[var(--accent-soft)] px-1.5 py-0.5 text-[11px] font-medium leading-tight text-[var(--accent)]"
                title={`读 ${fmtTok(pt)} (${((pt / tt) * 100).toFixed(1)}%)`}
              >
                读 {fmtTok(pt)}
              </span>
              <span
                className="inline-flex items-center rounded-md border border-[var(--ok)]/20 bg-[var(--ok-soft)] px-1.5 py-0.5 text-[11px] font-medium leading-tight text-[var(--ok)]"
                title={`取 ${fmtTok(ct)} (${((ct / tt) * 100).toFixed(1)}%)`}
              >
                取 {fmtTok(ct)}
              </span>
            </>
          )}
        </div>
      </td>
      {withPerf && (
        <>
          <td className="tabular px-3.5 py-2 align-middle text-[var(--ink)]">{fmtMs(x.avg_latency_ms)}</td>
          <td className="tabular px-3.5 py-2 align-middle text-[var(--ink-2)]">{fmtRate(x.avg_tokens_per_second)}</td>
        </>
      )}
    </tr>
  );
}

/* 缓存命中率格子。 */
function CacheCell({ hit, miss }: { hit?: number; miss?: number }) {
  const pct = cacheRatePct(hit, miss);
  if (pct == null) return <span className="text-[var(--ink-3)]">—</span>;
  return (
    <span
      className={
        'inline-flex items-center rounded-md px-1.5 py-0.5 text-[11px] font-semibold tabular ' +
        (pct > 0 ? 'bg-[var(--ok-soft)] text-[var(--ok)] border border-[var(--ok)]/20' : 'bg-[var(--surface-2)] text-[var(--ink-3)]')
      }
    >
      {pct > 0 ? `⚡ ${trimFixed(pct.toFixed(1))}%` : '0%'}
    </span>
  );
}

export function UsageView() {
  const range = useTimeRange('0');
  const [data, setData] = useState<UsageSnapshot | null>(null);
  const [error, setError] = useState<string | null>(null);

  // 表格 Tab：'dimension' (用量维度明细) | 'credit' (积分扣除明细)
  const [tableTab, setTableTab] = useState<'dimension' | 'credit'>('dimension');

  // 维度表过滤与分页
  const [dim, setDim] = useState<'account' | 'model' | 'realm'>('account');
  const [sort, setSort] = useState('total');
  const [dimPage, setDimPage] = useState(1);
  const [dimPageSize, setDimPageSize] = useState(20);

  // 积分扣除表过滤与分页
  const [creditDim, setCreditDim] = useState<'model' | 'account'>('model');
  const [creditPage, setCreditPage] = useState(1);
  const [creditPageSize, setCreditPageSize] = useState(20);

  const load = useCallback(async () => {
    setError(null);
    try {
      const qs = range.qs(false);
      const url = 'usage' + (qs ? '?' + qs : '');
      const d = await api<UsageSnapshot>(url);
      setData(d);
    } catch (e) {
      setError((e as Error).message || String(e));
    }
  }, [range]);

  useEffect(() => {
    void load();
  }, [load]);

  const d = data;
  const t = d?.totals || {};
  const total = Number(t.total_tokens || 0);
  const pt = Number(t.prompt_tokens || 0);
  const ct = Number(t.completion_tokens || 0);
  const reqs = Number(t.requests || 0);
  const errs = Number(t.errors || 0);
  const okRate = reqs ? ((reqs - errs) / reqs) * 100 : null;

  const dimMeta = { account: { withRealm: true, withPerf: true }, model: { withRealm: false, withPerf: false }, realm: { withRealm: false, withPerf: false } }[dim];
  const dimRows = sortRows(d?.[({ account: 'by_account', model: 'by_model', realm: 'by_realm' } as const)[dim]] || [], sort);
  const dimTotalPages = Math.max(1, Math.ceil(dimRows.length / dimPageSize));
  const curDimPage = Math.min(dimPage, dimTotalPages);
  const pagedDimRows = dimRows.slice((curDimPage - 1) * dimPageSize, curDimPage * dimPageSize);

  const creditRows = creditDim === 'model' ? d?.credit_by_model || [] : d?.credit_by_account || [];
  const creditTotalPages = Math.max(1, Math.ceil(creditRows.length / creditPageSize));
  const curCreditPage = Math.min(creditPage, creditTotalPages);
  const pagedCreditRows = creditRows.slice((curCreditPage - 1) * creditPageSize, curCreditPage * creditPageSize);

  const pts = toChartPoints(d?.series || []);
  const peak = pts.length ? pts.reduce((a, b) => (b.tt > a.tt ? b : a)) : null;
  const avg = pts.length ? pts.reduce((s, p) => s + p.tt, 0) / pts.length : 0;

  const rangeEcho = d?.window_from
    ? String(d.window_from).replace('T', ' ').slice(0, 16) + (d.window_to ? ' → ' + String(d.window_to).replace('T', ' ').slice(0, 16) : ' → 现在')
    : '';
  const note =
    (rangeEcho || range.label() ? (rangeEcho || range.label()) + ' · ' : '') +
    (d?.buckets || 0) + ' 分桶' +
    (d?.since ? ' · 始自 ' + d.since.replace('T', ' ').slice(0, 16) : '');

  return (
    <div className="flex flex-col gap-4">
      {/* 总览面板 */}
      <div className="rounded-xl border border-[var(--line)] bg-[var(--surface)] shadow-xs">
        <header className="flex flex-wrap items-center gap-2.5 px-4 py-3 border-b border-[var(--line-soft)]">
          <h3 className="text-[14px] font-semibold text-[var(--ink)]">用量与积分总览</h3>
          <span className="flex-1" />
          <span className="text-[12px] text-[var(--ink-3)] tabular" title={note}>
            {error ? '读取失败：' + error : note || '—'}
          </span>
          <TimeRangeControl range={range} onChange={() => void load()} />
          <button className={btnXs} onClick={() => void load()}>
            刷新
          </button>
        </header>

        {error ? (
          <Empty>读取用量失败：{error}</Empty>
        ) : !d ? (
          <Loading />
        ) : (
          <div className="p-4">
            {/* KPI 指标微卡（精简化展示） */}
            <div className="grid grid-cols-2 gap-2.5 sm:grid-cols-3 lg:grid-cols-6">
              <Kpi
                v={fmtTok(reqs)}
                k="请求数"
                tone={errs ? 'warn' : 'accent'}
                sub={okRate == null ? '—' : `成功率 ${okRate.toFixed(1)}%${errs ? ' · 失败 ' + errs : ''}`}
              />
              <Kpi
                v={fmtTok(total)}
                k="总 Token"
                tone="accent"
                sub={`读 ${fmtTok(pt)} · 取 ${fmtTok(ct)}`}
              />
              <Kpi
                v={fmtMs(t.avg_latency_ms)}
                k="平均延迟"
                tone="soft"
                sub={t.avg_tokens_per_second ? `速率 ${fmtRate(t.avg_tokens_per_second)}` : '延迟统计'}
              />
              <Kpi
                v={fmtCredit(t.credits)}
                k="扣除积分"
                tone="accent"
                sub="积分消耗总额"
              />
              <Kpi
                v={t.credits_per_1m_tokens != null && t.credit_tokens ? trimFixed(Number(t.credits_per_1m_tokens).toFixed(3)) : '—'}
                k="积分 / 1M Token"
                tone="ok"
                sub="单位均价成本"
              />
              <Kpi
                v={cacheRatePct(t.cache_hit_tokens, t.cache_miss_tokens) == null ? '—' : trimFixed(cacheRatePct(t.cache_hit_tokens, t.cache_miss_tokens)!.toFixed(1)) + '%'}
                k="缓存命中率"
                tone="mute"
                sub="前缀缓存命中比"
              />
            </div>

            {/* Token 时序图（简洁明了视图） */}
            <div className="mt-3.5 rounded-lg border border-[var(--line-soft)] bg-[var(--surface-2)]/30 p-3.5">
              <div className="mb-2.5 flex flex-wrap items-center gap-3 text-[12px] text-[var(--ink-3)]">
                <span className="font-semibold text-[var(--ink)]">Token 时序</span>
                {pts.length ? (
                  <span className="tabular text-[11.5px] text-[var(--ink-3)]">
                    {pts.length} 个时间桶 · 峰值 <span className="font-medium text-[var(--ink-2)]">{fmtTok(peak ? peak.tt : 0)}</span> · 均值 <span className="font-medium text-[var(--ink-2)]">{fmtTok(avg)}</span>
                  </span>
                ) : (
                  <span>—</span>
                )}
                <span className="flex-1" />
                <span className="flex items-center gap-3 tabular text-[11px]">
                  <span className="inline-flex items-center gap-1.5 text-[var(--ink-2)]">
                    <i className="h-2 w-2 rounded-xs bg-[var(--accent)]" />读 (Prompt)
                  </span>
                  <span className="inline-flex items-center gap-1.5 text-[var(--ink-2)]">
                    <i className="h-2 w-2 rounded-xs bg-[var(--ok)]" />取 (Completion)
                  </span>
                </span>
              </div>
              <div>
                {pts.length ? <StackedBars pts={pts} /> : <Empty>暂无用量数据，发起一次对话后再刷新。</Empty>}
              </div>
            </div>
          </div>
        )}
      </div>

      {/* 两个表格复用 Tab 切换展示卡片 */}
      <div className="rounded-xl border border-[var(--line)] bg-[var(--surface)] shadow-xs overflow-hidden">
        <header className="flex flex-wrap items-center gap-2.5 px-4 py-3 border-b border-[var(--line-soft)] bg-[var(--surface)]">
          {/* 主 Tab 选择器 */}
          <div className="flex items-center gap-1 rounded-lg bg-[var(--surface-2)] p-1">
            <button
              type="button"
              className={
                'flex items-center gap-1.5 rounded-md px-3 py-1 text-[12.5px] font-medium transition-all ' +
                (tableTab === 'dimension'
                  ? 'bg-[var(--surface)] text-[var(--accent)] font-semibold shadow-xs'
                  : 'text-[var(--ink-2)] hover:text-[var(--ink)]')
              }
              onClick={() => setTableTab('dimension')}
            >
              <span>用量维度明细</span>
              <span className="text-[11px] opacity-75 tabular">({dimRows.length})</span>
            </button>
            <button
              type="button"
              className={
                'flex items-center gap-1.5 rounded-md px-3 py-1 text-[12.5px] font-medium transition-all ' +
                (tableTab === 'credit'
                  ? 'bg-[var(--surface)] text-[var(--accent)] font-semibold shadow-xs'
                  : 'text-[var(--ink-2)] hover:text-[var(--ink)]')
              }
              onClick={() => setTableTab('credit')}
            >
              <span>积分扣除明细</span>
              <span className="text-[11px] opacity-75 tabular">({creditRows.length})</span>
            </button>
          </div>

          <span className="flex-1" />

          {/* Tab 1 专属子过滤器 */}
          {tableTab === 'dimension' && (
            <div className="flex flex-wrap items-center gap-2">
              <div className="flex gap-1">
                {([['account', '按账号'], ['model', '按模型'], ['realm', '按域']] as const).map(([k, label]) => (
                  <button
                    key={k}
                    className={
                      'rounded-md px-2 py-0.5 text-[12px] ' +
                      (dim === k ? 'bg-[var(--accent-soft)] font-medium text-[var(--accent)] border border-[var(--accent)]/20' : 'text-[var(--ink-3)] hover:text-[var(--ink)]')
                    }
                    onClick={() => {
                      setDim(k);
                      setDimPage(1);
                    }}
                  >
                    {label}
                  </button>
                ))}
              </div>
              <label className="flex items-center gap-1 text-[12px] text-[var(--ink-3)]">
                排序
                <select className="wb-select px-2 py-0.5 text-[11.5px]" value={sort} onChange={(e) => setSort(e.target.value)}>
                  <option value="total">合计 Token</option>
                  <option value="requests">请求数</option>
                  <option value="errors">失败数</option>
                  <option value="latency">平均延迟</option>
                </select>
              </label>
            </div>
          )}

          {/* Tab 2 专属子过滤器 */}
          {tableTab === 'credit' && (
            <div className="flex items-center gap-1">
              {([['model', '按模型'], ['account', '按账号']] as const).map(([k, label]) => (
                <button
                  key={k}
                  className={
                    'rounded-md px-2 py-0.5 text-[12px] ' +
                    (creditDim === k ? 'bg-[var(--accent-soft)] font-medium text-[var(--accent)] border border-[var(--accent)]/20' : 'text-[var(--ink-3)] hover:text-[var(--ink)]')
                  }
                  onClick={() => {
                    setCreditDim(k);
                    setCreditPage(1);
                  }}
                >
                  {label}
                </button>
              ))}
            </div>
          )}
        </header>

        {/* ── Tab 1 内容：用量维度明细 ── */}
        {tableTab === 'dimension' && (
          <div>
            <div className="overflow-x-auto">
              <table className="w-full border-collapse text-[13px]">
                <thead>
                  <tr className="border-b border-[var(--line-soft)] bg-[var(--surface-2)]/50 text-left text-[11.5px] font-semibold text-[var(--ink-2)] whitespace-nowrap">
                    <th className="px-3.5 py-2.5">{dim === 'account' ? '账号' : dim === 'model' ? '模型' : '域'}</th>
                    {dimMeta.withRealm && <th className="px-3.5 py-2.5">域</th>}
                    <th className="px-3.5 py-2.5">请求数</th>
                    <th className="px-3.5 py-2.5">失败数</th>
                    <th className="px-3.5 py-2.5">Token 构成 (读 / 取)</th>
                    {dimMeta.withPerf && <th className="px-3.5 py-2.5">平均延迟</th>}
                    {dimMeta.withPerf && <th className="px-3.5 py-2.5">平均速率</th>}
                  </tr>
                </thead>
                <tbody className="divide-y divide-[var(--line-soft)]/60">
                  {pagedDimRows.map((x, i) => (
                    <DimRow key={i} x={x} withRealm={dimMeta.withRealm} withPerf={dimMeta.withPerf} />
                  ))}
                  {!dimRows.length && (
                    <tr>
                      <td colSpan={7}>
                        <Empty>{error ? '读取用量失败' : '暂无数据'}</Empty>
                      </td>
                    </tr>
                  )}
                </tbody>
              </table>
            </div>

            {/* 维度表翻页器 */}
            <Pager
              page={curDimPage}
              totalPages={dimTotalPages}
              total={dimRows.length}
              pageSize={dimPageSize}
              pageSizeOptions={[10, 20, 50]}
              onPage={setDimPage}
              onPageSizeChange={(sz) => {
                setDimPageSize(sz);
                setDimPage(1);
              }}
            />
          </div>
        )}

        {/* ── Tab 2 内容：积分扣除明细 ── */}
        {tableTab === 'credit' && (
          <div>
            <div className="overflow-x-auto">
              <table className="w-full border-collapse text-[13px]">
                <thead>
                  <tr className="border-b border-[var(--line-soft)] bg-[var(--surface-2)]/50 text-left text-[11.5px] font-semibold text-[var(--ink-2)]">
                    <th className="px-3.5 py-2.5">{creditDim === 'model' ? '模型' : '账号'}</th>
                    {creditDim === 'model' && <th className="px-3.5 py-2.5">积分倍率</th>}
                    <th className="px-3.5 py-2.5">请求数</th>
                    <th className="px-3.5 py-2.5">扣除积分</th>
                    <th className="px-3.5 py-2.5">有效样本 Token</th>
                    <th className="px-3.5 py-2.5">积分 / 1M Token</th>
                    <th className="px-3.5 py-2.5">缓存命中率</th>
                  </tr>
                </thead>
                <tbody className="divide-y divide-[var(--line-soft)]/60">
                  {pagedCreditRows.map((r, i) => (
                    <tr key={i} className="transition-colors hover:bg-[var(--surface-2)]/60">
                      <td className="px-3.5 py-2.5">
                        {creditDim === 'model' ? (
                          <span className="font-[family-name:var(--mono)] text-[12.5px] font-medium text-[var(--ink)]">{r.key || '—'}</span>
                        ) : (
                          <>
                            <div className="font-medium text-[var(--ink)]">{r.nickname || String(r.key || '').slice(0, 8) || '—'}</div>
                            <div className="text-[11px] text-[var(--ink-3)] font-[family-name:var(--mono)]">{r.realm || ''} · {String(r.key || '').slice(0, 8)}</div>
                          </>
                        )}
                      </td>
                      {creditDim === 'model' && <td className="tabular px-3.5 py-2.5 text-[var(--ink-2)]">{r.rate ? 'x' + r.rate : '—'}</td>}
                      <td className="tabular px-3.5 py-2.5 text-[var(--ink)] font-medium">{fmtTok(r.requests)}</td>
                      <td className="tabular px-3.5 py-2.5 font-bold text-[var(--accent)]">{fmtCredit(r.credits)}</td>
                      <td className="tabular px-3.5 py-2.5 text-[var(--ink)]">{fmtTok(r.credit_tokens)}</td>
                      <td className="tabular px-3.5 py-2.5 font-medium text-[var(--ink)]">
                        {r.credit_samples && r.credit_tokens && r.credits_per_1m_tokens != null ? trimFixed(Number(r.credits_per_1m_tokens).toFixed(4)) : '—'}
                      </td>
                      <td className="px-3.5 py-2.5">
                        <CacheCell hit={r.cache_hit_tokens} miss={r.cache_miss_tokens} />
                      </td>
                    </tr>
                  ))}
                  {!creditRows.length && (
                    <tr>
                      <td colSpan={7}>
                        <Empty>{error ? '读取用量失败' : '暂无积分扣除记录；升级前仅含 Token 的历史不会伪造积分。'}</Empty>
                      </td>
                    </tr>
                  )}
                </tbody>
              </table>
            </div>

            {/* 积分表翻页器 */}
            <Pager
              page={curCreditPage}
              totalPages={creditTotalPages}
              total={creditRows.length}
              pageSize={creditPageSize}
              pageSizeOptions={[10, 20, 50]}
              onPage={setCreditPage}
              onPageSizeChange={(sz) => {
                setCreditPageSize(sz);
                setCreditPage(1);
              }}
            />
          </div>
        )}
      </div>
    </div>
  );
}
