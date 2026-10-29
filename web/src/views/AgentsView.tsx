import { useState } from 'react';
import { useQuery, useQueryClient } from '@tanstack/react-query';
import { api, postJSON } from '@/lib/api';
import { toast } from '@/components/Toaster';
import { useConfirm } from '@/components/ConfirmDialog';
import { LoadError } from '@/components/LoadError';
import { Button } from '@/components/ui/button';
import { Field, FieldDescription, FieldLabel } from '@/components/ui/field';
import { Input } from '@/components/ui/input';
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card';
import {
  Select,
  SelectContent,
  SelectOptions,
  SelectTrigger,
  SelectValue,
  selectOptionItems,
  type SelectOption,
} from '@/components/ui/select';
import { MAX_IMPORT_MODELS, countByRealm, selectModelIDs, type ModelRealmFilter } from '@/lib/agents';
import type { ModelEntry } from '@/lib/models';

type AgentStatus = {
  id: string;
  name: string;
  path: string;
  detected?: boolean;
  applied?: boolean;
  modified?: boolean;
  applied_at?: string;
};

// MODEL_SOURCE_OPTIONS 模型来源：单个模型（现状）/ 全部上游模型（#20 一键导入）。
const MODEL_SOURCE_OPTIONS: SelectOption[] = [
  { value: 'single', label: '单个模型' },
  { value: 'all', label: '全部上游模型' },
];

// REALM_FILTER_OPTIONS 全部模型模式的域过滤。
const REALM_FILTER_OPTIONS: SelectOption[] = [
  { value: 'all', label: '两域' },
  { value: 'cn', label: '只看 cn' },
  { value: 'global', label: '只看 global' },
];

// CLIENT_MODEL_SUPPORT 各客户端的多模型能力说明（避免用户以为三处行为一致）。
const CLIENT_MODEL_SUPPORT: Record<string, string> = {
  opencode: '全部模型写进 provider.wokbuddy.models',
  codex: '额外模型写成 profiles，用 codex --profile "<id>" 切换',
  claude: '只写默认模型，其余在 Claude Code 内用 /model <id> 切换',
};

// AgentsView 本机智能体一键配置（issue #102）：仅回环访问。
export function AgentsView() {
  const qc = useQueryClient();
  const [apiKey, setApiKey] = useState('');
  const [baseURL, setBaseURL] = useState('http://127.0.0.1:7863');
  const [model, setModel] = useState('glm-5.2');
  const [modelSource, setModelSource] = useState<'single' | 'all'>('single');
  const [realmFilter, setRealmFilter] = useState<ModelRealmFilter>('all');
  const [busy, setBusy] = useState('');
  const { confirm, confirmDialog } = useConfirm();

  const q = useQuery({
    queryKey: ['agents'],
    queryFn: () => api<{ clients?: AgentStatus[] }>('agents'),
  });
  const clients = q.data?.clients || [];

  // all 模式才拉模型清单（与 ModelsView 同 key，复用缓存）。
  const modelsQ = useQuery({
    queryKey: ['models'],
    queryFn: () => api<{ models?: ModelEntry[] }>('models'),
    enabled: modelSource === 'all',
  });
  const ids = modelSource === 'all' ? selectModelIDs(modelsQ.data?.models, realmFilter) : [];
  const counts = countByRealm(ids);
  // all 模式清单读取失败时禁用写入：宁可写不进去，也不要写一份空模型清单。
  const blocked = modelSource === 'all' && (modelsQ.isError || ids.length === 0);

  const apply = async (id: string) => {
    if (!apiKey.trim()) {
      toast('请填写 API key（只写入本机客户端配置）', 'err');
      return;
    }
    if (modelSource === 'all' && ids.length === 0) {
      toast('模型清单为空，可切回单个模型', 'err');
      return;
    }
    setBusy(id);
    try {
      // all 模式只传 models（第一个元素即默认模型，model 不传）。
      const body =
        modelSource === 'all'
          ? { api_key: apiKey.trim(), base_url: baseURL.trim(), models: ids }
          : { api_key: apiKey.trim(), base_url: baseURL.trim(), model: model.trim() };
      await postJSON('agents/' + id + '/apply', body);
      toast('已写入配置并备份原件', 'ok');
      await qc.invalidateQueries({ queryKey: ['agents'] });
    } catch (e) {
      toast('写入失败：' + (e as Error).message, 'err');
    } finally {
      setBusy('');
    }
  };

  const restore = async (c: AgentStatus) => {
    if (
      !(await confirm({
        title: '还原「' + c.name + '」？',
        description: '还原到首次写入前，网关新建的文件会被删除。',
        confirmText: '还原',
        destructive: true,
      }))
    )
      return;
    setBusy(c.id);
    try {
      await postJSON('agents/' + c.id + '/restore', {});
      toast('已还原', 'ok');
    } catch (e) {
      const msg = (e as Error).message;
      if (msg.includes('修改')) {
        if (
          !(await confirm({
            title: '仍要强制还原？',
            description: msg,
            confirmText: '强制还原',
            destructive: true,
          }))
        )
          return;
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
            <FieldDescription>填网关根地址；Codex / OpenCode 会自动补 /v1</FieldDescription>
          </Field>
          <Field>
            <FieldLabel>API key</FieldLabel>
            <Input type="password" value={apiKey} onChange={(e) => setApiKey(e.target.value)} placeholder="config.json 中的 api_key" />
          </Field>
          <Field>
            <FieldLabel>模型来源</FieldLabel>
            <Select
              items={selectOptionItems(MODEL_SOURCE_OPTIONS)}
              value={modelSource}
              onValueChange={(v) => setModelSource(v === 'all' ? 'all' : 'single')}>
              <SelectTrigger aria-label="模型来源">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                <SelectOptions options={MODEL_SOURCE_OPTIONS} />
              </SelectContent>
            </Select>
            <FieldDescription>单个模型 = 现状；全部上游模型 = 一次写入 /panel/api/models 的清单</FieldDescription>
          </Field>
          {modelSource === 'single' ? (
            <Field>
              <FieldLabel>默认模型</FieldLabel>
              <Input value={model} onChange={(e) => setModel(e.target.value)} placeholder="glm-5.2" />
              <FieldDescription>写进各客户端的默认 model</FieldDescription>
            </Field>
          ) : (
            <>
              <Field>
                <FieldLabel>域过滤</FieldLabel>
                <Select
                  items={selectOptionItems(REALM_FILTER_OPTIONS)}
                  value={realmFilter}
                  onValueChange={(v) => setRealmFilter(v === 'cn' || v === 'global' ? v : 'all')}>
                  <SelectTrigger aria-label="域过滤">
                    <SelectValue />
                  </SelectTrigger>
                  <SelectContent>
                    <SelectOptions options={REALM_FILTER_OPTIONS} />
                  </SelectContent>
                </Select>
                <FieldDescription>
                  {modelsQ.isError
                    ? '读取模型清单失败：' + (modelsQ.error as Error).message + '，可切回单个模型'
                    : '将写入 ' + ids.length + ' 个模型（CN ' + counts.cn + ' · Global ' + counts.global + '）'}
                </FieldDescription>
              </Field>
              {modelsQ.isError ? null : (
                <p className="text-2xs text-muted-foreground md:col-span-1">
                  第一个模型即默认模型；清单上限 {MAX_IMPORT_MODELS} 个，超出仅写入前 {MAX_IMPORT_MODELS} 个。
                </p>
              )}
            </>
          )}
        </CardContent>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle>本机客户端</CardTitle>
          <CardDescription>写入前备份首次原件；还原按备份恢复。</CardDescription>
        </CardHeader>
        <CardContent className="flex flex-col gap-3">
          {q.error ? (
            <LoadError
              variant={q.data === undefined ? 'page' : 'inline'}
              message={(q.data === undefined ? '读取失败：' : '刷新失败：') + (q.error as Error).message}
              onRetry={() => void q.refetch()}
            />
          ) : null}
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
                <div className="text-2xs text-muted-foreground">{CLIENT_MODEL_SUPPORT[c.id] || ''}</div>
              </div>
              <Button size="xs" disabled={busy === c.id || blocked} onClick={() => void apply(c.id)}>
                写入
              </Button>
              <Button size="xs" variant="ghost" disabled={!c.applied || busy === c.id} onClick={() => void restore(c)}>
                还原
              </Button>
            </div>
          ))}
          <p className="text-2xs text-muted-foreground">
            API key 直接写入各客户端配置（Codex 使用 <code>experimental_bearer_token</code>），无需额外导出环境变量。
          </p>
        </CardContent>
      </Card>
      {confirmDialog}
    </>
  );
}
