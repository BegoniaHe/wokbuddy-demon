import type { ReactNode } from 'react';
import { cn } from '@/lib/utils';

export interface TabItem {
  value: string;
  label: ReactNode;
  count?: number;
  title?: string;
}

// Tabs：分段控件（切换维度 / 渠道 / 登录方式），选中项浮起。
export function Tabs({
  items,
  active,
  onSelect,
  className,
}: {
  items: TabItem[];
  active: string;
  onSelect: (v: string) => void;
  className?: string;
}) {
  return (
    <span className={cn('inline-flex gap-0.5 rounded-control border border-line bg-surface-2 p-0.5', className)}>
      {items.map((it) => (
        <button
          key={it.value}
          type="button"
          title={it.title}
          onClick={() => onSelect(it.value)}
          className={cn(
            'rounded-control px-3 py-1 text-xs font-medium transition-colors',
            it.value === active
              ? 'border border-line bg-surface text-ink'
              : 'border border-transparent text-ink-3 hover:text-ink',
          )}
        >
          {it.label}
          {it.count != null ? (
            <span className="ml-1 font-normal tabular-nums text-ink-3">{it.count}</span>
          ) : null}
        </button>
      ))}
    </span>
  );
}
