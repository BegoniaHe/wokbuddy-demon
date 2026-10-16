import type { CSSProperties, HTMLAttributes, ReactNode, ThHTMLAttributes, TdHTMLAttributes } from 'react';
import { cn } from '@/lib/utils';

// 表格：与旧面板一致——横向可滚、行 hover 高亮、表头小号弱化。
export function TableWrap({ children, className }: { children: ReactNode; className?: string }) {
  return <div className={cn('overflow-x-auto', className)}>{children}</div>;
}

export function Table({ children, className }: { children: ReactNode; className?: string }) {
  return (
    <table className={cn('w-full min-w-[640px] border-collapse text-sm', className)}>
      {children}
    </table>
  );
}

export function Th({ children, className, ...props }: ThHTMLAttributes<HTMLTableCellElement>) {
  return (
    <th
      className={cn(
        'whitespace-nowrap border-b border-line px-2.5 py-2.5 text-left text-2xs font-medium text-ink-3',
        className,
      )}
      {...props}
    >
      {children}
    </th>
  );
}

export function Td({ children, className, ...props }: TdHTMLAttributes<HTMLTableCellElement>) {
  return (
    <td
      className={cn('border-b border-line-soft px-2.5 py-2.5 align-middle', className)}
      {...props}
    >
      {children}
    </td>
  );
}

// Empty：空态占位。
export function Empty({ children }: { children: ReactNode }) {
  return <div className="px-5 py-10 text-center text-ink-3">{children}</div>;
}

// Num：等宽数字单元格内容。
export function Num({ children, className, ...props }: HTMLAttributes<HTMLSpanElement>) {
  return (
    <span className={cn('font-mono tabular-nums whitespace-nowrap', className)} {...props}>
      {children}
    </span>
  );
}

const railTone = {
  accent: 'bg-accent',
  ok: 'bg-ok',
  warn: 'bg-warn',
  bad: 'bg-bad',
  mute: 'bg-line',
} as const;

export type RailTone = keyof typeof railTone;

// RailCell：行首 3px 状态色条单元格（账号 / 模型 / 包表统一使用）。
export function RailCell({
  tone,
  style,
  className,
}: {
  tone?: RailTone;
  style?: CSSProperties;
  className?: string;
}) {
  return (
    <Td className="w-[3px] p-0">
      {tone || style ? (
        <i className={cn('block h-full min-h-[36px] w-[3px]', tone && railTone[tone], className)} style={style} />
      ) : null}
    </Td>
  );
}
