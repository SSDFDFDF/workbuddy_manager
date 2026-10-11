/* 数值与时间格式化（全站唯一实现，等宽 tabular 数字呈现）。 */

export function trimFixed(s: string): string {
  return s.includes('.') ? s.replace(/0+$/, '').replace(/\.$/, '') : s;
}

/** 千分位精确数字："12,345"。 */
export function fmtNum(n: number | undefined | null): string {
  const v = Number(n || 0);
  return Number.isFinite(v) ? v.toLocaleString('zh-CN') : '—';
}

export function fmtTok(n: number | undefined | null): string {
  const v = Number(n || 0);
  if (v >= 1e9) return trimFixed((v / 1e9).toFixed(2)) + 'B';
  if (v >= 1e6) return trimFixed((v / 1e6).toFixed(2)) + 'M';
  if (v >= 1e3) return trimFixed((v / 1e3).toFixed(2)) + 'K';
  return String(Math.round(v));
}

export function fmtK(n: number | undefined | null): string {
  const v = Number(n || 0);
  return v >= 1000 ? Math.round(v / 1000) + 'K' : String(v);
}

export function fmtMs(ms: number | undefined | null): string {
  const v = Number(ms || 0);
  if (!Number.isFinite(v) || v <= 0) return '—';
  if (v >= 1000) return trimFixed((v / 1000).toFixed(2)) + 's';
  return Math.round(v) + 'ms';
}

export function fmtRate(r: number | undefined | null): string {
  const n = Number(r);
  return r && Number.isFinite(n) ? n.toFixed(1) + ' tok/s' : '—';
}

export function fmtCredit(n: number | undefined | null): string {
  const v = Number(n || 0);
  return Number.isFinite(v) ? trimFixed(v.toFixed(2)) : '—';
}

export function fmtBytes(bytes: number | undefined | null): string {
  const n = Number(bytes || 0);
  if (n < 1024) return n + ' B';
  if (n < 1024 * 1024) return (n / 1024).toFixed(1) + ' KB';
  return (n / 1024 / 1024).toFixed(1) + ' MB';
}

/** 相对时间："3 分钟前"。无效/零值返回 "—"。 */
export function ago(iso?: string): string {
  if (!iso || String(iso).startsWith('0001-')) return '—';
  const s = (Date.now() - new Date(iso).getTime()) / 1000;
  if (!Number.isFinite(s)) return '—';
  if (s < 0) return '刚刚';
  if (s < 60) return Math.floor(s) + ' 秒前';
  if (s < 3600) return Math.floor(s / 60) + ' 分钟前';
  if (s < 86400) return Math.floor(s / 3600) + ' 小时前';
  return Math.floor(s / 86400) + ' 天前';
}

/** 秒数 → "1时05分" / "3分20秒" / "45秒"。 */
export function dur(sec: number): string {
  sec = Math.max(0, Math.round(Number(sec) || 0));
  const h = Math.floor(sec / 3600);
  const m = Math.floor((sec % 3600) / 60);
  const s = sec % 60;
  const p = (n: number) => String(n).padStart(2, '0');
  if (h) return `${h}时${p(m)}分`;
  if (m) return `${m}分${p(s)}秒`;
  return s + '秒';
}

/** 运行时长（秒）→ "2 天 5 时 13 分"。 */
export function fmtUptime(sec: number): string {
  const t = Math.floor(Number(sec) || 0);
  const days = Math.floor(t / 86400);
  const h = Math.floor((t % 86400) / 3600);
  const m = Math.floor((t % 3600) / 60);
  return (days ? days + ' 天 ' : '') + h + ' 时 ' + m + ' 分';
}

/** ISO/RFC3339 → 毫秒时间戳；零值/无效返回 0。 */
export function parseAPITime(v?: string): number {
  const text = String(v || '');
  if (!text || text.startsWith('0001-')) return 0;
  const ms = Date.parse(text);
  return Number.isFinite(ms) ? ms : 0;
}

/** 毫秒 → "2026-01-02 13:45"（本地时区）。 */
export function fmtLocalDateTime(ms: number): string {
  const d = new Date(ms);
  const p = (n: number) => String(n).padStart(2, '0');
  return (
    d.getFullYear() + '-' + p(d.getMonth() + 1) + '-' + p(d.getDate()) + ' ' + p(d.getHours()) + ':' + p(d.getMinutes())
  );
}

export function fmtTimeHM(v?: string): string {
  if (!v) return '—';
  const d = new Date(v);
  return Number.isFinite(d.getTime()) ? d.toLocaleTimeString('zh-CN', { hour12: false }) : '—';
}

/** 缓存命中率百分比（0-100）；无样本返回 null。 */
export function cacheRatePct(hit?: number, miss?: number): number | null {
  const h = Number(hit || 0);
  const m = Number(miss || 0);
  const total = h + m;
  return total ? (h / total) * 100 : null;
}

export function cacheRateText(hit?: number, miss?: number): string {
  const pct = cacheRatePct(hit, miss);
  return pct == null ? '—' : String(Math.round(pct * 10) / 10) + '%';
}
