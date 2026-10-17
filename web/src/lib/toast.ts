// toast：模块级提示队列，右下角 3.6s 自动消失。与 UI 组件解耦，便于任意层调用。
export type ToastCls = '' | 'ok' | 'err';

export interface ToastItem {
  id: number;
  msg: string;
  cls: ToastCls;
}

let toasts: ToastItem[] = [];
let nextId = 1;
const listeners = new Set<() => void>();

function emit() {
  for (const l of listeners) l();
}

export function toast(msg: string, cls: ToastCls = '') {
  const item: ToastItem = { id: nextId++, msg, cls };
  toasts = [...toasts, item];
  emit();
  setTimeout(() => {
    toasts = toasts.filter((t) => t.id !== item.id);
    emit();
  }, 3600);
}

export function subscribe(cb: () => void): () => void {
  listeners.add(cb);
  return () => listeners.delete(cb);
}

export function get(): ToastItem[] {
  return toasts;
}
