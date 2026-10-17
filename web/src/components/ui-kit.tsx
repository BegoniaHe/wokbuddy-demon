import type { ReactNode } from 'react';
import { cva, type VariantProps } from 'class-variance-authority';
import { cn } from '@/lib/utils';

// 语义色集合：ok/warn/bad 对应状态，accent 对应强调，mute 对应弱化。
export type Tone = 'ok' | 'warn' | 'bad' | 'accent' | 'mute';

// Dot：状态圆点。
export function Dot({ tone = 'mute', className }: { tone?: Tone; className?: string }) {
  return <span className={cn('inline-block h-[7px] w-[7px] flex-none rounded-full', dotBg[tone], className)} />;
}

const dotBg: Record<Tone, string> = {
  ok: 'bg-success',
  warn: 'bg-warning',
  bad: 'bg-destructive',
  accent: 'bg-primary',
  mute: 'bg-muted-foreground',
};

const tagVariants = cva(
  'inline-flex items-center gap-1.5 rounded-md px-2 py-px text-2xs font-medium whitespace-nowrap',
  {
    variants: {
      tone: {
        ok: 'bg-success-soft text-success',
        warn: 'bg-warning-soft text-warning',
        bad: 'bg-destructive-soft text-destructive',
        accent: 'bg-accent-soft text-accent-foreground',
        mute: 'bg-secondary text-muted-foreground',
      },
    },
    defaultVariants: { tone: 'mute' },
  },
);

export interface TagProps extends VariantProps<typeof tagVariants> {
  children: ReactNode;
  className?: string;
  title?: string;
}

// Tag：状态标签（软底 + 语义色）。
export function Tag({ tone, children, className, title }: TagProps) {
  return (
    <span className={cn(tagVariants({ tone }), className)} title={title}>
      {children}
    </span>
  );
}

const chipVariants = cva(
  'inline-flex items-baseline rounded-md px-1.5 py-0.5 font-mono text-xs font-semibold tabular-nums',
  {
    variants: {
      tone: {
        ok: 'bg-success-soft text-success',
        warn: 'bg-warning-soft text-warning',
        bad: 'bg-destructive-soft text-destructive',
        accent: 'bg-accent-soft text-accent-foreground',
        mute: 'bg-secondary text-muted-foreground',
      },
    },
    defaultVariants: { tone: 'mute' },
  },
);

export interface ChipProps extends VariantProps<typeof chipVariants> {
  children: ReactNode;
  unit?: ReactNode;
  className?: string;
  title?: string;
}

// Chip：等宽数值软徽标（次 / tok / 延迟 …），可选单位后缀。
export function Chip({ tone, children, unit, className, title }: ChipProps) {
  return (
    <span className={cn(chipVariants({ tone }), className)} title={title}>
      {children}
      {unit ? <em className="ml-0.5 font-sans text-3xs not-italic">{unit}</em> : null}
    </span>
  );
}

// Bar：细进度条（轨道 + 单色填充）。
export function Bar({ pct, tone = 'accent', className, title }: { pct: number; tone?: Tone; className?: string; title?: string }) {
  return (
    <span className={cn('block h-[3px] overflow-hidden rounded-full bg-border', className)} title={title}>
      <i
        className={cn('block h-full rounded-full transition-[width] duration-300', dotBg[tone])}
        style={{ width: Math.max(0, Math.min(100, pct)) + '%' }}
      />
    </span>
  );
}

const clampPct = (v: number) => Math.max(0, Math.min(100, v));

// MixBar：prompt / completion 双色占比条。
export function MixBar({ prompt, completion, total, className, title }: { prompt?: number; completion?: number; total?: number; className?: string; title?: string }) {
  const t = Number(total || 0);
  if (!t) return null;
  const pp = clampPct((Number(prompt || 0) / t) * 100);
  const pc = clampPct((Number(completion || 0) / t) * 100);
  return (
    <span
      className={cn('flex h-[3px] overflow-hidden rounded-full bg-border', className)}
      title={title ?? 'prompt ' + pp.toFixed(1) + '% · completion ' + pc.toFixed(1) + '%'}
    >
      <i className="block h-full bg-primary" style={{ width: pp.toFixed(2) + '%' }} />
      <i className="block h-full bg-success" style={{ width: pc.toFixed(2) + '%' }} />
    </span>
  );
}

// Legend：图例项（色块 + 文案，超长截断）。
export function Legend({ color, children }: { color: string; children: ReactNode }) {
  return (
    <span className="inline-flex max-w-full items-center gap-1.5">
      <i className="inline-block h-2.5 w-2.5 flex-none rounded-[3px]" style={{ background: color }} />
      <span className="truncate">{children}</span>
    </span>
  );
}
