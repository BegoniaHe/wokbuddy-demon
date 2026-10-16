import type { ReactNode } from 'react';
import * as DialogPrimitive from '@radix-ui/react-dialog';
import { cn } from '@/lib/utils';

// Modal：基于 Radix Dialog（焦点陷阱 / Esc / 滚动锁开箱即用）。
// 外观对齐旧面板的 .veil/.dlg：遮罩 + 居中卡片，标题走 Radix.Title（a11y）。
export function Modal({
  open,
  onOpenChange,
  width = 560,
  children,
}: {
  open: boolean;
  onOpenChange: (v: boolean) => void;
  width?: number;
  children: ReactNode;
}) {
  return (
    <DialogPrimitive.Root open={open} onOpenChange={onOpenChange}>
      <DialogPrimitive.Portal>
        <DialogPrimitive.Overlay className="fixed inset-0 z-[60] bg-[rgba(6,8,13,0.62)] backdrop-blur-[2px]" />
        <DialogPrimitive.Content
          aria-describedby={undefined}
          className="fixed left-1/2 top-1/2 z-[60] flex max-h-[86vh] w-[calc(100%-40px)] -translate-x-1/2 -translate-y-1/2 flex-col rounded-surface border border-line bg-surface shadow-[0_24px_64px_-24px_rgba(0,0,0,0.6)] focus:outline-none"
          style={{ maxWidth: width }}
        >
          {children}
        </DialogPrimitive.Content>
      </DialogPrimitive.Portal>
    </DialogPrimitive.Root>
  );
}

export function ModalHeader({ children, className }: { children: ReactNode; className?: string }) {
  return (
    <header className={cn('border-b border-line-soft px-5 pb-3 pt-4', className)}>{children}</header>
  );
}

export function ModalTitle({ children }: { children: ReactNode }) {
  return <DialogPrimitive.Title className="text-lg font-semibold">{children}</DialogPrimitive.Title>;
}

export function ModalHint({ children }: { children: ReactNode }) {
  return <div className="mt-1 text-xs text-ink-3">{children}</div>;
}

export function ModalBody({ children, className }: { children: ReactNode; className?: string }) {
  return <div className={cn('overflow-auto px-5 py-4', className)}>{children}</div>;
}

export function ModalFooter({ children, className }: { children: ReactNode; className?: string }) {
  return (
    <footer className={cn('flex gap-2.5 border-t border-line-soft px-5 py-3', className)}>
      {children}
    </footer>
  );
}

export const CloseOnEscape = DialogPrimitive.Close;
