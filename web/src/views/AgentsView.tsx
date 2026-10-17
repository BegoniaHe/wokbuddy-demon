import { useState } from 'react';
import { useQuery, useQueryClient } from '@tanstack/react-query';
import { api, postJSON } from '@/lib/api';
import { toast } from '@/components/Toaster';
import { Button } from '@/components/ui/button';
import { Field, FieldDescription, FieldLabel } from '@/components/ui/field';
import { Input } from '@/components/ui/input';
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card';

type AgentStatus = {
  id: string;
  name: string;
  path: string;
  detected?: boolean;
  applied?: boolean;
  modified?: boolean;
  applied_at?: string;
};

// AgentsView 本机智能体一键配置（issue #102）：仅回环访问。
export function AgentsView() {
  const qc = useQueryClient();
  const [apiKey, setApiKey] = useState('');
  const [baseURL, setBaseURL] = useState('http://127.0.0.1:7863');
  const [model, setModel] = useState('glm-5.2');
  const [busy, setBusy] = useState('');

  const q = useQuery({
    queryKey: ['agents'],
    queryFn: () => api<{ clients?: AgentStatus[] }>('agents'),
  });
  const clients = q.data?.clients || [];

  const apply = async (id: string) => {
    if (!apiKey.trim()) {
      toast('请填写 API key（只写入本机客户端配置）', 'err');
      return;
    }
    setBusy(id);
    try {
      await postJSON('agents/' + id + '/apply', { api_key: apiKey.trim(), base_url: baseURL.trim(), model: model.trim() });
      toast('已写入配置并备份原件', 'ok');
      await qc.invalidateQueries({ queryKey: ['agents'] });
    } catch (e) {
      toast('写入失败：' + (e as Error).message, 'err');
    } finally {
      setBusy('');
    }
  };

  const restore = async (c: AgentStatus) => {
    if (!confirm('还原「' + c.name + '」到首次写入前？网关新建的文件会被删除。')) return;
    setBusy(c.id);
    try {
      await postJSON('agents/' + c.id + '/restore', {});
      toast('已还原', 'ok');
    } catch (e) {
      const msg = (e as Error).message;
      if (msg.includes('修改')) {
        if (!confirm(msg + '\n\n仍要强制还原？')) return;
        try {
          await postJSON('agents/' + c.id + '/restore', { force: true });
          toast('已强制还原', 'ok');
        } catch (e2) {
          toast('还原失败：' + (e2 as Error).message, 'err');
        }
      } else {
        toast('还原失败：' + msg, 'err');
      }
    } finally {
      setBusy('');
      await qc.invalidateQueries({ queryKey: ['agents'] });
    }
  };

  return (
    <>
      <Card className="mb-5">
        <CardHeader>
          <CardTitle>连接参数</CardTitle>
          <CardDescription>仅本机回环可用；API key 只写入客户端本地配置，面板不回显。</CardDescription>
        </CardHeader>
        <CardContent className="grid gap-4 md:grid-cols-3">
          <Field>
            <FieldLabel>网关地址</FieldLabel>
            <Input value={baseURL} onChange={(e) => setBaseURL(e.target.value)} placeholder="http://127.0.0.1:7863" />
            <FieldDescription>不要带 /v1 后缀</FieldDescription>
          </Field>
          <Field>
            <FieldLabel>API key</FieldLabel>
            <Input type="password" value={apiKey} onChange={(e) => setApiKey(e.target.value)} placeholder="config.json 中的 api_key" />
          </Field>
          <Field>
            <FieldLabel>默认模型</FieldLabel>
            <Input value={model} onChange={(e) => setModel(e.target.value)} placeholder="glm-5.2" />
          </Field>
        </CardContent>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle>本机客户端</CardTitle>
          <CardDescription>写入前备份首次原件；还原按备份恢复。</CardDescription>
        </CardHeader>
        <CardContent className="space-y-3">
          {q.error ? <p className="text-sm text-destructive">读取失败：{(q.error as Error).message}</p> : null}
          {clients.map((c) => (
            <div key={c.id} className="flex flex-wrap items-center gap-3 rounded-md border border-border p-3">
              <div className="min-w-0 flex-1">
                <div className="flex items-center gap-2">
                  <span className="font-medium">{c.name}</span>
                  <span className={'text-2xs ' + (c.detected ? 'text-success' : 'text-muted-foreground')}>
                    {c.detected ? '已检测到' : '未检测到'}
                  </span>
                  {c.applied ? <span className="text-2xs text-warning">已写入</span> : null}
                  {c.modified ? <span className="text-2xs text-destructive">外部修改</span> : null}
                </div>
                <div className="truncate font-mono text-2xs text-muted-foreground">{c.path}</div>
              </div>
              <Button size="xs" disabled={busy === c.id} onClick={() => void apply(c.id)}>
                写入
              </Button>
              <Button size="xs" variant="ghost" disabled={!c.applied || busy === c.id} onClick={() => void restore(c)}>
                还原
              </Button>
            </div>
          ))}
          <p className="text-2xs text-muted-foreground">
            Codex CLI 的 provider 使用环境变量 <code>WOKBUDDY_API_KEY</code>；写入后需在启动 Codex 的 shell 中导出同一把 key。
          </p>
        </CardContent>
      </Card>
    </>
  );
}
