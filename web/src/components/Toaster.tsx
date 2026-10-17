import { useSyncExternalStore } from 'react';
import { subscribe, get, type ToastCls } from '@/lib/toast';
import { cn } from '@/lib/utils';

// Toaster：右下角提示，3.6s 自动消失。
export function Toaster() {
  const list = useSyncExternalStore(subscribe, get, get);
  return (
    <div className="pointer-events-none fixed bottom-5 right-5 z-[90] flex flex-col items-end gap-2">
      {list.map((t) => (
        <div
          key={t.id}
          className={cn(
            'max-w-[380px] rounded-lg border border-l-[3px] border-l-muted-foreground bg-popover px-3.5 py-2.5 text-sm text-popover-foreground shadow-panel',
            t.cls === 'ok' && 'border-l-success',
            t.cls === 'err' && 'border-l-destructive',
          )}
        >
          {t.msg}
        </div>
      ))}
    </div>
  );
}

export { subscribe as subscribeToast, get as getToasts, type ToastCls };
export { toast } from '@/lib/toast';
