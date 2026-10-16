import {
  forwardRef,
  type InputHTMLAttributes,
  type ReactNode,
  type SelectHTMLAttributes,
} from 'react';
import { cva, type VariantProps } from 'class-variance-authority';
import { cn } from '@/lib/utils';

// 通用表单控件样式：整行宽，窄场景由调用方覆盖 width。
const controlCls =
  'w-full rounded-md border border-line bg-surface-2 px-2.5 py-[7px] text-ink placeholder:text-ink-3 focus:outline-none focus:border-accent';

export const Input = forwardRef<HTMLInputElement, InputHTMLAttributes<HTMLInputElement>>(
  ({ className, ...props }, ref) => (
    <input ref={ref} className={cn(controlCls, className)} {...props} />
  ),
);
Input.displayName = 'Input';

export const Select = forwardRef<HTMLSelectElement, SelectHTMLAttributes<HTMLSelectElement>>(
  ({ className, ...props }, ref) => (
    <select ref={ref} className={cn(controlCls, 'cursor-pointer', className)} {...props} />
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
      <span className="mb-1.5 block text-[12.5px] text-ink-2">{label}</span>
      {children}
      {hint ? <span className="mt-1 block text-[11.5px] text-ink-3">{hint}</span> : null}
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
      <span className="text-[13.5px]">{label}</span>
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
  'inline-flex items-center gap-1.5 rounded-full px-2.5 py-px text-[11.5px] font-medium whitespace-nowrap',
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
