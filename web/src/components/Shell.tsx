import { useQuery } from '@tanstack/react-query';
import {
  BarChart3,
  Bot,
  Boxes,
  ChefHat,
  ChevronDown,
  KeyRound,
  MessageSquare,
  MessagesSquare,
  Moon,
  Package,
  RefreshCw,
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
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuGroup,
  DropdownMenuItem,
  DropdownMenuLabel,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from '@/components/ui/dropdown-menu';
import { cn } from '@/lib/utils';
import { Dot } from '@/components/ui-kit';

const NAV: { id: ViewId; icon: LucideIcon }[] = [
  { id: 'accounts', icon: Users },
  { id: 'usage', icon: BarChart3 },
  { id: 'packages', icon: Package },
  { id: 'taskscenter', icon: MessageSquare },
];

const TOOLS: { id: ViewId; icon: LucideIcon }[] = [
  { id: 'chat', icon: MessagesSquare },
  { id: 'models', icon: Boxes },
  { id: 'keys', icon: KeyRound },
  { id: 'agents', icon: Bot },
];

const SYSTEM: { id: ViewId; icon: LucideIcon }[] = [
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
  const statusDetail = total ? healthy + ' / ' + total + ' 个账号可用' : '尚未添加账号';

  const renderNav = (items: { id: ViewId; icon: LucideIcon }[]) =>
    items.map(({ id, icon: Icon }) => {
      const on = id === view;
      return (
        <li key={id} className="shrink-0 md:w-full">
          <a
            href={'#' + id}
            aria-current={on ? 'page' : undefined}
            onClick={(e) => {
              e.preventDefault();
              onNavigate(id);
            }}
            className={cn(
              'group relative flex items-center gap-2.5 rounded-lg px-2.5 py-[7px] text-[13px] no-underline transition-colors',
              on
                ? 'bg-sidebar-active font-medium text-foreground'
                : 'text-muted-foreground hover:bg-secondary/70 hover:text-foreground',
            )}
          >
            {on ? <span className="absolute inset-y-1.5 left-0 w-[2px] rounded-full bg-primary" /> : null}
            <Icon
              className={cn(
                'size-[15px] flex-none transition-colors',
                on ? 'text-primary' : 'text-ink-3 group-hover:text-muted-foreground',
              )}
            />
            <span className="whitespace-nowrap">{VIEW_TITLES[id]}</span>
          </a>
        </li>
      );
    });

  return (
    <div className="flex min-h-screen flex-col md:flex-row">
      <nav className="flex flex-col border-b border-border bg-sidebar md:sticky md:top-0 md:h-screen md:w-[232px] md:flex-none md:border-b-0 md:border-r">
        <div className="flex h-14 items-center gap-2.5 px-4">
          <span className="grid size-7 flex-none place-items-center rounded-lg bg-brand-soft text-brand">
            <ChefHat className="size-4" strokeWidth={2.2} />
          </span>
          <div className="min-w-0">
            <div className="truncate text-[13px] font-semibold tracking-tight">WokBuddy Demon</div>
            <div className="truncate font-mono text-3xs text-muted-foreground">v{data?.version ?? '-'}</div>
          </div>
        </div>

        <div className="hidden md:flex md:flex-1 md:flex-col md:items-stretch md:gap-5 md:overflow-y-auto md:px-2.5 md:pb-4 md:pt-2">
          <div>
            <div className="mb-1.5 hidden px-2.5 text-3xs font-medium tracking-[0.08em] text-ink-3 uppercase md:block">
              概览
            </div>
            <ul className="flex gap-0.5 md:flex-col">{renderNav(NAV)}</ul>
          </div>
          <div>
            <div className="mb-1.5 hidden px-2.5 text-3xs font-medium tracking-[0.08em] text-ink-3 uppercase md:block">
              工具
            </div>
            <ul className="flex gap-0.5 md:flex-col">{renderNav(TOOLS)}</ul>
          </div>
          <div>
            <div className="mb-1.5 hidden px-2.5 text-3xs font-medium tracking-[0.08em] text-ink-3 uppercase md:block">
              系统
            </div>
            <ul className="flex gap-0.5 md:flex-col">{renderNav(SYSTEM)}</ul>
          </div>
        </div>

        <div className="hidden border-t border-border/80 px-4 py-3.5 md:block">
          <div className="rounded-lg border border-border/80 bg-card/70 px-3 py-2.5">
            <div className="flex items-center gap-2 text-xs font-medium">
              <Dot tone={statusTone} />
              <span>{stateText}</span>
            </div>
            <div className="mt-1 text-2xs text-muted-foreground">{statusDetail}</div>
            <div className="mt-1 font-mono text-3xs text-ink-3">
              {data?.redis_mode === 'upstash' ? 'Redis 镜像' : '本地内存'} · {uptimeText(data?.uptime_sec ?? 0)}
            </div>
          </div>
        </div>
      </nav>

      <div className="flex min-w-0 flex-1 flex-col">
        <header className="sticky top-0 z-20 flex h-14 items-center gap-3 border-b border-border bg-background/85 px-4 backdrop-blur-md md:px-6">
          <div className="flex min-w-0 flex-col">
            <h1 className="sr-only md:not-sr-only md:truncate md:text-sm md:font-semibold md:tracking-tight">{VIEW_TITLES[view]}</h1>
            <DropdownMenu>
              <DropdownMenuTrigger
                render={<Button variant="ghost" className="h-auto max-w-[190px] justify-start gap-1.5 px-1.5 py-1 md:hidden" />}
              >
                <span className="flex min-w-0 flex-col items-start">
                  <span className="flex items-center gap-1 text-sm font-semibold">
                    <span className="truncate">{VIEW_TITLES[view]}</span>
                    <ChevronDown className="size-3.5 text-muted-foreground" />
                  </span>
                  <span className="sr-only">切换页面</span>
                  <span className="truncate font-mono text-3xs font-normal text-muted-foreground">
                    {data ? uptimeText(data.uptime_sec) : '—'}
                  </span>
                </span>
              </DropdownMenuTrigger>
              <DropdownMenuContent align="start" className="w-52">
                {[
                  { label: '概览', items: NAV },
                  { label: '工具', items: TOOLS },
                  { label: '系统', items: SYSTEM },
                ].map((group, index) => (
                  <DropdownMenuGroup key={group.label}>
                    {index ? <DropdownMenuSeparator /> : null}
                    <DropdownMenuLabel>{group.label}</DropdownMenuLabel>
                    {group.items.map(({ id, icon: Icon }) => (
                      <DropdownMenuItem
                        key={id}
                        aria-current={id === view ? 'page' : undefined}
                        onClick={() => onNavigate(id)}
                        className={cn(id === view && 'bg-accent font-medium text-accent-foreground')}
                      >
                        <Icon />
                        {VIEW_TITLES[id]}
                      </DropdownMenuItem>
                    ))}
                  </DropdownMenuGroup>
                ))}
              </DropdownMenuContent>
            </DropdownMenu>
            <span className="hidden truncate font-mono text-3xs text-muted-foreground md:block">
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
          <Button
            variant="outline"
            disabled={refreshing}
            onClick={onRefresh}
            title="从上游刷新全部账号余额"
            aria-label={refreshing ? '刷新中' : '刷新余额'}
          >
            <RefreshCw className={cn('size-3.5', refreshing && 'animate-spin')} />
            <span className="hidden sm:inline">{refreshing ? '刷新中…' : '刷新'}</span>
          </Button>
          {view === 'accounts' ? (
            <Button onClick={onAdd} aria-label="添加账号">
              <Users className="size-3.5" />
              <span className="hidden sm:inline">添加账号</span>
            </Button>
          ) : null}
        </header>

        <main className="mx-auto w-full max-w-[1480px] flex-1 px-4 pb-16 pt-5 md:px-7 md:pt-6">{children}</main>
      </div>
    </div>
  );
}
