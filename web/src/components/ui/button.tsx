import { forwardRef, type ButtonHTMLAttributes } from 'react';
import { cva, type VariantProps } from 'class-variance-authority';
import { cn } from '@/lib/utils';

const buttonVariants = cva(
  'inline-flex items-center justify-center gap-1.5 rounded-control whitespace-nowrap transition-colors disabled:opacity-50 disabled:cursor-default focus-visible:outline-none',
  {
    variants: {
      variant: {
        default: 'border border-line bg-surface text-ink hover:border-ink-3',
        primary: 'border border-accent bg-accent text-accent-ink font-medium hover:opacity-90',
        ghost: 'border border-transparent text-ink-2 hover:text-ink hover:bg-surface-2',
        danger: 'border border-line bg-surface text-bad hover:border-bad',
      },
      size: {
        default: 'px-3 py-1.5 text-sm',
        xs: 'px-2.5 py-1 text-xs',
        icon: 'p-2',
      },
    },
    defaultVariants: { variant: 'default', size: 'default' },
  },
);

export interface ButtonProps
  extends ButtonHTMLAttributes<HTMLButtonElement>,
    VariantProps<typeof buttonVariants> {}

export const Button = forwardRef<HTMLButtonElement, ButtonProps>(
  ({ className, variant, size, type = 'button', ...props }, ref) => (
    <button
      ref={ref}
      type={type}
      className={cn(buttonVariants({ variant, size }), className)}
      {...props}
    />
  ),
);
Button.displayName = 'Button';

export { buttonVariants };
