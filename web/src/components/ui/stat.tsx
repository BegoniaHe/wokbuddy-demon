import type { ReactNode } from 'react';
import { cn } from '@/lib/utils';

// Stats：顶部统计条。窄屏降为 3 / 2 列。
export function Stats({ children, className }: { children: ReactNode; className?: string }) {
  return (
    <div
      className={cn(
        'mb-5 grid grid-cols-2 overflow-hidden rounded-surface border border-line bg-surface shadow-panel sm:grid-cols-3 lg:grid-cols-7',
        className,
      )}
    >
      {children}
    </div>
  );
}

export function Stat({
  value,
  label,
  tone = 'default',
}: {
  value: ReactNode;
  label: ReactNode;
  tone?: 'default' | 'good' | 'warn' | 'bad';
}) {
  return (
    <div className="border-b border-r border-line-soft px-4 py-3.5">
      <div
        className={cn(
          'whitespace-nowrap font-mono text-[26px] font-semibold leading-tight tracking-tight tabular-nums',
          tone === 'good' && 'text-ok',
          tone === 'warn' && 'text-warn',
          tone === 'bad' && 'text-bad',
        )}
      >
        {value}
      </div>
      <div className="mt-1.5 text-2xs text-ink-3">{label}</div>
    </div>
  );
}

// Kpis：用量页 KPI 卡容器（默认 6 列，k4 变体 4 列）。
export function Kpis({
  children,
  cols = 6,
  className,
}: {
  children: ReactNode;
  cols?: 4 | 6;
  className?: string;
}) {
  return (
    <div
      className={cn(
        'grid gap-2.5 px-4 pt-3.5',
        cols === 4
          ? 'grid-cols-2 lg:grid-cols-4'
          : 'grid-cols-2 sm:grid-cols-3 xl:grid-cols-6',
        className,
      )}
    >
      {children}
    </div>
  );
}

export type KpiTone = 'accent' | 'ok' | 'warn' | 'bad' | 'mute' | 'soft';

const kpiRail: Record<KpiTone, string> = {
  accent: 'before:bg-accent',
  ok: 'before:bg-ok',
  warn: 'before:bg-warn',
  bad: 'before:bg-bad',
  mute: 'before:bg-line',
  soft: 'before:bg-[color-mix(in_srgb,var(--accent)_45%,var(--line))]',
};

const kpiValue: Record<KpiTone, string> = {
  accent: 'text-accent',
  ok: 'text-ok',
  warn: 'text-warn',
  bad: 'text-bad',
  mute: '',
  soft: '',
};

export function Kpi({
  value,
  label,
  sub,
  tone = 'mute',
  children,
}: {
  value: ReactNode;
  label: ReactNode;
  sub?: ReactNode;
  tone?: KpiTone;
  children?: ReactNode;
}) {
  return (
    <div
      className={cn(
        'relative min-w-0 overflow-hidden rounded-inner border border-line-soft bg-surface-2 py-2.5 pl-4 pr-3.5',
        'before:absolute before:inset-y-0 before:left-0 before:w-[3px] before:opacity-90 before:content-[""]',
        kpiRail[tone],
      )}
    >
      <div className="truncate text-2xs text-ink-3">{label}</div>
      <div
        className={cn(
          'mt-1 whitespace-nowrap font-mono text-[23px] font-semibold leading-tight tracking-tight tabular-nums',
          kpiValue[tone],
        )}
      >
        {value}
      </div>
      {sub ? <div className="mt-1 truncate text-2xs text-ink-3">{sub}</div> : null}
      {children}
    </div>
  );
}
