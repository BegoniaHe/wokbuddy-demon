import type { ReactNode } from 'react';
import { cn } from '@/lib/utils';

const fillTone = {
  accent: 'bg-accent',
  ok: 'bg-ok',
  warn: 'bg-warn',
  bad: 'bg-bad',
  mute: 'bg-ink-3',
} as const;

export type BarTone = keyof typeof fillTone;

const clampPct = (v: number) => Math.max(0, Math.min(100, v));

// Bar：细进度条（轨道 + 单色填充）。
export function Bar({
  pct,
  tone = 'accent',
  className,
  title,
}: {
  pct: number;
  tone?: BarTone;
  className?: string;
  title?: string;
}) {
  return (
    <span className={cn('block h-[3px] overflow-hidden rounded-full bg-line', className)} title={title}>
      <i className={cn('block h-full rounded-full transition-[width] duration-300', fillTone[tone])} style={{ width: clampPct(pct) + '%' }} />
    </span>
  );
}

// MixBar：prompt / completion 双色占比条。
export function MixBar({
  prompt,
  completion,
  total,
  className,
  title,
}: {
  prompt?: number;
  completion?: number;
  total?: number;
  className?: string;
  title?: string;
}) {
  const t = Number(total || 0);
  if (!t) return null;
  const pp = clampPct((Number(prompt || 0) / t) * 100);
  const pc = clampPct((Number(completion || 0) / t) * 100);
  return (
    <span
      className={cn('flex h-[3px] overflow-hidden rounded-full bg-line', className)}
      title={title ?? 'prompt ' + pp.toFixed(1) + '% · completion ' + pc.toFixed(1) + '%'}
    >
      <i className="block h-full bg-accent" style={{ width: pp.toFixed(2) + '%' }} />
      <i className="block h-full bg-ok" style={{ width: pc.toFixed(2) + '%' }} />
    </span>
  );
}

// Legend：图例项（色块 + 文案）。
export function Legend({ color, children }: { color: string; children: ReactNode }) {
  return (
    <span className="inline-flex items-center gap-1.5">
      <i className="inline-block h-2.5 w-2.5 rounded-[3px]" style={{ background: color }} />
      {children}
    </span>
  );
}
