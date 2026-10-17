import { useEffect, useRef, useState } from 'react';
import { useQueryClient } from '@tanstack/react-query';
import { api, getKey, postJSON } from '@/lib/api';
import { toast } from '@/components/Toaster';
import { Button } from '@/components/ui/button';
import { Input } from '@/components/ui/input';
import { Tabs, TabsList, TabsTrigger } from '@/components/ui/tabs';
import {
  Dialog,
  DialogContent,
  DialogHeader,
  DialogTitle,
  DialogFooter,
} from '@/components/ui/dialog';

type Phase = 'pick' | 'load' | 'ready' | 'done' | 'err';
type Tab = 'login' | 'import' | 'secure';

export function AddAccountDialog({ open, onOpenChange }: { open: boolean; onOpenChange: (v: boolean) => void }) {
  const qc = useQueryClient();
  const [tab, setTab] = useState<Tab>('login');
  const [realm, setRealm] = useState('cn');
  const [phase, setPhase] = useState<Phase>('pick');
  const [url, setUrl] = useState('');
  const [errMsg, setErrMsg] = useState('');
  const [doneMsg, setDoneMsg] = useState('');
  const [importMsg, setImportMsg] = useState('');
  const [importErr, setImportErr] = useState('');
  const [secPass, setSecPass] = useState('');
  const [secMsg, setSecMsg] = useState('');
  const [secErr, setSecErr] = useState('');
  const stateRef = useRef<string | null>(null);
  const timerRef = useRef<number | null>(null);

  const stopPoll = () => {
    if (timerRef.current) {
      clearInterval(timerRef.current);
      timerRef.current = null;
    }
  };

  useEffect(() => {
    if (open) {
      setTab('login');
      setPhase('pick');
      setUrl('');
      setErrMsg('');
      setDoneMsg('');
      setImportMsg('');
      setImportErr('');
      setSecPass('');
      setSecMsg('');
      setSecErr('');
      stateRef.current = null;
      stopPoll();
    } else {
      stopPoll();
    }
    return stopPoll;
  }, [open]);

  const startLogin = async () => {
    setPhase('load');
    setErrMsg('');
    try {
      const r = await postJSON<{ state: string; url: string }>('login/start', { realm });
      stateRef.current = r.state;
      setUrl(r.url);
      setPhase('ready');
      stopPoll();
      timerRef.current = window.setInterval(() => void poll(), 3000);
    } catch (e) {
      setPhase('err');
      setErrMsg((e as Error).message);
    }
  };

  const poll = async () => {
    if (!stateRef.current) return;
    try {
      const r = await api<{ done?: boolean; nickname?: string; uid?: string; realm?: string; credits?: number; credits_total?: number }>(
        'login/poll?state=' + encodeURIComponent(stateRef.current),
      );
      if (r.done) {
        stopPoll();
        setPhase('done');
        setDoneMsg(
          '已添加 ' +
            (r.nickname || r.uid) +
            (r.realm === 'global' ? '（国际版）' : '') +
            ((r.credits ?? -1) >= 0 ? ' · 积分 ' + r.credits + ((r.credits_total ?? 0) > 0 ? '/' + r.credits_total : '') : '') +
            '，账号已载入池中',
        );
        setTimeout(() => {
          onOpenChange(false);
          void qc.invalidateQueries({ queryKey: ['overview'] });
        }, 1600);
      }
    } catch (e) {
      stopPoll();
      setPhase('err');
      setErrMsg((e as Error).message + '（关闭后重新添加）');
    }
  };

  const onImport = async (file: File) => {
    setImportMsg('');
    setImportErr('');
    const fd = new FormData();
    fd.append('file', file);
    const h: Record<string, string> = {};
    const k = getKey();
    if (k) h['Authorization'] = 'Bearer ' + k;
    try {
      const r = await fetch('/panel/api/import/cockpit', { method: 'POST', body: fd, headers: h });
      const d = (await r.json()) as { error?: string; imported?: number; skipped?: number };
      if (!r.ok) throw new Error(d.error || 'HTTP ' + r.status);
      setImportMsg('导入完成：成功 ' + d.imported + ' 个' + (d.skipped ? '，跳过 ' + d.skipped + ' 个' : ''));
      void qc.invalidateQueries({ queryKey: ['overview'] });
    } catch (e) {
      setImportErr('导入失败：' + (e as Error).message);
    }
  };

  const onSecureExport = async () => {
    setSecMsg('');
    setSecErr('');
    if (secPass.length < 8) {
      setSecErr('密码至少 8 位');
      return;
    }
    const h: Record<string, string> = { 'Content-Type': 'application/json' };
    const k = getKey();
    if (k) h['Authorization'] = 'Bearer ' + k;
    try {
      const r = await fetch('/panel/api/export/encrypted', { method: 'POST', headers: h, body: JSON.stringify({ password: secPass }) });
      if (!r.ok) {
        const d = (await r.json().catch(() => ({}))) as { error?: string };
        throw new Error(d.error || 'HTTP ' + r.status);
      }
      const blob = await r.blob();
      const url = URL.createObjectURL(blob);
      const a = document.createElement('a');
      a.href = url;
      a.download = 'wokbuddy-accounts.wbenc.json';
      a.click();
      URL.revokeObjectURL(url);
      setSecMsg('已导出加密备份文件');
    } catch (e) {
      setSecErr('导出失败：' + (e as Error).message);
    }
  };

  const onSecureImport = async (file: File) => {
    setSecMsg('');
    setSecErr('');
    if (secPass.length < 8) {
      setSecErr('请先输入至少 8 位密码');
      return;
    }
    const fd = new FormData();
    fd.append('file', file);
    fd.append('password', secPass);
    const h: Record<string, string> = {};
    const k = getKey();
    if (k) h['Authorization'] = 'Bearer ' + k;
    try {
      const r = await fetch('/panel/api/import/encrypted', { method: 'POST', body: fd, headers: h });
      const d = (await r.json()) as { error?: string; imported?: number; skipped?: number };
      if (!r.ok) throw new Error(d.error || 'HTTP ' + r.status);
      setSecMsg('导入完成：成功 ' + d.imported + ' 个' + (d.skipped ? '，跳过 ' + d.skipped + ' 个' : ''));
      void qc.invalidateQueries({ queryKey: ['overview'] });
    } catch (e) {
      setSecErr('导入失败：' + (e as Error).message);
    }
  };

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-lg" showCloseButton={false}>
        <DialogHeader>
          <div className="flex items-center gap-3">
            <DialogTitle>添加账号</DialogTitle>
            <Tabs value={tab} onValueChange={(v) => setTab(v as Tab)}>
              <TabsList>
                <TabsTrigger value="login">浏览器登录</TabsTrigger>
                <TabsTrigger value="import">导入 JSON</TabsTrigger>
                <TabsTrigger value="secure">加密备份</TabsTrigger>
              </TabsList>
            </Tabs>
          </div>
        </DialogHeader>
        <div className="flex flex-col gap-3">
          {tab === 'login' ? (
            <>
              {phase === 'pick' ? (
                <>
                  <div className="flex items-center gap-3.5">
                    <span className="text-xs text-muted-foreground">版本：</span>
                    <label className="inline-flex items-center gap-1.5 text-xs text-foreground">
                      <input type="radio" name="addRealm" checked={realm === 'cn'} onChange={() => setRealm('cn')} /> 国内版（CN）
                    </label>
                    <label className="inline-flex items-center gap-1.5 text-xs text-foreground">
                      <input type="radio" name="addRealm" checked={realm === 'global'} onChange={() => setRealm('global')} /> 国际版（Global）
                    </label>
                  </div>
                  <p className="text-xs text-muted-foreground">国际版登录后，网关自动完成注册地区、激活与试用额度领取，全程无需手动操作。</p>
                </>
              ) : null}
              {phase === 'load' ? <p className="text-sm text-muted-foreground">正在获取授权链接…</p> : null}
              {phase === 'ready' ? (
                <>
                  <p className="text-sm text-muted-foreground">在浏览器打开以下链接并登录：</p>
                  <div className="select-all break-all rounded-lg border border-border bg-secondary px-3 py-2.5 font-mono text-xs text-accent-foreground">{url}</div>
                  <p className="text-sm text-muted-foreground">等待授权完成，自动检测中…</p>
                </>
              ) : null}
              {phase === 'done' ? <p className="text-sm text-success">{doneMsg}</p> : null}
              {phase === 'err' ? <p className="text-sm text-destructive">{errMsg}</p> : null}
            </>
          ) : tab === 'import' ? (
            <>
              <p className="text-2xs text-muted-foreground">选择 cockpit tools 导出的 JSON 文件，批量导入账号。文件需为账号数组格式。</p>
              <Input
                type="file"
                accept=".json"
                onChange={(e) => {
                  const f = e.target.files?.[0];
                  if (f) void onImport(f);
                  e.target.value = '';
                }}
              />
              {importMsg ? <p className="text-sm text-success">{importMsg}</p> : null}
              {importErr ? <p className="text-sm text-destructive">{importErr}</p> : null}
            </>
          ) : (
            <>
              <p className="text-2xs text-muted-foreground">
                AES-256-GCM 加密的账号备份（含登录态 token）。设置密码后导出为加密文件；导入需同一密码。密码不会写入文件，也无法找回。
              </p>
              <Input
                type="password"
                placeholder="备份密码（至少 8 位）"
                value={secPass}
                onChange={(e) => setSecPass(e.target.value)}
              />
              <div className="flex items-center gap-2">
                <Button variant="default" onClick={() => void onSecureExport()}>
                  导出加密备份
                </Button>
                <span className="text-2xs text-muted-foreground">或选择备份文件导入：</span>
              </div>
              <Input
                type="file"
                accept=".json"
                onChange={(e) => {
                  const f = e.target.files?.[0];
                  if (f) void onSecureImport(f);
                  e.target.value = '';
                }}
              />
              {secMsg ? <p className="text-sm text-success">{secMsg}</p> : null}
              {secErr ? <p className="text-sm text-destructive">{secErr}</p> : null}
            </>
          )}
        </div>
        <DialogFooter>
          {tab === 'login' && phase === 'pick' ? (
            <Button variant="default" onClick={() => void startLogin()}>
              获取授权链接
            </Button>
          ) : null}
          {tab === 'login' && phase === 'ready' ? (
            <>
              <Button
                onClick={() =>
                  void navigator.clipboard.writeText(url).then(
                    () => toast('链接已复制', 'ok'),
                    () => toast('复制失败，请手动选择复制', 'err'),
                  )
                }
              >
                复制链接
              </Button>
              <Button variant="default" onClick={() => window.open(url, '_blank')}>
                在浏览器打开
              </Button>
            </>
          ) : null}
          <Button onClick={() => onOpenChange(false)}>关闭</Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
