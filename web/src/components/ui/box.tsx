import type { ReactNode } from 'react';
import { cn } from '@/lib/utils';

// Box：面板块。
export function Box({ children, className }: { children: ReactNode; className?: string }) {
  return (
    <section
      className={cn(
        'mb-5 overflow-hidden rounded-xl border border-line bg-surface shadow-panel',
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
    <h3 className={cn('whitespace-nowrap text-[13.5px] font-semibold', className)}>{children}</h3>
  );
}

export function BoxHint({ children }: { children: ReactNode }) {
  return <span className="ml-1.5 text-[11.5px] font-normal text-ink-3">{children}</span>;
}

export function BoxNote({ children, className, title }: { children: ReactNode; className?: string; title?: string }) {
  return (
    <span className={cn('truncate text-xs text-ink-3', className)} title={title}>
      {children}
    </span>
  );
}

export function Grow({ className }: { className?: string }) {
  return <span className={cn('flex-1', className)} />;
}
