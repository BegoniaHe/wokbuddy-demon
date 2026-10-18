import { useCallback, useRef, useState } from 'react';
import { Button } from '@/components/ui/button';
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from '@/components/ui/dialog';

export interface ConfirmOptions {
  title: string;
  description?: string;
  confirmText?: string;
  cancelText?: string;
  destructive?: boolean;
}

// useConfirm：Promise 形态的确认弹窗，替换原生 confirm()。
//
// 原生 confirm 阻塞渲染、不可样式化，也无法和页面的主题/无障碍语义一致。
// 用法：`if (!(await confirm({ title: '删除？', destructive: true }))) return;`
// 弹窗实例由 hook 返回，渲染一次即可；从 DropdownMenu 里触发也不会因菜单
// 关闭而丢失（不依赖 trigger 元素）。
export function useConfirm() {
  const [open, setOpen] = useState(false);
  const [opts, setOpts] = useState<ConfirmOptions>({ title: '' });
  const resolver = useRef<((ok: boolean) => void) | null>(null);

  const confirm = useCallback((next: ConfirmOptions): Promise<boolean> => {
    setOpts(next);
    setOpen(true);
    return new Promise<boolean>((resolve) => {
      resolver.current = resolve;
    });
  }, []);

  const settle = useCallback((ok: boolean) => {
    resolver.current?.(ok);
    resolver.current = null;
    setOpen(false);
  }, []);

  const confirmDialog = (
    <Dialog
      open={open}
      onOpenChange={(next) => {
        if (!next) settle(false);
      }}
    >
      <DialogContent showCloseButton={false}>
        <DialogHeader>
          <DialogTitle>{opts.title}</DialogTitle>
          {opts.description ? (
            <DialogDescription className="whitespace-pre-line">{opts.description}</DialogDescription>
          ) : null}
        </DialogHeader>
        <DialogFooter>
          <Button variant="outline" onClick={() => settle(false)}>
            {opts.cancelText ?? '取消'}
          </Button>
          <Button
            variant={opts.destructive ? 'destructive' : 'default'}
            onClick={() => settle(true)}
          >
            {opts.confirmText ?? '确认'}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );

  return { confirm, confirmDialog };
}
