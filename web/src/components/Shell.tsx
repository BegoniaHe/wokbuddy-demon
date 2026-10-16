import { useQuery } from '@tanstack/react-query';
import {
  BarChart3,
  Boxes,
  MessageSquare,
  Moon,
  Package,
  ScrollText,
  Settings,
  Sun,
  Users,
  type LucideIcon,
} from 'lucide-react';
import { api } from '@/lib/api';
import type { Overview } from '@/lib/types';
import { VIEW_TITLES, type ViewId } from '@/views/registry';
import { Button } from '@/components/ui/button';
import { cn } from '@/lib/utils';

const NAV: { id: ViewId; icon: LucideIcon }[] = [
  { id: 'accounts', icon: Users },
  { id: 'usage', icon: BarChart3 },
  { id: 'packages', icon: Package },
  { id: 'taskscenter', icon: MessageSquare },
  { id: 'models', icon: Boxes },
  { id: 'config', icon: Settings },
  { id: 'logs', icon: ScrollText },
];

function uptimeText(sec: number): string {
  const up = Math.floor(sec);
  return (
    '运行 ' +
    (up >= 86400 ? Math.floor(up / 86400) + ' 天 ' : '') +
    Math.floor((up % 86400) / 3600) +
    ' 时 ' +
    Math.floor((up % 3600) / 60) +
    ' 分'
  );
}

export function Shell({
  view,
  onNavigate,
  theme,
  onToggleTheme,
  onAdd,
  onRefresh,
  refreshing,
  children,
}: {
  view: ViewId;
  onNavigate: (v: ViewId) => void;
  theme: 'light' | 'dark';
  onToggleTheme: () => void;
  onAdd: () => void;
  onRefresh: () => void;
  refreshing: boolean;
  children: React.ReactNode;
}) {
  // 侧栏状态与账号池共用 ['overview'] 缓存；此观察者不设轮询（由账号池视图驱动）。
  const { data } = useQuery({ queryKey: ['overview'], queryFn: () => api<Overview>('overview') });

  const healthy = data?.healthy ?? 0;
  const total = data?.total ?? 0;
  const stateText = healthy > 0 ? '服务正常' : total ? '无可用账号' : '待添加账号';
  const pulseCls = healthy > 0 ? 'bg-ok' : total ? 'bg-warn' : 'bg-bad';

  return (
    <div className="grid min-h-screen grid-cols-1 md:grid-cols-[208px_1fr]">
      <nav className="flex flex-col border-b border-line bg-surface md:sticky md:top-0 md:h-screen md:border-b-0 md:border-r">
        <div className="flex items-center gap-3 border-b border-line-soft px-[18px] py-4 md:block">
          <div className="text-[15px] font-semibold tracking-tight">WokBuddy Demon</div>
          <div className="mt-0.5 font-mono text-[11.5px] text-ink-3">
            v{data?.version ?? '-'}
          </div>
        </div>
        <ul className="flex flex-1 gap-0.5 overflow-x-auto p-2 md:flex-col md:overflow-visible">
          {NAV.map(({ id, icon: Icon }) => {
            const on = id === view;
            return (
              <li key={id} className="shrink-0">
                <a
                  href={'#' + id}
                  onClick={(e) => {
                    e.preventDefault();
                    onNavigate(id);
                  }}
                  className={cn(
                    'flex items-center gap-2.5 rounded-[7px] px-2.5 py-2 text-[13.5px] no-underline',
                    on
                      ? 'bg-accent-soft font-medium text-accent'
                      : 'text-ink-2 hover:bg-surface-2 hover:text-ink',
                  )}
                >
                  <Icon className="h-[15px] w-[15px] flex-none opacity-85" />
                  <span className="whitespace-nowrap">{VIEW_TITLES[id]}</span>
                </a>
              </li>
            );
          })}
        </ul>
        <div className="hidden border-t border-line-soft px-4 py-3 text-[11.5px] text-ink-3 md:block">
          <div className="flex items-center gap-2">
            <span className={cn('h-[7px] w-[7px] flex-none rounded-full', pulseCls)} />
            <span>{stateText}</span>
          </div>
          <div className="mt-1.5">
            v{data?.version ?? '-'} · {data?.redis_mode === 'upstash' ? 'Redis 镜像' : '本地内存'}
          </div>
        </div>
      </nav>

      <div className="flex min-w-0 flex-col">
        <div className="sticky top-0 z-20 flex items-center gap-3.5 border-b border-line bg-surface px-4 py-3.5 md:px-6">
          <h2 className="text-[15.5px] font-semibold tracking-tight">{VIEW_TITLES[view]}</h2>
          <span className="text-ink-3">/</span>
          <span className="font-mono text-xs text-ink-3">
            {data ? uptimeText(data.uptime_sec) : '-'}
          </span>
          <span className="flex-1" />
          <Button
            variant="ghost"
            size="icon"
            title={theme === 'light' ? '切换到深色' : '切换到浅色'}
            aria-label="切换主题"
            onClick={onToggleTheme}
          >
            {theme === 'light' ? <Sun className="h-4 w-4" /> : <Moon className="h-4 w-4" />}
          </Button>
          <Button disabled={refreshing} onClick={onRefresh}>
            {refreshing ? '刷新中…' : '刷新'}
          </Button>
          <Button variant="primary" onClick={onAdd}>
            添加账号
          </Button>
        </div>

        <div className="mx-auto w-full max-w-[1560px] px-4 pb-16 pt-5 md:px-6">{children}</div>
      </div>
    </div>
  );
}
