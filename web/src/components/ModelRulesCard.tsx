import { useState } from 'react';
import { useQuery } from '@tanstack/react-query';
import { api } from '@/lib/api';
import type { Account, ModelRule, ModelRuleMode, ModelRulesView } from '@/lib/types';
import { Button } from '@/components/ui/button';
import { Card, CardAction, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card';
import { Input } from '@/components/ui/input';
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from '@/components/ui/select';
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table';
import { Tag } from '@/components/ui-kit';
import { toast } from '@/components/Toaster';
import { cn } from '@/lib/utils';

interface RuleDraft {
  id?: string;
  name: string;
  mode: ModelRuleMode;
  models: string;
}

const emptyDraft: RuleDraft = { name: '', mode: 'deny', models: '' };

// parseModels 把逗号 / 空白分隔的输入拆成模型名列表（面板侧只做拆分，校验在服务端）。
const parseModels = (raw: string): string[] =>
  raw
    .split(/[\s,，]+/)
    .map((s) => s.trim())
    .filter(Boolean);

// ModelRulesCard 模型规则（issue #569）：按账号批量挂模型黑白名单。
// 规则本体与账号引用都由服务端持久化，这里只做编辑与挂载，不维护第二份状态。
export function ModelRulesCard({ accounts }: { accounts: Account[] }) {
  const [draft, setDraft] = useState<RuleDraft>(emptyDraft);
  const [batchRule, setBatchRule] = useState('');
  const [busy, setBusy] = useState(false);

  const rulesQ = useQuery({
    queryKey: ['model_rules'],
    queryFn: () =>
      api<ModelRulesView>('model_rules').catch(
        (): ModelRulesView => ({ rules: [], assignments: {}, counts: {} }),
      ),
  });

  const rules = rulesQ.data?.rules ?? [];
  const assignments = rulesQ.data?.assignments ?? {};
  const counts = rulesQ.data?.counts ?? {};

  // run 统一收口：按钮禁用、错误 toast、成功后刷新规则视图。
  const run = async (call: () => Promise<unknown>, okMsg: string): Promise<boolean> => {
    setBusy(true);
    try {
      await call();
      toast(okMsg, 'ok');
      await rulesQ.refetch();
      return true;
    } catch (e) {
      toast((e as Error).message, 'err');
      return false;
    } finally {
      setBusy(false);
    }
  };

  const save = async () => {
    const ok = await run(
      () =>
        api('model_rules', {
          method: 'POST',
          body: JSON.stringify({
            id: draft.id,
            name: draft.name,
            mode: draft.mode,
            models: parseModels(draft.models),
          }),
        }),
      draft.id ? '规则已更新' : '规则已创建',
    );
    if (ok) setDraft(emptyDraft);
  };

  const remove = (rule: ModelRule) =>
    void run(() => api('model_rules/' + encodeURIComponent(rule.id) + '/delete', { method: 'POST' }), '规则已删除');

  // toggleRule 账号维度整体替换：把该账号的规则列表改为当前列表 ± 这一条。
  const toggleRule = (uid: string, ruleID: string, attached: boolean) => {
    const next = (assignments[uid] ?? []).filter((id) => id !== ruleID);
    if (!attached) next.push(ruleID);
    void run(
      () =>
        api('model_rules/assign', {
          method: 'POST',
          body: JSON.stringify({ uids: [uid], rule_ids: next, action: 'replace' }),
        }),
      attached ? '已摘除规则' : '已挂载规则',
    );
  };

  // batch 批量挂载 / 摘除同一规则到全部账号。
  const batch = (action: 'attach' | 'detach') => {
    if (!batchRule) return;
    void run(
      () =>
        api('model_rules/assign', {
          method: 'POST',
          body: JSON.stringify({
            uids: accounts.map((a) => a.uid),
            rule_ids: [batchRule],
            action,
          }),
        }),
      action === 'attach' ? '已批量挂载' : '已批量摘除',
    );
  };

  const edit = (rule: ModelRule) =>
    setDraft({ id: rule.id, name: rule.name ?? '', mode: rule.mode, models: rule.models.join(' ') });

  return (
    <Card className="mb-4">
      <CardHeader className="flex flex-wrap items-center justify-between gap-3">
        <div className="min-w-0">
          <CardTitle>模型规则</CardTitle>
          <CardDescription>
            按账号批量挂模型黑白名单：deny 优先于 allow；模型名可带 `cn:` / `global:` 前缀，裸名两域都命中。
            改规则内容后所有挂载账号下一请求即生效。
          </CardDescription>
        </div>
        <CardAction>
          <div className="flex flex-wrap items-center gap-2">
            <Select
              value={batchRule}
              onValueChange={(v) => setBatchRule(v ?? '')}
              items={rules.map((r) => ({ value: r.id, label: r.name || r.id }))}>
              <SelectTrigger size="sm" aria-label="选择批量规则">
                <SelectValue placeholder="选择规则" />
              </SelectTrigger>
              <SelectContent>
                {rules.map((r) => (
                  <SelectItem key={r.id} value={r.id}>
                    {r.name || r.id}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
            <Button size="xs" variant="ghost" disabled={busy || !batchRule} onClick={() => batch('attach')}>
              全部账号挂载
            </Button>
            <Button size="xs" variant="ghost" disabled={busy || !batchRule} onClick={() => batch('detach')}>
              全部账号摘除
            </Button>
          </div>
        </CardAction>
      </CardHeader>

      <CardContent className="space-y-4">
        <div className="flex flex-wrap items-end gap-2">
          <label className="flex min-w-[140px] flex-1 flex-col gap-1 text-2xs text-muted-foreground">
            名称
            <Input
              className="h-7 text-xs"
              value={draft.name}
              placeholder="例如：昂贵模型只给主力号"
              onChange={(e) => setDraft((d) => ({ ...d, name: e.target.value }))}
            />
          </label>
          <label className="flex w-[120px] flex-col gap-1 text-2xs text-muted-foreground">
            模式
            <Select
              value={draft.mode}
              onValueChange={(v) => setDraft((d) => ({ ...d, mode: (v ?? 'deny') as ModelRuleMode }))}>
              <SelectTrigger size="sm" aria-label="规则模式">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value="deny">黑名单 deny</SelectItem>
                <SelectItem value="allow">白名单 allow</SelectItem>
              </SelectContent>
            </Select>
          </label>
          <label className="flex min-w-[220px] flex-[2] flex-col gap-1 text-2xs text-muted-foreground">
            模型（空格 / 逗号分隔，可带 cn: / global: 前缀）
            <Input
              className="h-7 text-xs"
              value={draft.models}
              placeholder="glm-5.2 global:kimi-k3"
              onChange={(e) => setDraft((d) => ({ ...d, models: e.target.value }))}
            />
          </label>
          <Button size="xs" disabled={busy || parseModels(draft.models).length === 0} onClick={() => void save()}>
            {draft.id ? '保存修改' : '新建规则'}
          </Button>
          {draft.id ? (
            <Button size="xs" variant="ghost" disabled={busy} onClick={() => setDraft(emptyDraft)}>
              取消编辑
            </Button>
          ) : null}
        </div>

        {rules.length === 0 ? (
          <div className="py-6 text-center text-xs text-muted-foreground">还没有模型规则</div>
        ) : (
          <>
            <div className="overflow-x-auto">
              <Table className="min-w-[560px]">
                <TableHeader>
                  <TableRow>
                    <TableHead>规则</TableHead>
                    <TableHead>模式</TableHead>
                    <TableHead>模型</TableHead>
                    <TableHead className="w-[80px]">账号数</TableHead>
                    <TableHead className="w-[120px]">操作</TableHead>
                  </TableRow>
                </TableHeader>
                <TableBody>
                  {rules.map((rule) => (
                    <TableRow key={rule.id}>
                      <TableCell>
                        <div className="text-xs font-medium">{rule.name || rule.id}</div>
                        <div className="font-mono text-2xs text-muted-foreground">{rule.id}</div>
                      </TableCell>
                      <TableCell>
                        <Tag tone={rule.mode === 'deny' ? 'bad' : 'ok'}>{rule.mode}</Tag>
                      </TableCell>
                      <TableCell className="font-mono text-2xs">{rule.models.join(' ')}</TableCell>
                      <TableCell className="tabular-nums">{counts[rule.id] ?? 0}</TableCell>
                      <TableCell>
                        <div className="flex gap-1">
                          <Button size="xs" variant="ghost" disabled={busy} onClick={() => edit(rule)}>
                            编辑
                          </Button>
                          <Button size="xs" variant="ghost" disabled={busy} onClick={() => void remove(rule)}>
                            删除
                          </Button>
                        </div>
                      </TableCell>
                    </TableRow>
                  ))}
                </TableBody>
              </Table>
            </div>

            <div className="overflow-x-auto">
              <Table className="min-w-[560px]">
                <TableHeader>
                  <TableRow>
                    <TableHead className="w-[200px]">账号</TableHead>
                    <TableHead>已挂规则（点击切换）</TableHead>
                  </TableRow>
                </TableHeader>
                <TableBody>
                  {accounts.map((account) => {
                    const attached = assignments[account.uid] ?? [];
                    return (
                      <TableRow key={account.uid}>
                        <TableCell>
                          <div className="text-xs">{account.nickname || account.uid}</div>
                          <div className="text-2xs text-muted-foreground">
                            {account.realm || 'cn'}
                            {account.disabled ? ' · 已禁用' : account.paused ? ' · 已暂停选号' : ''}
                          </div>
                        </TableCell>
                        <TableCell>
                          <div className="flex flex-wrap gap-1">
                            {rules.map((rule) => {
                              const on = attached.includes(rule.id);
                              return (
                                <button
                                  key={rule.id}
                                  type="button"
                                  disabled={busy}
                                  onClick={() => toggleRule(account.uid, rule.id, on)}
                                  className={cn(
                                    'rounded border px-1.5 py-0.5 text-2xs',
                                    on
                                      ? 'border-primary bg-primary/10 text-primary'
                                      : 'border-border text-muted-foreground hover:text-foreground',
                                  )}>
                                  {(on ? '✓ ' : '+ ') + (rule.name || rule.id)}
                                </button>
                              );
                            })}
                          </div>
                        </TableCell>
                      </TableRow>
                    );
                  })}
                </TableBody>
              </Table>
            </div>
          </>
        )}
      </CardContent>
    </Card>
  );
}
