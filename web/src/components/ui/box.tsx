import type { ReactNode } from 'react';
import { cn } from '@/lib/utils';

// Box：面板块。
export function Box({ children, className }: { children: ReactNode; className?: string }) {
  return (
    <section
      className={cn(
        'mb-5 overflow-hidden rounded-surface border border-line bg-surface shadow-panel',
        className,
      )}
    >
      {children}
    </section>
  );
}

// BoxHeader：面板块头部。
export function BoxHeader({ children, className }: { children: ReactNode; className?: string }) {
  return (
    <header
      className={cn(
        'flex flex-wrap items-center gap-3 border-b border-line-soft px-4 py-3',
        className,
      )}
    >
      {children}
    </header>
  );
}

export function BoxTitle({ children, className }: { children: ReactNode; className?: string }) {
  return (
    <h3 className={cn('whitespace-nowrap text-sm font-semibold', className)}>{children}</h3>
  );
}

export function BoxHint({ children }: { children: ReactNode }) {
  return <span className="ml-1.5 text-2xs font-normal text-ink-3">{children}</span>;
}

export function BoxNote({ children, className, title }: { children: ReactNode; className?: string; title?: string }) {
  return (
    <span className={cn('truncate text-xs text-ink-3', className)} title={title}>
      {children}
    </span>
  );
}

// BoxBody：面板块正文（统一内边距）。
export function BoxBody({ children, className }: { children: ReactNode; className?: string }) {
  return <div className={cn('p-4', className)}>{children}</div>;
}

// Toolbar：面板内的筛选 / 操作条（贴表头下方的次级背景）。
export function Toolbar({ children, className }: { children: ReactNode; className?: string }) {
  return (
    <div
      className={cn(
        'flex flex-wrap items-center gap-2 border-b border-line-soft bg-surface-2 px-4 py-2.5',
        className,
      )}
    >
      {children}
    </div>
  );
}

export function Grow({ className }: { className?: string }) {
  return <span className={cn('flex-1', className)} />;
}

const dotTone = {
  accent: 'bg-accent',
  ok: 'bg-ok',
  warn: 'bg-warn',
  bad: 'bg-bad',
  mute: 'bg-ink-3',
} as const;

export type DotTone = keyof typeof dotTone;

// Dot：状态圆点。
export function Dot({ tone = 'mute', className }: { tone?: DotTone; className?: string }) {
  return <span className={cn('inline-block h-[7px] w-[7px] flex-none rounded-full', dotTone[tone], className)} />;
}
