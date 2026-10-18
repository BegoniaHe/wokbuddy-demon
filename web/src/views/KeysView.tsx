import { useState } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { MoreHorizontal } from 'lucide-react';
import { api, postJSON } from '@/lib/api';
import { ago, fmtTok } from '@/lib/format';
import { toast } from '@/components/Toaster';
import { useConfirm } from '@/components/ConfirmDialog';
import { LoadError } from '@/components/LoadError';
import type { ApiKey, ApiKeyLimits } from '@/lib/types';
import { Button } from '@/components/ui/button';
import { Field, FieldLabel, FieldDescription } from '@/components/ui/field';
import { Input } from '@/components/ui/input';
import {
  Select,
  SelectContent,
  SelectOptions,
  SelectTrigger,
  SelectValue,
  selectOptionItems,
  type SelectOption,
} from '@/components/ui/select';
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuTrigger,
} from '@/components/ui/dropdown-menu';
import {
  Card,
  CardContent,
  CardAction,
  CardHeader,
  CardTitle,
} from '@/components/ui/card';
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from '@/components/ui/table';
import {
  Dialog,
  DialogContent,
  DialogHeader,
  DialogTitle,
  DialogDescription,
  DialogFooter,
} from '@/components/ui/dialog';

const REALM_OPTIONS = [
  { value: 'auto', label: '跟随模型前缀 / 全局开关' },
  { value: 'cn', label: '固定 cn' },
  { value: 'global', label: '固定 global' },
] as const satisfies readonly SelectOption[];

// KeysView：命名 API 密钥管理。命名密钥只用于网关 /v1/*（chat/models）；
// 面板与 /status、/metrics 仍只认 config.json 的默认 api_key。
export function KeysView() {
  const qc = useQueryClient();
  const [createOpen, setCreateOpen] = useState(false);
  const [name, setName] = useState('');
  const [rawKey, setRawKey] = useState('');
  const [limitsFor, setLimitsFor] = useState<ApiKey | null>(null);
  const [rpm, setRpm] = useState('');
  const [tpm, setTpm] = useState('');
  const [concurrent, setConcurrent] = useState('');
  const [models, setModels] = useState('');
  const [realm, setRealm] = useState<'' | 'cn' | 'global'>('');
  const { confirm, confirmDialog } = useConfirm();

  const keysQ = useQuery({
    queryKey: ['keys'],
    queryFn: () => api<{ keys?: ApiKey[] }>('keys'),
  });
  const list = keysQ.data?.keys ?? [];

  const invalidate = () => qc.invalidateQueries({ queryKey: ['keys'] });

  const createM = useMutation({
    mutationFn: (n: string) => postJSON<{ key: string; record: ApiKey }>('keys', { name: n }),
    onSuccess: (r) => {
      setRawKey(r.key);
      void invalidate();
    },
    onError: (e) => toast('创建失败：' + (e as Error).message, 'err'),
  });

  const revokeM = useMutation({
    mutationFn: (id: string) => postJSON('keys/' + id + '/revoke'),
    onSuccess: () => {
      toast('已撤销，该密钥立即失效', 'ok');
      void invalidate();
    },
    onError: (e) => toast('撤销失败：' + (e as Error).message, 'err'),
  });

  const deleteM = useMutation({
    mutationFn: (id: string) => postJSON('keys/' + id + '/delete'),
    onSuccess: () => {
      toast('已删除', 'ok');
      void invalidate();
    },
    onError: (e) => toast('删除失败：' + (e as Error).message, 'err'),
  });

  const limitsM = useMutation({
    mutationFn: async (v: { id: string; realm: '' | 'cn' | 'global'; limits: ApiKeyLimits }) => {
      await postJSON('keys/' + v.id + '/realm', { realm: v.realm });
      return postJSON('keys/' + v.id + '/limits', { limits: v.limits });
    },
    onSuccess: () => {
      toast('设置已保存，下一个请求生效', 'ok');
      setLimitsFor(null);
      void invalidate();
    },
    onError: (e) => toast('保存限制失败：' + (e as Error).message, 'err'),
  });

  const openLimits = (k: ApiKey) => {
    setLimitsFor(k);
    setRpm(String(k.limits?.rpm ?? ''));
    setTpm(String(k.limits?.tpm ?? ''));
    setConcurrent(String(k.limits?.max_concurrent ?? ''));
    setModels((k.limits?.models ?? []).join(', '));
    setRealm(k.realm ?? '');
  };

  const saveLimits = () => {
    if (!limitsFor) return;
    const parseLimit = (v: string): number | null => {
      const trimmed = v.trim();
      if (trimmed === '') return 0;
      if (!/^\d+$/.test(trimmed)) return null;
      const n = Number(trimmed);
      return Number.isSafeInteger(n) ? n : null;
    };
    const nextRpm = parseLimit(rpm);
    const nextTpm = parseLimit(tpm);
    const nextConcurrent = parseLimit(concurrent);
    if (nextRpm === null || nextTpm === null || nextConcurrent === null) {
      toast('限制必须是非负整数', 'err');
      return;
    }
    limitsM.mutate({
      id: limitsFor.id,
      realm,
      limits: {
        rpm: nextRpm,
        tpm: nextTpm,
        max_concurrent: nextConcurrent,
        models: models.split(/[,，\s]+/).filter(Boolean),
      },
    });
  };

  const limitsText = (k: ApiKey) => {
    const l = k.limits;
    if (!l) return '—';
    const parts: string[] = [];
    if (l.rpm) parts.push('RPM ' + l.rpm);
    if (l.tpm) parts.push('TPM ' + l.tpm);
    if (l.max_concurrent) parts.push('并发 ' + l.max_concurrent);
    if (l.models?.length) parts.push(l.models.join(', '));
    return parts.length ? parts.join(' · ') : '—';
  };

  const openCreate = () => {
    setName('');
    setRawKey('');
    setCreateOpen(true);
  };

  const copyKey = (k: string) =>
    void navigator.clipboard.writeText(k).then(
      () => toast('密钥已复制', 'ok'),
      () => toast('复制失败，请手动选择复制', 'err'),
    );

  return (
    <>
      <Card>
        <CardHeader className="flex flex-wrap items-center justify-between gap-3">
          <CardTitle>命名密钥</CardTitle>
          <CardAction>
            <div className="flex flex-wrap items-center gap-3">
              <span className="text-xs text-muted-foreground">仅用于 /v1/*（chat / models）；面板与 /status、/metrics 仍只认默认 api_key</span>
              <Button onClick={openCreate}>创建密钥</Button>
            </div>
          </CardAction>
        </CardHeader>
        {keysQ.data !== undefined && keysQ.error ? (
          <LoadError
            className="mx-3 mt-3"
            message={'刷新失败：' + (keysQ.error as Error).message}
            onRetry={() => void keysQ.refetch()}
          />
        ) : null}
        {keysQ.isLoading ? (
          <CardContent className="py-10 text-center text-muted-foreground">加载中…</CardContent>
        ) : keysQ.error && keysQ.data === undefined ? (
          <CardContent>
            <LoadError
              variant="page"
              message={'读取失败：' + (keysQ.error as Error).message}
              onRetry={() => void keysQ.refetch()}
            />
          </CardContent>
        ) : list.length === 0 ? (
          <CardContent className="py-10 text-center text-muted-foreground">暂无命名密钥。创建后即可分发给不同客户端，并在此查看各自的请求与 token 用量。</CardContent>
        ) : (
          <div className="overflow-x-auto">
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>名称</TableHead>
                  <TableHead>前缀</TableHead>
                  <TableHead className="text-right">请求数</TableHead>
                  <TableHead className="text-right">Token</TableHead>
                  <TableHead className="text-right">拒绝</TableHead>
                  <TableHead>出口</TableHead>
                  <TableHead>限制</TableHead>
                  <TableHead>创建</TableHead>
                  <TableHead>最近使用</TableHead>
                  <TableHead>状态</TableHead>
                  <TableHead className="text-right">操作</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {list.map((k) => (
                  <TableRow key={k.id}>
                    <TableCell>{k.name}</TableCell>
                    <TableCell className="font-mono text-muted-foreground">{k.prefix}…</TableCell>
                    <TableCell className="text-right">
                      <span className="font-mono tabular-nums">{k.requests ?? 0}</span>
                    </TableCell>
                    <TableCell className="text-right">
                      <span className="font-mono tabular-nums">{fmtTok(k.total_tokens ?? 0)}</span>
                    </TableCell>
                    <TableCell className="text-right">
                      <span className="font-mono tabular-nums">{k.rejected ?? 0}</span>
                    </TableCell>
                    <TableCell className="text-muted-foreground">{k.realm || '跟随模型'}</TableCell>
                    <TableCell className="text-muted-foreground">{limitsText(k)}</TableCell>
                    <TableCell className="text-muted-foreground">{ago(k.created_at)}</TableCell>
                    <TableCell className="text-muted-foreground">{k.last_used_at ? ago(k.last_used_at) : '—'}</TableCell>
                    <TableCell>
                      {k.revoked ? <span className="text-destructive">已撤销</span> : <span className="text-success">有效</span>}
                    </TableCell>
                    <TableCell className="text-right">
                      {k.revoked ? null : (
                        <>
                          <Button size="xs" variant="ghost" onClick={() => openLimits(k)}>
                            限制
                          </Button>
                          <Button
                            size="xs"
                            variant="ghost"
                            disabled={revokeM.isPending}
                            onClick={() => revokeM.mutate(k.id)}
                          >
                            撤销
                          </Button>
                        </>
                      )}
                      <DropdownMenu>
                        <DropdownMenuTrigger
                          render={
                            <Button size="xs" variant="ghost" title="更多操作">
                              <MoreHorizontal />
                            </Button>
                          }
                        />
                        <DropdownMenuContent align="end">
                          <DropdownMenuItem variant="destructive"
                            disabled={deleteM.isPending}
                            onClick={async () => {
                              if (
                                !(await confirm({
                                  title: '删除密钥「' + k.name + '」？',
                                  description: '将同时清除其用量记录，且不可恢复。',
                                  confirmText: '删除',
                                  destructive: true,
                                }))
                              )
                                return;
                              deleteM.mutate(k.id);
                            }}
                          >
                            删除
                          </DropdownMenuItem>
                        </DropdownMenuContent>
                      </DropdownMenu>
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          </div>
        )}
      </Card>

      <Dialog open={createOpen} onOpenChange={setCreateOpen}>
        <DialogContent className="sm:max-w-md" showCloseButton={false}>
          <DialogHeader>
            <DialogTitle>创建命名密钥</DialogTitle>
            <DialogDescription>密钥明文仅创建时显示一次，请立即复制保存。</DialogDescription>
          </DialogHeader>
          {rawKey ? (
            <>
              <Field>
                <FieldLabel>密钥（仅此一次显示）</FieldLabel>
                <Input readOnly value={rawKey} className="font-mono" onFocus={(e) => e.target.select()} />
              </Field>
              <p className="text-2xs text-warning">关闭后无法再次查看。如丢失，请撤销后重新创建。</p>
            </>
          ) : (
            <Field>
              <FieldLabel>名称</FieldLabel>
              <Input
                placeholder="客户端名称"
                value={name}
                onChange={(e) => setName(e.target.value)}
                onKeyDown={(e) => {
                  if (e.key === 'Enter') createM.mutate(name);
                }}
              />
              <FieldDescription>用于区分客户端，例如「笔记本」「手机」。</FieldDescription>
            </Field>
          )}
          <DialogFooter>
            {rawKey ? (
              <>
                <Button onClick={() => copyKey(rawKey)}>复制密钥</Button>
                <Button variant="default" onClick={() => setCreateOpen(false)}>
                  完成
                </Button>
              </>
            ) : (
              <>
                <Button onClick={() => setCreateOpen(false)}>取消</Button>
                <Button variant="default" disabled={createM.isPending} onClick={() => createM.mutate(name)}>
                  {createM.isPending ? '创建中…' : '创建'}
                </Button>
              </>
            )}
          </DialogFooter>
        </DialogContent>
      </Dialog>

      <Dialog open={!!limitsFor} onOpenChange={(open) => !open && setLimitsFor(null)}>
        <DialogContent className="sm:max-w-md">
          <DialogHeader>
            <DialogTitle>限制「{limitsFor?.name}」</DialogTitle>
            <DialogDescription>0 或留空 = 不限制；保存后下一个请求生效。模型白名单支持 glob 与 cn:/global: 前缀。</DialogDescription>
          </DialogHeader>
          <Field>
            <FieldLabel>出口绑定</FieldLabel>
            <Select
              items={selectOptionItems(REALM_OPTIONS)}
              value={realm || 'auto'}
              onValueChange={(v) => setRealm((v === 'auto' ? '' : v) as '' | 'cn' | 'global')}>
              <SelectTrigger aria-label="出口绑定">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                <SelectOptions options={REALM_OPTIONS} />
              </SelectContent>
            </Select>
            <FieldDescription>固定后该密钥只走对应域；与请求的显式 cn:/global: 前缀冲突时返回 400。</FieldDescription>
          </Field>
          <div className="grid grid-cols-2 gap-3">
            <Field>
              <FieldLabel>RPM</FieldLabel>
              <Input inputMode="numeric" placeholder="每分钟请求数" value={rpm} onChange={(e) => setRpm(e.target.value)} />
            </Field>
            <Field>
              <FieldLabel>TPM</FieldLabel>
              <Input inputMode="numeric" placeholder="每分钟 token" value={tpm} onChange={(e) => setTpm(e.target.value)} />
            </Field>
            <Field>
              <FieldLabel>最大并发</FieldLabel>
              <Input inputMode="numeric" placeholder="并发请求数" value={concurrent} onChange={(e) => setConcurrent(e.target.value)} />
            </Field>
          </div>
          <Field>
            <FieldLabel>模型白名单</FieldLabel>
            <Input placeholder="cn:glm-*, gpt-5*" value={models} onChange={(e) => setModels(e.target.value)} />
            <FieldDescription>逗号或空格分隔；留空 = 全部模型。</FieldDescription>
          </Field>
          <DialogFooter>
            <Button variant="ghost" onClick={() => setLimitsFor(null)}>取消</Button>
            <Button disabled={limitsM.isPending} onClick={saveLimits}>保存</Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
      {confirmDialog}
    </>
  );
}
