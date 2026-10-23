// RemarksDialog.tsx 账号备注管理（只在本机浏览器生效）。
//
// 备注不落服务端、不改 auth 文件：它只是「这台机器上我怎么称呼这批号」的显示层
// 偏好。对话框一次列出全部账号 + 已存但当前不在列表的键，方便换号后清理残留。
import { useEffect, useMemo, useState } from 'react';
import { Button } from '@/components/ui/button';
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from '@/components/ui/dialog';
import { Input } from '@/components/ui/input';
import { readRemarks, remarkKey, writeRemarks, type RemarkMap } from '@/lib/remarks';

export interface RemarkAccount {
  uid: string;
  name?: string;
}

// RemarksDialog 备注编辑弹窗：保存/清空均写 localStorage。
export function RemarksDialog({
  open,
  accounts,
  onClose,
  onSaved,
}: {
  open: boolean;
  accounts: RemarkAccount[];
  onClose: () => void;
  onSaved?: () => void;
}) {
  const [draft, setDraft] = useState<RemarkMap>({});

  // 打开时读一次当前值：避免在渲染期反复读 localStorage。
  useEffect(() => {
    if (!open) return;
    setDraft(readRemarks());
  }, [open]);

  const rows = useMemo(() => {
    const listed = accounts.map((a) => ({
      key: remarkKey(a.uid),
      uid: a.uid,
      name: a.name || remarkKey(a.uid) || '—',
      stored: true,
    }));
    const seen = new Set(listed.map((r) => r.key));
    const orphans = Object.keys(draft)
      .filter((key) => !seen.has(key))
      .sort()
      .map((key) => ({ key, uid: key, name: '（已不在账号池）', stored: false }));
    return [...listed, ...orphans];
  }, [accounts, draft]);

  const save = () => {
    const next: RemarkMap = {};
    for (const [key, value] of Object.entries(draft)) {
      const text = value.trim();
      if (key && text) next[key] = text;
    }
    writeRemarks(next);
    onSaved?.();
    onClose();
  };

  return (
    <Dialog
      open={open}
      onOpenChange={(next) => {
        if (!next) onClose();
      }}
    >
      <DialogContent className="sm:max-w-2xl">
        <DialogHeader>
          <DialogTitle>账号备注</DialogTitle>
          <DialogDescription>
            备注只保存在<strong>当前这台机器的浏览器</strong>里：不改 auth 文件、不进服务端状态，换设备/换浏览器不可见。
          </DialogDescription>
        </DialogHeader>
        <div className="max-h-[60vh] overflow-y-auto">
          {rows.length === 0 ? (
            <p className="py-6 text-center text-sm text-muted-foreground">还没有账号</p>
          ) : (
            <div className="flex flex-col gap-2">
              {rows.map((row) => (
                <div key={row.key + row.uid} className="flex flex-col gap-1.5 border-b border-border/60 pb-2 last:border-0 last:pb-0 sm:flex-row sm:items-center sm:gap-3">
                  <div className="min-w-0 sm:w-[42%]">
                    <div className="truncate font-mono text-xs text-muted-foreground" title={row.uid}>
                      {row.uid}
                    </div>
                    <div className="truncate text-xs" title={row.name}>
                      {row.name}
                    </div>
                  </div>
                  <div className="flex min-w-0 items-center gap-2 sm:flex-1">
                    <Input
                      className="min-w-0 flex-1"
                      placeholder="备注（留空 = 恢复原标题）"
                      value={draft[row.key] ?? ''}
                      onChange={(e) => setDraft((prev) => ({ ...prev, [row.key]: e.target.value }))}
                    />
                    <Button
                      size="xs"
                      variant="ghost"
                      className="shrink-0"
                      onClick={() => setDraft((prev) => ({ ...prev, [row.key]: '' }))}
                    >
                      清空
                    </Button>
                  </div>
                </div>
              ))}
            </div>
          )}
        </div>
        <DialogFooter>
          <Button size="sm" variant="ghost" onClick={onClose}>
            取消
          </Button>
          <Button size="sm" onClick={save}>
            保存
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
