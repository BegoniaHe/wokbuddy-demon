import {
  forwardRef,
  type InputHTMLAttributes,
  type ReactNode,
  type SelectHTMLAttributes,
} from 'react';
import { cva, type VariantProps } from 'class-variance-authority';
import { cn } from '@/lib/utils';

const controlBase =
  'rounded-control border border-line bg-surface-2 text-ink placeholder:text-ink-3 focus:outline-none focus:border-accent';

// 控件尺寸：default 整行宽（表单）；sm 内联紧凑（工具栏筛选）。
const controlSizes = {
  default: 'w-full px-2.5 py-[7px]',
  sm: 'w-auto px-2.5 py-[5px] text-xs',
} as const;

export interface InputProps extends Omit<InputHTMLAttributes<HTMLInputElement>, 'size'> {
  size?: keyof typeof controlSizes;
}

export const Input = forwardRef<HTMLInputElement, InputProps>(
  ({ className, size = 'default', ...props }, ref) => (
    <input ref={ref} className={cn(controlBase, controlSizes[size], className)} {...props} />
  ),
);
Input.displayName = 'Input';

export interface SelectProps extends Omit<SelectHTMLAttributes<HTMLSelectElement>, 'size'> {
  size?: keyof typeof controlSizes;
}

export const Select = forwardRef<HTMLSelectElement, SelectProps>(
  ({ className, size = 'default', ...props }, ref) => (
    <select
      ref={ref}
      className={cn(controlBase, controlSizes[size], 'cursor-pointer', className)}
      {...props}
    />
  ),
);
Select.displayName = 'Select';

// Field：label + 控件 + 可选 hint 的竖排字段。
export function Field({
  label,
  hint,
  children,
  className,
}: {
  label: ReactNode;
  hint?: ReactNode;
  children: ReactNode;
  className?: string;
}) {
  return (
    <label className={cn('mb-3.5 block', className)}>
      <span className="mb-1.5 block text-xs text-ink-2">{label}</span>
      {children}
      {hint ? <span className="mt-1 block text-2xs text-ink-3">{hint}</span> : null}
    </label>
  );
}

// Switch：label 在左、复选框在右的一行开关。
export function Switch({
  label,
  checked,
  onChange,
  className,
}: {
  label: ReactNode;
  checked: boolean;
  onChange: (v: boolean) => void;
  className?: string;
}) {
  return (
    <label className={cn('flex items-center justify-between gap-3 py-2.5', className)}>
      <span className="text-sm">{label}</span>
      <input
        type="checkbox"
        className="h-4 w-4 cursor-pointer accent-accent"
        checked={checked}
        onChange={(e) => onChange(e.target.checked)}
      />
    </label>
  );
}

const tagVariants = cva(
  'inline-flex items-center gap-1.5 rounded-control px-2.5 py-px text-2xs font-medium whitespace-nowrap',
  {
    variants: {
      tone: {
        ok: 'bg-ok-soft text-ok',
        warn: 'bg-warn-soft text-warn',
        bad: 'bg-bad-soft text-bad',
        accent: 'bg-accent-soft text-accent',
        mute: 'border border-line bg-surface-2 text-ink-3',
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

export function Tag({ tone, children, className, title }: TagProps) {
  return (
    <span className={cn(tagVariants({ tone }), className)} title={title}>
      {children}
    </span>
  );
}

const badgeVariants = cva(
  'inline-flex items-center rounded-control border px-1 py-px text-3xs font-medium whitespace-nowrap',
  {
    variants: {
      tone: {
        accent: 'border-accent/25 bg-accent-soft text-accent',
        ok: 'border-ok/25 bg-ok-soft text-ok',
        warn: 'border-warn/25 bg-warn-soft text-warn',
        bad: 'border-bad/25 bg-bad-soft text-bad',
        mute: 'border-line bg-surface-2 text-ink-3',
      },
    },
    defaultVariants: { tone: 'accent' },
  },
);

export interface BadgeProps extends VariantProps<typeof badgeVariants> {
  children: ReactNode;
  className?: string;
  title?: string;
}

// Badge：极小方形标（域 / 版本 / 能力标记）。
export function Badge({ tone, children, className, title }: BadgeProps) {
  return (
    <span className={cn(badgeVariants({ tone }), className)} title={title}>
      {children}
    </span>
  );
}

const chipVariants = cva(
  'inline-flex items-baseline rounded-control px-1.5 py-0.5 font-mono text-xs font-semibold tabular-nums',
  {
    variants: {
      tone: {
        accent: 'bg-accent-soft text-accent',
        ok: 'bg-ok-soft text-ok',
        warn: 'bg-warn-soft text-warn',
        bad: 'bg-bad-soft text-bad',
        mute: 'bg-surface-2 text-ink-2',
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
