import { useEffect, useState } from 'react';
import { useQueryClient } from '@tanstack/react-query';
import { api, setKey } from '@/lib/api';
import { Button } from '@/components/ui/button';
import { Input } from '@/components/ui/input';
import {
  Dialog,
  DialogContent,
  DialogHeader,
  DialogTitle,
  DialogDescription,
  DialogFooter,
} from '@/components/ui/dialog';

// KeyGate：管理面启用鉴权时，401 会打开此弹窗。提交后校验一次 overview 通过即放行。
export function KeyGate({ open, onOpenChange }: { open: boolean; onOpenChange: (v: boolean) => void }) {
  const [val, setVal] = useState('');
  const [err, setErr] = useState(false);
  const qc = useQueryClient();

  useEffect(() => {
    if (open) {
      setVal('');
      setErr(false);
    }
  }, [open]);

  const submit = async () => {
    const v = val.trim();
    if (!v) return;
    setKey(v);
    try {
      await api('overview');
      setErr(false);
      onOpenChange(false);
      qc.invalidateQueries();
    } catch {
      setErr(true);
    }
  };

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-md" showCloseButton={false}>
        <DialogHeader>
          <DialogTitle>需要管理密钥</DialogTitle>
          <DialogDescription>面板管理面需要 admin_api_key（或仅只读的 readonly_api_key）；未配置管理密钥时使用 config.json 的 api_key。</DialogDescription>
        </DialogHeader>
        <Input
          type="password"
          autoComplete="current-password"
          placeholder="管理密钥"
          value={val}
          onChange={(e) => setVal(e.target.value)}
          onKeyDown={(e) => {
            if (e.key === 'Enter') void submit();
          }}
        />
        {err ? <p className="text-sm text-destructive">密钥不正确，请重试。</p> : null}
        <DialogFooter>
          <Button variant="default" onClick={() => void submit()}>
            进入
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
