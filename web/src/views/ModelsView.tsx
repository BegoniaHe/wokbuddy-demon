import { useEffect, useMemo, useState } from 'react';
import { useQuery } from '@tanstack/react-query';
import { api } from '@/lib/api';
import { mdMatch, mdSortList, DEFAULT_MODEL_FILTER, type ModelEntry, type ModelFilter } from '@/lib/models';
import { Button } from '@/components/ui/button';
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
  Card,
  CardAction,
  CardDescription,
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
import { Tag } from '@/components/ui-kit';
import { toast } from '@/components/Toaster';
import { Input } from '@/components/ui/input';
import { cn } from '@/lib/utils';

interface Probe {
  claimed?: number;
  measured?: number;
  note?: string;
  tested_at?: string;
  verdict?: string;
}

// 面板主动探测的健康聚合（GET /panel/api/model_health，issue #110）。
interface ModelHealth {
  last?: { at?: string; ok?: boolean; ttft_ms?: number; error_class?: string; error?: string };
  success?: number;
  failure?: number;
  avg_ttft_ms?: number;
  p50_ttft_ms?: number;
  p95_ttft_ms?: number;
  last_probe_at?: string;
  window?: number;
}

const fmtK = (n: unknown) => {
  const v = Number(n || 0);
  return v >= 1000 ? Math.round(v / 1000) + 'K' : String(v);
};

function probeDays(ts?: string): number | null {
  if (!ts) return null;
  const t = new Date(String(ts).replace(' ', 'T'));
  const d = (Date.now() - t.getTime()) / 86400000;
  return isNaN(d) ? null : Math.floor(d);
}

// FilterSelect：工具栏筛选下拉。options 同时驱动 items 映射与列表项；
// value 为空串时回落 options[0]（即「全部…」项），以空串表示不筛选。
function FilterSelect({
  options,
  value,
  onChange,
  ariaLabel,
}: {
  options: readonly SelectOption[];
  value: string;
  onChange: (v: string) => void;
  ariaLabel?: string;
}) {
  const fallback = options[0]?.value ?? '';
  return (
    <Select
      items={selectOptionItems(options)}
      value={value || fallback}
      onValueChange={(v) => onChange(v === fallback ? '' : (v ?? ''))}>
      <SelectTrigger size="sm" aria-label={ariaLabel}>
        <SelectValue />
      </SelectTrigger>
      <SelectContent>
        <SelectOptions options={options} />
      </SelectContent>
    </Select>
  );
}

export function ModelsView() {
  const [filter, setFilter] = useState<ModelFilter>(DEFAULT_MODEL_FILTER);
  const [q, setQ] = useState('');

  useEffect(() => {
    const t = setTimeout(() => setFilter((f) => ({ ...f, q: q.trim() })), 120);
    return () => clearTimeout(t);
  }, [q]);

  const modelsQ = useQuery({
    queryKey: ['models'],
    queryFn: () => api<{ models?: ModelEntry[] }>('models'),
  });
  const probesQ = useQuery({
    queryKey: ['model_probes'],
    queryFn: () =>
      api<{ probes?: Record<string, Probe> }>('model_probes').catch(
        (): { probes?: Record<string, Probe> } => ({ probes: {} }),
      ),
  });
  const healthQ = useQuery({
    queryKey: ['model_health'],
    queryFn: () =>
      api<{ models?: Record<string, ModelHealth> }>('model_health').catch(
        (): { models?: Record<string, ModelHealth> } => ({ models: {} }),
      ),
  });
  const [probing, setProbing] = useState<string | null>(null);
  const reload = () => {
    void modelsQ.refetch();
    void probesQ.refetch();
    void healthQ.refetch();
  };
  const probe = async (entry: ModelEntry) => {
    const i = entry.id.indexOf(':');
    const realm = i > 0 ? entry.id.slice(0, i) : 'cn';
    const model = i > 0 ? entry.id.slice(i + 1) : entry.id;
    setProbing(entry.id);
    try {
      const r = await api<{ ok?: boolean; sample?: { ttft_ms?: number; error_class?: string; error?: string } }>(
        'model_probes',
        { method: 'POST', body: JSON.stringify({ model, realm }) },
      );
      if (r.ok) {
        toast('探测成功：首字 ' + (r.sample?.ttft_ms ?? '?') + 'ms', 'ok');
      } else {
        toast('探测失败：' + (r.sample?.error_class || r.sample?.error || '未知错误'), 'err');
      }
      void healthQ.refetch();
    } catch (e) {
      toast((e as Error).message, 'err');
    } finally {
      setProbing(null);
    }
  };

  const data = modelsQ.data;
  const isFetching = modelsQ.isFetching;
  const error = modelsQ.error;
  const all = data?.models ?? [];
  const probes = probesQ.data?.probes ?? {};
  const health = healthQ.data?.models ?? {};
  const probeKeys = Object.keys(probes);
  const probeOf = (id: string): Probe | undefined =>
    probes[id] || probes[probeKeys.find((k) => k.endsWith(':' + id)) ?? ''];

  const list = useMemo(() => mdSortList(all.filter((m) => mdMatch(m, filter)), filter), [all, filter]);
  const filtered = list.length !== all.length;
  const hit = all.filter((m) => probeOf(m.id)).length;

  const note = !all.length
    ? isFetching
      ? '正在向上游查询…'
      : '上游未返回模型'
    : all.length + ' 个模型 · 已刷新降级缓存' + (hit ? ' · ' + hit + ' 个有实测上限' : '');

  return (
    <Card>
      <CardHeader className="flex flex-wrap items-center justify-between gap-3">
        <CardTitle className="flex items-baseline">
          模型能力
          <CardDescription className="ml-1.5">max_tokens bug 已修复目前可以正常透传最大输出，但是官方返回的最大输出仅为参考，不保证完全正确</CardDescription>
        </CardTitle>
        <CardAction>
          <div className="flex flex-wrap items-center gap-3">
            <span className="text-xs text-muted-foreground">{note}</span>
            <Button size="xs" disabled={isFetching} onClick={reload}>
              重新获取
            </Button>
          </div>
        </CardAction>
      </CardHeader>

      <div className="flex flex-wrap items-center gap-2 border-y border-border/60 bg-secondary px-4 py-2.5">
        <Input
          type="search"
          className="h-7 min-w-[200px] flex-[0_1_280px] text-xs"
          placeholder="搜索模型 ID / 名称 / 描述 / 厂商"
          aria-label="搜索模型"
          value={q}
          onChange={(e) => setQ(e.target.value)}
        />
        <FilterSelect
          options={[
            { value: '__all__', label: '全部域' },
            { value: 'cn', label: '国内版 CN' },
            { value: 'global', label: '国际版 Global' },
          ]}
          ariaLabel="按域筛选"
          value={filter.realm}
          onChange={(v) => setFilter((f) => ({ ...f, realm: v }))} />
        <FilterSelect
          options={[
            { value: '__all__', label: '全部能力' },
            { value: 'tool', label: '支持工具调用' },
            { value: 'vision', label: '支持视觉' },
            { value: 'reasoning', label: '支持思考' },
            { value: 'default', label: '默认模型' },
          ]}
          ariaLabel="按能力筛选"
          value={filter.cap}
          onChange={(v) => setFilter((f) => ({ ...f, cap: v }))} />
        <FilterSelect
          options={[
            { value: '__all__', label: '全部档位' },
            { value: 'off', label: '可关闭思考' },
            { value: 'low', label: 'low' },
            { value: 'medium', label: 'medium' },
            { value: 'high', label: 'high' },
            { value: 'xhigh', label: 'xhigh' },
            { value: 'max', label: 'max' },
          ]}
          ariaLabel="按思考档位筛选"
          value={filter.effort}
          onChange={(v) => setFilter((f) => ({ ...f, effort: v }))} />
        <FilterSelect
          options={[
            { value: '__all__', label: '全部价格' },
            { value: 'promo', label: '有折扣 / 限时免费' },
            { value: 'free', label: '限时免费' },
            { value: 'discount', label: '打折（非免费）' },
          ]}
          ariaLabel="按价格筛选"
          value={filter.promo}
          onChange={(v) => setFilter((f) => ({ ...f, promo: v }))} />
        <FilterSelect
          options={[
            { value: 'default', label: '上游默认顺序' },
            { value: 'rate', label: '积分倍率 低 → 高' },
            { value: 'context', label: '上下文长度 大 → 小' },
            { value: 'output', label: '最大输出 大 → 小' },
            { value: 'name', label: '模型 ID A → Z' },
          ]}
          ariaLabel="排序方式"
          value={filter.sort}
          onChange={(v) => setFilter((f) => ({ ...f, sort: v }))} />
        <span className="flex-1" />
        <span className={cn('text-xs text-muted-foreground', filtered && 'text-warning')}>
          {!all.length ? '' : filtered ? '命中 ' + list.length + ' / ' + all.length + ' 个模型' : all.length + ' 个模型'}
        </span>
        <Button size="xs" variant="ghost" title="清空全部筛选条件" onClick={() => { setQ(''); setFilter(DEFAULT_MODEL_FILTER); }}>
          重置
        </Button>
      </div>

      <div className="overflow-x-auto">
        <Table className="min-w-[700px]">
          <TableHeader>
            <TableRow>
              <TableHead className="w-[3px] p-0" aria-hidden />
              <TableHead>模型</TableHead>
              <TableHead>积分倍率</TableHead>
              <TableHead>默认档</TableHead>
              <TableHead>支持的思考档位</TableHead>
              <TableHead>上下文长度</TableHead>
              <TableHead>最大输出</TableHead>
              <TableHead>首字延迟</TableHead>
              <TableHead className="w-[64px]">探测</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {error ? (
              <TableRow>
                <TableCell colSpan={9} className="py-10 text-center text-muted-foreground">{(error as Error).message}</TableCell>
              </TableRow>
            ) : list.length === 0 ? (
              <TableRow>
                <TableCell colSpan={9} className="py-10 text-center text-muted-foreground">{all.length ? '没有符合当前筛选条件的模型' : '正在向上游查询…'}</TableCell>
              </TableRow>
            ) : (
              list.map((m) => (
                <ModelRow
                  key={m.id}
                  m={m}
                  pr={probeOf(m.id)}
                  h={health[m.id]}
                  probing={probing === m.id}
                  onProbe={() => void probe(m)}
                />
              ))
            )}
          </TableBody>
        </Table>
      </div>
    </Card>
  );
}

function RateCell({ m }: { m: ModelEntry }) {
  const tip = m.promo_note;
  if (m.promo_factor != null && m.promo_credits) {
    return (
      <span title={tip} className="cursor-help">
        <b className="font-mono">{m.promo_credits}</b>
        {m.promo_label ? <Tag tone="ok" className="ml-1">{m.promo_label}</Tag> : null}
        {m.credits ? <s className="ml-1 text-2xs text-muted-foreground">{m.credits}</s> : null}
      </span>
    );
  }
  if (m.promo_label) {
    return (
      <span title={tip} className="cursor-help">
        {m.credits ?? '—'} <Tag tone="warn">{m.promo_label}</Tag>
      </span>
    );
  }
  return <span className="font-mono">{m.credits ?? '—'}</span>;
}

function OutCell({ m, pr }: { m: ModelEntry; pr?: Probe }) {
  if (!pr) return <span className="font-mono tabular-nums">{m.max_output_tokens ? fmtK(m.max_output_tokens) : '—'}</span>;
  const tip =
    '声称 ' +
    (pr.claimed ? fmtK(pr.claimed) : '?') +
    ' · 实测 ' +
    (pr.measured ? fmtK(pr.measured) : '?') +
    (pr.note ? ' · ' + pr.note : '') +
    (pr.tested_at ? ' · 探测于 ' + pr.tested_at : '');
  const days = probeDays(pr.tested_at);
  const stale = days !== null && days > 30 ? ' · ' + days + ' 天前' : '';
  if (pr.verdict === 'clamped' && pr.measured) {
    if (pr.claimed && pr.measured < pr.claimed) {
      const x = pr.claimed / pr.measured;
      const xs = (x >= 10 ? Math.round(x) : Math.round(x * 10) / 10) + '×';
      return (
        <span className="font-mono tabular-nums" title={tip}>
          <span className="font-semibold text-warning">{fmtK(pr.measured)} ⚠</span>
          <div className="text-2xs text-muted-foreground">钳制 {xs}{stale}</div>
        </span>
      );
    }
    return (
      <span className="font-mono tabular-nums" title={tip}>
        <span className="text-success">
          {fmtK(pr.measured)}
          {pr.claimed && pr.measured > pr.claimed ? ' ↑' : ' ✓'}
        </span>
      </span>
    );
  }
  if (pr.verdict === 'at_least' && pr.measured)
    return (
      <span className="font-mono tabular-nums" title={tip}>
        <span className="text-muted-foreground">≥{fmtK(pr.measured)}</span>
      </span>
    );
  return (
    <span className="font-mono tabular-nums" title={tip}>
      <span className="text-muted-foreground">?</span>
      <div className="text-2xs text-muted-foreground">未测出{stale}</div>
    </span>
  );
}

function TTFTCell({ h }: { h?: ModelHealth }) {
  if (!h || (!h.last && !h.success)) return <span className="text-xs text-muted-foreground">未探测</span>;
  if (h.last && h.last.ok && h.last.ttft_ms) {
    return (
      <span className="font-mono tabular-nums" title={'最近 ' + h.last.ttft_ms + 'ms · 均值 ' + (h.avg_ttft_ms ?? '—') + 'ms · p95 ' + (h.p95_ttft_ms ?? '—') + 'ms'}>
        <b>{h.last.ttft_ms}ms</b>
        {h.avg_ttft_ms ? <div className="text-2xs text-muted-foreground">均值 {h.avg_ttft_ms}ms</div> : null}
      </span>
    );
  }
  return (
    <span className="text-2xs text-muted-foreground" title={h.last?.error || h.last?.error_class}>
      失败{h.failure ? ' ×' + h.failure : ''}
    </span>
  );
}

function ModelRow({
  m,
  pr,
  h,
  probing,
  onProbe,
}: {
  m: ModelEntry;
  pr?: Probe;
  h?: ModelHealth;
  probing: boolean;
  onProbe: () => void;
}) {
  const eff = (m.supported_efforts || []).slice();
  if (m.can_disable_thinking && eff.length && !eff.includes('off')) eff.push('off（可关）');
  const effs = eff.length ? (
    <span className="flex flex-wrap gap-1">
      {eff.map((e) => (
        <Tag key={e} tone="warn">
          {e}
        </Tag>
      ))}
    </span>
  ) : (
    <span className="text-xs text-muted-foreground">
      {m.supports_reasoning ? '固定档 · 默认 ' + (m.default_effort || '?') : '不支持思考'}
    </span>
  );
  const caps: React.ReactNode[] = [];
  if (m.is_default) caps.push(<Tag key="d" tone="ok">默认</Tag>);
  if (m.supports_tool_call) caps.push(<Tag key="t" tone="warn">工具</Tag>);
  if (m.supports_images) caps.push(<Tag key="v" tone="warn">视觉</Tag>);
  if (m.supports_reasoning && !m.can_disable_thinking) caps.push(<Tag key="r" tone="warn">思考常开</Tag>);
  return (
    <TableRow>
      <TableCell className="w-[3px] p-0">
        <span className="block h-full min-h-[36px] w-[3px] bg-success" />
      </TableCell>
      <TableCell title={m.description}>
        <div className="font-medium">{m.id}</div>
        <div className="font-mono text-2xs text-muted-foreground">{m.name || ''}</div>
        {caps.length ? <div className="mt-0.5 flex gap-1">{caps}</div> : null}
      </TableCell>
      <TableCell>
        <RateCell m={m} />
      </TableCell>
      <TableCell>{m.default_effort ? <Tag tone="ok">{m.default_effort}</Tag> : <span className="text-muted-foreground">—</span>}</TableCell>
      <TableCell className="min-w-[150px]">{effs}</TableCell>
      <TableCell>
        <span className="font-mono tabular-nums">{m.context_length ? Math.round(m.context_length / 1000) + 'K' : '—'}</span>
      </TableCell>
      <TableCell>
        <OutCell m={m} pr={pr} />
      </TableCell>
      <TableCell>
        <TTFTCell h={h} />
      </TableCell>
      <TableCell className="text-right">
        <Button size="xs" variant="ghost" disabled={probing} onClick={onProbe}>
          {probing ? '探测中' : '探测'}
        </Button>
      </TableCell>
    </TableRow>
  );
}
