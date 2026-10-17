import { useQuery } from '@tanstack/react-query';
import {
  BarChart3,
  Boxes,
  KeyRound,
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
import { Dot } from '@/components/ui-kit';

const NAV: { id: ViewId; icon: LucideIcon }[] = [
  { id: 'accounts', icon: Users },
  { id: 'usage', icon: BarChart3 },
  { id: 'packages', icon: Package },
  { id: 'taskscenter', icon: MessageSquare },
  { id: 'models', icon: Boxes },
  { id: 'keys', icon: KeyRound },
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
  const statusTone = healthy > 0 ? 'ok' : total ? 'warn' : 'bad';

  return (
    <div className="flex min-h-screen flex-col md:flex-row">
      <nav className="flex flex-col border-b border-border bg-card md:sticky md:top-0 md:h-screen md:w-[224px] md:flex-none md:border-b-0 md:border-r">
        <div className="flex items-center gap-2 px-4 py-3.5">
          <div className="text-[15px] font-semibold tracking-tight">WokBuddy Demon</div>
          <span className="font-mono text-2xs text-muted-foreground">v{data?.version ?? '-'}</span>
        </div>
        <ul className="flex flex-1 gap-0.5 overflow-x-auto p-2 md:flex-col md:overflow-visible">
          {NAV.map(({ id, icon: Icon }) => {
            const on = id === view;
            return (
              <li key={id} className="shrink-0 md:w-full">
                <a
                  href={'#' + id}
                  onClick={(e) => {
                    e.preventDefault();
                    onNavigate(id);
                  }}
                  className={cn(
                    'flex items-center gap-2.5 rounded-md px-2.5 py-2 text-sm no-underline transition-colors',
                    on
                      ? 'bg-accent-soft font-medium text-accent-foreground'
                      : 'text-muted-foreground hover:bg-secondary hover:text-foreground',
                  )}
                >
                  <Icon className="size-[15px] flex-none opacity-85" />
                  <span className="whitespace-nowrap">{VIEW_TITLES[id]}</span>
                </a>
              </li>
            );
          })}
        </ul>
        <div className="hidden border-t border-border px-4 py-3 text-2xs text-muted-foreground md:block">
          <div className="flex items-center gap-2">
            <Dot tone={statusTone} />
            <span>{stateText}</span>
          </div>
          <div className="mt-1.5">
            v{data?.version ?? '-'} · {data?.redis_mode === 'upstash' ? 'Redis 镜像' : '本地内存'}
          </div>
        </div>
      </nav>

      <div className="flex min-w-0 flex-1 flex-col">
        <header className="sticky top-0 z-20 flex h-14 items-center gap-3 border-b border-border bg-card px-4 md:px-6">
          <div className="flex min-w-0 flex-col">
            <h2 className="truncate text-[15px] font-semibold tracking-tight">{VIEW_TITLES[view]}</h2>
            <span className="truncate font-mono text-2xs text-muted-foreground">
              {data ? uptimeText(data.uptime_sec) : '—'}
            </span>
          </div>
          <span className="flex-1" />
          <Button
            variant="ghost"
            size="icon"
            title={theme === 'light' ? '切换到深色' : '切换到浅色'}
            aria-label="切换主题"
            onClick={onToggleTheme}
          >
            {theme === 'light' ? <Sun /> : <Moon />}
          </Button>
          <Button variant="outline" disabled={refreshing} onClick={onRefresh}>
            {refreshing ? '刷新中…' : '刷新'}
          </Button>
          <Button onClick={onAdd}>添加账号</Button>
        </header>

        <main className="mx-auto w-full max-w-[1560px] flex-1 px-4 pb-16 pt-6 md:px-6">{children}</main>
      </div>
    </div>
  );
}
