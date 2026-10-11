/* 全局数据上下文：overview 轮询 + 刷新 + 401 密钥门。
   轮询周期见 OVERVIEW_POLL_FAST_MS / OVERVIEW_POLL_SLOW_MS（按视图分档）；页面隐藏时暂停。
   视图级数据（用量/模型/…）由各视图自取。 */
import { createContext, useCallback, useContext, useEffect, useRef, useState } from 'react';
import type { Overview } from './types';
import { api, setUnauthorizedHandler } from './api';
import { toast } from './toast';

interface DataCtx {
  overview: Overview | null;
  refresh: (quiet?: boolean) => Promise<void>;
  hardRefresh: () => Promise<void>;
}

const Ctx = createContext<DataCtx>({ overview: null, refresh: async () => {}, hardRefresh: async () => {} });

/* overview 是「全账号快照 + 模型锁池」：服务端每次都要序列化整个账号池（含每号的冷却/
   在途/积分字段）与模型锁清单，而这两份数据的自然变化尺度是分钟级（冷却倒计时、在途数）。
   10s 已比人眼所需的刷新快，5s 纯属白烧 CPU 与带宽。

   轮询按视图分档：overview 的重度消费者只有「账号池」视图（账号卡片全靠它）；其余视图
   上它只喂侧边栏状态点 / 顶栏运行时长 / 版本号这几个分钟级慢变量，没必要同样 10s 去序列化
   整个账号池。因此账号池视图走 FAST，其余视图走 SLOW。视图由 hash 路由决定，这里自行监听
   hashchange，不从 AppShell 透传（避免耦合）。 */
const OVERVIEW_POLL_FAST_MS = 10000;
const OVERVIEW_POLL_SLOW_MS = 45000;

/* 账号池视图 = 空 hash 或 #accounts。其余视图一律走慢档。 */
function isAccountsView(): boolean {
  const h = (location.hash || '#accounts').slice(1);
  return h === '' || h === 'accounts';
}

export function useData() {
  return useContext(Ctx);
}

export function DataProvider({ children, onNeedKey }: { children: React.ReactNode; onNeedKey: () => void }) {
  const [overview, setOverview] = useState<Overview | null>(null);
  const inFlight = useRef(false);

  useEffect(() => {
    setUnauthorizedHandler(onNeedKey);
    return () => setUnauthorizedHandler(null);
  }, [onNeedKey]);

  const refresh = useCallback(async (quiet = false) => {
    if (inFlight.current) return;
    inFlight.current = true;
    try {
      const d = await api<Overview>('overview');
      setOverview(d);
    } catch (e) {
      if (!quiet) toast((e as Error).message, 'err');
    } finally {
      inFlight.current = false;
    }
  }, []);

  /* hardRefresh：全池余额刷新（打上游）+ 重拉 overview。 */
  const hardRefresh = useCallback(async () => {
    try {
      await api('balance_all', { method: 'POST' });
      await refresh(true);
      toast('余额已从上游刷新', 'ok');
    } catch (e) {
      toast('刷新失败：' + (e as Error).message, 'err');
      await refresh(true);
    }
  }, [refresh]);

  /* 当前轮询档位随视图切换：账号池 10s，其余 45s。监听 hashchange 重算。 */
  const [fastPoll, setFastPoll] = useState(isAccountsView);
  useEffect(() => {
    const onHash = () => setFastPoll(isAccountsView());
    addEventListener('hashchange', onHash);
    return () => removeEventListener('hashchange', onHash);
  }, []);

  /* 轮询：页面隐藏时暂停（切后台/最小化没人看）。档位变化时重建定时器。
     切回账号池视图时立刻 refresh 一次，避免等满一个慢周期才看到最新快照。 */
  useEffect(() => {
    void refresh();
    const period = fastPoll ? OVERVIEW_POLL_FAST_MS : OVERVIEW_POLL_SLOW_MS;
    const id = setInterval(() => {
      if (!document.hidden) void refresh(true);
    }, period);
    return () => clearInterval(id);
  }, [refresh, fastPoll]);

  return <Ctx.Provider value={{ overview, refresh, hardRefresh }}>{children}</Ctx.Provider>;
}
