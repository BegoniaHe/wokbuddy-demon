import { RefreshCw, TriangleAlert } from 'lucide-react';
import { Button } from '@/components/ui/button';
import { cn } from '@/lib/utils';

// LoadError：读取失败的常驻提示。
//
// page   = 首屏一个字段都没成功，占满内容区并提供重试；
// inline = 已有数据、本次刷新失败，只挂一条提示，不能把旧数据顶掉。
// 不用 toast：toast 几秒后消失，用户切回来时看不到「有东西没刷新成功」。
export function LoadError({
  variant = 'inline',
  message,
  hint,
  onRetry,
  className,
}: {
  variant?: 'page' | 'inline';
  message?: string;
  hint?: string;
  onRetry?: () => void;
  className?: string;
}) {
  if (variant === 'inline') {
    return (
      <div
        role="status"
        className={cn(
          'flex flex-wrap items-center gap-x-2 gap-y-1 rounded-md border border-warning/40 bg-warning-soft px-3.5 py-2 text-xs',
          className,
        )}
      >
        <TriangleAlert className="size-3.5 shrink-0 text-warning" />
        <span className="flex-1 leading-5">{message ?? '数据刷新失败，当前显示上一次结果'}</span>
        {onRetry ? (
          <Button variant="ghost" size="xs" onClick={onRetry}>
            <RefreshCw />
            重试
          </Button>
        ) : null}
      </div>
    );
  }

  return (
    <div
      role="alert"
      className={cn('flex flex-col items-center justify-center gap-3 rounded-lg bg-muted px-6 py-16 text-center', className)}
    >
      <div className="grid size-12 place-items-center rounded-full bg-warning-soft">
        <TriangleAlert className="size-5 text-warning" />
      </div>
      <div className="text-sm font-medium">{message ?? '读取失败'}</div>
      <p className="max-w-md text-xs leading-5 text-muted-foreground">
        {hint ?? '请确认服务仍在运行，然后重试。'}
      </p>
      {onRetry ? (
        <Button variant="outline" size="sm" onClick={onRetry}>
          <RefreshCw />
          重试
        </Button>
      ) : null}
    </div>
  );
}
