import { useEffect, useRef, useState } from 'react';
import { useQueryClient } from '@tanstack/react-query';
import { api, getKey, postJSON } from '@/lib/api';
import { toast } from '@/components/Toaster';
import { Button } from '@/components/ui/button';
import { Input } from '@/components/ui/field';
import { Tabs } from '@/components/ui/tabs';
import { Modal, ModalBody, ModalFooter, ModalHeader, ModalTitle } from '@/components/ui/dialog';

type Phase = 'pick' | 'load' | 'ready' | 'done' | 'err';
type Tab = 'login' | 'import';

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

  return (
    <Modal open={open} onOpenChange={onOpenChange}>
      <ModalHeader>
        <div className="flex items-center gap-3">
          <ModalTitle>添加账号</ModalTitle>
          <Tabs
            active={tab}
            onSelect={(v) => setTab(v as Tab)}
            items={[
              { value: 'login', label: '浏览器登录' },
              { value: 'import', label: '导入 JSON' },
            ]}
          />
        </div>
      </ModalHeader>
      <ModalBody>
        {tab === 'login' ? (
          <>
            {phase === 'pick' ? (
              <>
                <div className="flex items-center gap-3.5 py-1 pb-2.5">
                  <span className="text-xs text-ink-3">版本：</span>
                  <label className="inline-flex items-center gap-1.5 text-xs text-ink-2">
                    <input type="radio" name="addRealm" className="accent-accent" checked={realm === 'cn'} onChange={() => setRealm('cn')} /> 国内版（CN）
                  </label>
                  <label className="inline-flex items-center gap-1.5 text-xs text-ink-2">
                    <input type="radio" name="addRealm" className="accent-accent" checked={realm === 'global'} onChange={() => setRealm('global')} /> 国际版（Global）
                  </label>
                </div>
                <div className="text-xs text-ink-3">国际版登录后，网关自动完成注册地区、激活与试用额度领取，全程无需手动操作。</div>
              </>
            ) : null}
            {phase === 'load' ? <div className="text-sm text-ink-2">正在获取授权链接…</div> : null}
            {phase === 'ready' ? (
              <>
                <div className="text-sm text-ink-2">在浏览器打开以下链接并登录：</div>
                <div className="my-2 mb-3 select-all break-all rounded-inner border border-line bg-surface-2 px-3 py-2.5 font-mono text-xs text-accent">{url}</div>
                <div className="text-sm text-ink-2">等待授权完成，自动检测中…</div>
              </>
            ) : null}
            {phase === 'done' ? <div className="text-sm text-ok">{doneMsg}</div> : null}
            {phase === 'err' ? <div className="text-sm text-bad">{errMsg}</div> : null}
          </>
        ) : (
          <>
            <div className="mb-2 text-2xs text-ink-3">选择 cockpit tools 导出的 JSON 文件，批量导入账号。文件需为账号数组格式。</div>
            <Input
              type="file"
              accept=".json"
              className="mb-2 py-1.5"
              onChange={(e) => {
                const f = e.target.files?.[0];
                if (f) void onImport(f);
                e.target.value = '';
              }}
            />
            {importMsg ? <div className="text-sm text-ok">{importMsg}</div> : null}
            {importErr ? <div className="text-sm text-bad">{importErr}</div> : null}
          </>
        )}
      </ModalBody>
      <ModalFooter>
        <span className="flex-1" />
        {tab === 'login' && phase === 'pick' ? (
          <Button variant="primary" onClick={() => void startLogin()}>
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
            <Button variant="primary" onClick={() => window.open(url, '_blank')}>
              在浏览器打开
            </Button>
          </>
        ) : null}
        <Button onClick={() => onOpenChange(false)}>关闭</Button>
      </ModalFooter>
    </Modal>
  );
}
