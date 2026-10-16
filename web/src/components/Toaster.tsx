import { useSyncExternalStore } from 'react';
import { cn } from '@/lib/utils';

interface ToastItem {
  id: number;
  msg: string;
  cls: '' | 'ok' | 'err';
}

let toasts: ToastItem[] = [];
let nextId = 1;
const listeners = new Set<() => void>();

function emit() {
  for (const l of listeners) l();
}

// toast：右下角提示，3.6s 自动消失（与旧面板一致）。
export function toast(msg: string, cls: '' | 'ok' | 'err' = '') {
  const item: ToastItem = { id: nextId++, msg, cls };
  toasts = [...toasts, item];
  emit();
  setTimeout(() => {
    toasts = toasts.filter((t) => t.id !== item.id);
    emit();
  }, 3600);
}

function subscribe(cb: () => void) {
  listeners.add(cb);
  return () => listeners.delete(cb);
}

export function Toaster() {
  const list = useSyncExternalStore(subscribe, () => toasts, () => toasts);
  return (
    <div className="pointer-events-none fixed bottom-5 right-5 z-[90] flex flex-col items-end gap-2">
      {list.map((t) => (
        <div
          key={t.id}
          className={cn(
            'max-w-[380px] rounded-lg border border-line border-l-[3px] border-l-ink-3 bg-raise px-3.5 py-2.5 text-[13px] shadow-panel',
            t.cls === 'ok' && 'border-l-ok',
            t.cls === 'err' && 'border-l-bad',
          )}
        >
          {t.msg}
        </div>
      ))}
    </div>
  );
}
