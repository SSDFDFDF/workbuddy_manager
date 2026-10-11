/* StackedBars 时序堆叠柱图：简洁明了的时间序列用量图表。
   自适应容器真实像素宽度（ResizeObserver），杜绝宽屏下 SVG viewBox 缩放导致的文字与轴线变大。
   双段堆叠（Prompt 读入 / Completion 产出）、清晰刻度网格、交互悬浮 Tooltip。 */
import { useMemo, useState, useRef, useEffect } from 'react';
import { fmtTok, fmtNum } from '../fmt';

export interface ChartPoint {
  t: number; // 毫秒
  raw: string; // 原始标签
  pt: number;
  ct: number;
  tt: number;
  req: number;
  scope?: string;
}

function formatTickLabel(p: { t: number; scope?: string }): string {
  const d = new Date(p.t);
  const p2 = (n: number) => String(n).padStart(2, '0');
  return p.scope === 'day' ? `${d.getMonth() + 1}-${p2(d.getDate())}` : `${p2(d.getHours())}:00`;
}

function formatFullTime(p: ChartPoint): string {
  const d = new Date(p.t);
  const p2 = (n: number) => String(n).padStart(2, '0');
  const dateStr = `${d.getFullYear()}-${p2(d.getMonth() + 1)}-${p2(d.getDate())}`;
  if (p.scope === 'day') return dateStr;
  return `${dateStr} ${p2(d.getHours())}:${p2(d.getMinutes())}`;
}

export function StackedBars({ pts, tick }: { pts: ChartPoint[]; tick?: (p: { t: number; scope?: string }) => string }) {
  const containerRef = useRef<HTMLDivElement>(null);
  const [containerWidth, setContainerWidth] = useState<number>(0);
  const [hoverIdx, setHoverIdx] = useState<number | null>(null);

  // 监听容器真实像素宽度，1:1 匹配 SVG viewBox，防止宽屏下等比拉大字体
  useEffect(() => {
    if (!containerRef.current) return;
    const update = () => {
      if (containerRef.current) {
        const w = containerRef.current.clientWidth;
        if (w > 0) setContainerWidth(w);
      }
    };
    update();
    const ro = new ResizeObserver((entries) => {
      for (const entry of entries) {
        const w = Math.round(entry.contentRect.width);
        if (w > 0) setContainerWidth(w);
      }
    });
    ro.observe(containerRef.current);
    return () => ro.disconnect();
  }, []);

  const W = containerWidth || 960;
  const H = 140;

  const geo = useMemo(() => {
    if (!pts.length) return null;
    const PL = 46;
    const PR = 16;
    const PT = 12;
    const PB = 22;
    const iw = Math.max(10, W - PL - PR);
    const ih = Math.max(10, H - PT - PB);
    const t0 = pts[0].t;
    const span = Math.max(1, pts[pts.length - 1].t - t0);
    const maxVal = Math.max(1, ...pts.map((p) => p.tt));

    // 计算柱宽：留出适度间距，不拥挤也不过于纤细
    let minGap = Infinity;
    for (let i = 1; i < pts.length; i++) minGap = Math.min(minGap, pts[i].t - pts[i - 1].t);
    if (!isFinite(minGap) || minGap <= 0) minGap = span;
    const rawBw = iw * (minGap / span) * 0.65;
    const bw = Math.max(3.5, Math.min(20, rawBw));

    const xOf = (t: number) => PL + bw / 2 + ((t - t0) / span) * Math.max(1, iw - bw);
    const yOf = (v: number) => PT + ih - ih * (v / maxVal);

    return { W, H, PL, PR, PT, PB, iw, ih, t0, span, maxVal, bw, xOf, yOf };
  }, [pts, W, H]);

  if (!geo || !pts.length) {
    return (
      <div className="flex h-32 items-center justify-center text-[12.5px] text-[var(--ink-3)]">
        暂无时序数据
      </div>
    );
  }

  const { PL, PR, PT, ih, maxVal, bw, xOf } = geo;
  const yBase = PT + ih;
  const tl = tick || formatTickLabel;

  // X 轴刻度：根据当前容器实际像素宽度自适应数量，保持间距舒适（约 110px 一个刻度）
  const dynamicTickCount = Math.min(10, Math.max(2, Math.floor((W - PL - PR) / 110)));
  const TICKS = Math.min(dynamicTickCount, pts.length);
  const used = new Set<number>();
  const ticks: { x: number; anchor: 'start' | 'middle' | 'end'; text: string }[] = [];
  for (let k = 0; k < TICKS; k++) {
    const target = geo.t0 + geo.span * (TICKS === 1 ? 0.5 : k / (TICKS - 1));
    let bi = 0;
    let best = Infinity;
    for (let i = 0; i < pts.length; i++) {
      const d = Math.abs(pts[i].t - target);
      if (d < best) {
        best = d;
        bi = i;
      }
    }
    if (used.has(bi)) continue;
    used.add(bi);
    const cx = xOf(pts[bi].t);
    const anchor = cx < PL + 20 ? 'start' : cx > W - PR - 20 ? 'end' : 'middle';
    ticks.push({ x: Math.max(PL, Math.min(W - PR, cx)), anchor, text: tl({ t: pts[bi].t, scope: pts[bi].scope }) });
  }

  // 悬浮点与 Tooltip 位置（直接使用像素坐标，杜绝百分比漂移）
  const activePt = hoverIdx !== null && pts[hoverIdx] ? pts[hoverIdx] : null;
  const activeX = activePt ? xOf(activePt.t) : 0;

  return (
    <div
      ref={containerRef}
      className="relative select-none w-full"
      onMouseLeave={() => setHoverIdx(null)}
    >
      <svg
        viewBox={`0 0 ${W} ${H}`}
        style={{ width: '100%', height: `${H}px` }}
        role="img"
        preserveAspectRatio="none"
      >
        <defs>
          <linearGradient id="tokGradPrompt" x1="0" y1="0" x2="0" y2="1">
            <stop offset="0%" stopColor="var(--accent)" stopOpacity="0.95" />
            <stop offset="100%" stopColor="var(--accent)" stopOpacity="0.6" />
          </linearGradient>
          <linearGradient id="tokGradCompletion" x1="0" y1="0" x2="0" y2="1">
            <stop offset="0%" stopColor="var(--ok)" stopOpacity="0.95" />
            <stop offset="100%" stopColor="var(--ok)" stopOpacity="0.6" />
          </linearGradient>
        </defs>

        {/* Y 轴刻度 + 水平背景虚线 */}
        {[0, 1, 2, 3].map((step) => {
          const y = PT + ih - (ih * step) / 3;
          const val = (maxVal * step) / 3;
          return (
            <g key={step}>
              <line
                x1={PL}
                y1={y}
                x2={W - PR}
                y2={y}
                stroke="var(--line-soft)"
                strokeDasharray={step === 0 ? undefined : '3 3'}
                strokeOpacity={step === 0 ? 0.9 : 0.6}
              />
              <text
                x={PL - 6}
                y={y + 3.5}
                textAnchor="end"
                className="fill-[var(--ink-3)] text-[10px] tabular"
              >
                {fmtTok(val)}
              </text>
            </g>
          );
        })}

        {/* 悬浮列背景高亮柱 */}
        {activePt && (
          <rect
            x={activeX - bw * 1.2}
            y={PT}
            width={bw * 2.4}
            height={ih}
            fill="var(--accent)"
            opacity={0.08}
            rx={3}
          />
        )}

        {/* 柱状条绘制（双段堆叠） */}
        {pts.map((p, i) => {
          const x = xOf(p.t) - bw / 2;
          const isHovered = hoverIdx === i;
          const hTot = p.tt > 0 ? Math.max(2, ih * (p.tt / maxVal)) : 0;
          const hP = p.tt > 0 ? (p.pt ? Math.max(1, (hTot * p.pt) / p.tt) : 0) : 0;
          const hC = p.tt > 0 ? Math.max(0, hTot - hP) : 0;

          return (
            <g
              key={i}
              className="cursor-pointer transition-opacity duration-150"
              style={{ opacity: hoverIdx === null || isHovered ? 1 : 0.6 }}
              onMouseEnter={() => setHoverIdx(i)}
            >
              {/* 隐藏的透明触发区域，方便鼠标交互 */}
              <rect
                x={xOf(p.t) - bw * 1.5}
                y={PT}
                width={bw * 3}
                height={ih}
                fill="transparent"
              />

              {/* 读入 Prompt 柱 */}
              {hP > 0 && (
                <rect
                  x={x.toFixed(1)}
                  y={(yBase - hP).toFixed(1)}
                  width={bw.toFixed(1)}
                  height={hP.toFixed(1)}
                  fill="url(#tokGradPrompt)"
                  rx={hC > 0 ? 0 : 2}
                />
              )}

              {/* 产出 Completion 柱 */}
              {hC > 0 && (
                <rect
                  x={x.toFixed(1)}
                  y={(yBase - hP - hC).toFixed(1)}
                  width={bw.toFixed(1)}
                  height={hC.toFixed(1)}
                  fill="url(#tokGradCompletion)"
                  rx={2}
                />
              )}
            </g>
          );
        })}

        {/* X 轴底部基线 */}
        <line x1={PL} y1={yBase} x2={W - PR} y2={yBase} stroke="var(--line)" />

        {/* X 轴时间刻度文字（固定 10.5px，绝不随屏宽放大） */}
        {ticks.map((t, i) => (
          <text
            key={i}
            x={t.x.toFixed(1)}
            y={yBase + 15}
            textAnchor={t.anchor}
            className="fill-[var(--ink-3)] text-[10.5px] tabular"
          >
            {t.text}
          </text>
        ))}
      </svg>

      {/* 现代悬浮 Tooltip 气泡 */}
      {activePt && (
        <div
          className="pointer-events-none absolute top-1 z-30 -translate-x-1/2 rounded-lg border border-[var(--line)] bg-[var(--surface)]/95 px-3 py-2 text-[11.5px] shadow-lg backdrop-blur"
          style={{
            left: `${Math.max(100, Math.min(W - 100, activeX))}px`,
          }}
        >
          <div className="mb-1 border-b border-[var(--line-soft)] pb-1 font-medium text-[var(--ink)]">
            {formatFullTime(activePt)}
          </div>
          <div className="flex items-center justify-between gap-3 text-[12px] font-semibold text-[var(--ink)]">
            <span>总计 Token</span>
            <span className="tabular text-[var(--accent)]" title={`${fmtNum(activePt.tt)} tokens`}>
              {fmtTok(activePt.tt)}
            </span>
          </div>
          <div className="mt-1 flex flex-col gap-0.5 text-[11px] text-[var(--ink-2)]">
            <div className="flex items-center justify-between gap-3">
              <span className="inline-flex items-center gap-1.5">
                <i className="h-1.5 w-1.5 rounded-full bg-[var(--accent)]" />
                读 (Prompt)
              </span>
              <span className="tabular font-medium text-[var(--ink)]" title={`${fmtNum(activePt.pt)} tokens`}>
                {fmtTok(activePt.pt)}{' '}
                <span className="text-[10px] text-[var(--ink-3)]">
                  ({activePt.tt ? Math.round((activePt.pt / activePt.tt) * 100) : 0}%)
                </span>
              </span>
            </div>
            <div className="flex items-center justify-between gap-3">
              <span className="inline-flex items-center gap-1.5">
                <i className="h-1.5 w-1.5 rounded-full bg-[var(--ok)]" />
                取 (Completion)
              </span>
              <span className="tabular font-medium text-[var(--ink)]" title={`${fmtNum(activePt.ct)} tokens`}>
                {fmtTok(activePt.ct)}{' '}
                <span className="text-[10px] text-[var(--ink-3)]">
                  ({activePt.tt ? Math.round((activePt.ct / activePt.tt) * 100) : 0}%)
                </span>
              </span>
            </div>
            {activePt.req > 0 && (
              <div className="flex items-center justify-between gap-3 text-[var(--ink-3)]">
                <span>请求次数</span>
                <span className="tabular" title={`${fmtNum(activePt.req)} 次`}>
                  {fmtTok(activePt.req)} 次
                </span>
              </div>
            )}
          </div>
        </div>
      )}
    </div>
  );
}

/** 后端 series 点 → ChartPoint（丢弃无效点，防御 NaN）。 */
export function toChartPoints(
  series: { t: string; scope?: string; prompt_tokens?: number; completion_tokens?: number; total_tokens?: number; requests?: number }[],
): ChartPoint[] {
  const out: ChartPoint[] = [];
  for (const p of series || []) {
    const s = String(p.t || '');
    const iso = s.length === 13 ? s + ':00:00' : s.includes('T') || s.includes('-') ? s + (s.length === 10 ? 'T00:00:00' : '') : s;
    const t = Date.parse(iso);
    if (!Number.isFinite(t)) continue;
    const pt = Number(p.prompt_tokens || 0);
    const ct = Number(p.completion_tokens || 0);
    out.push({ t, raw: s, pt, ct, tt: Number(p.total_tokens || 0) || pt + ct, req: p.requests || 0, scope: p.scope });
  }
  return out;
}
