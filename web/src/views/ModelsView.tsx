import { useEffect, useMemo, useState } from 'react';
import { useQuery } from '@tanstack/react-query';
import { api } from '@/lib/api';
import { mdMatch, mdSortList, DEFAULT_MODEL_FILTER, type ModelEntry, type ModelFilter } from '@/lib/models';
import { Button } from '@/components/ui/button';
import { Input, Select, Tag } from '@/components/ui/field';
import { Box, BoxHeader, BoxHint, BoxNote, BoxTitle, Grow } from '@/components/ui/box';
import { Empty, Num, Table, TableWrap, Td, Th } from '@/components/ui/table';
import { cn } from '@/lib/utils';

interface Probe {
  claimed?: number;
  measured?: number;
  note?: string;
  tested_at?: string;
  verdict?: string;
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
  const reload = () => {
    void modelsQ.refetch();
    void probesQ.refetch();
  };

  const data = modelsQ.data;
  const isFetching = modelsQ.isFetching;
  const error = modelsQ.error;
  const all = data?.models ?? [];
  const probes = probesQ.data?.probes ?? {};
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
    <Box>
      <BoxHeader>
        <BoxTitle>
          模型能力{' '}
          <BoxHint>
            max_tokens bug已修复目前可以正常透传最大输出,但是官方返回的最大输出仅为参考，不保证完全正确
          </BoxHint>
        </BoxTitle>
        <Grow />
        <BoxNote>{note}</BoxNote>
        <Button size="xs" disabled={isFetching} onClick={reload}>
          重新获取
        </Button>
      </BoxHeader>

      <div className="flex flex-wrap items-center gap-2 border-b border-line-soft bg-surface-2 px-4 py-2.5">
        <Input
          type="search"
          className="w-auto min-w-[200px] flex-[0_1_280px] rounded-md px-2.5 py-[5px] text-[12.5px]"
          placeholder="搜索模型 ID / 名称 / 描述 / 厂商"
          aria-label="搜索模型"
          value={q}
          onChange={(e) => setQ(e.target.value)}
        />
        <FilterSelect aria-label="按域筛选" value={filter.realm} onChange={(v) => setFilter((f) => ({ ...f, realm: v }))}>
          <option value="">全部域</option>
          <option value="cn">国内版 CN</option>
          <option value="global">国际版 Global</option>
        </FilterSelect>
        <FilterSelect aria-label="按能力筛选" value={filter.cap} onChange={(v) => setFilter((f) => ({ ...f, cap: v }))}>
          <option value="">全部能力</option>
          <option value="tool">支持工具调用</option>
          <option value="vision">支持视觉</option>
          <option value="reasoning">支持思考</option>
          <option value="default">默认模型</option>
        </FilterSelect>
        <FilterSelect aria-label="按思考档位筛选" value={filter.effort} onChange={(v) => setFilter((f) => ({ ...f, effort: v }))}>
          <option value="">全部档位</option>
          <option value="off">可关闭思考</option>
          <option value="low">low</option>
          <option value="medium">medium</option>
          <option value="high">high</option>
          <option value="xhigh">xhigh</option>
          <option value="max">max</option>
        </FilterSelect>
        <FilterSelect aria-label="按价格筛选" value={filter.promo} onChange={(v) => setFilter((f) => ({ ...f, promo: v }))}>
          <option value="">全部价格</option>
          <option value="promo">有折扣 / 限时免费</option>
          <option value="free">限时免费</option>
          <option value="discount">打折（非免费）</option>
        </FilterSelect>
        <FilterSelect aria-label="排序方式" value={filter.sort} onChange={(v) => setFilter((f) => ({ ...f, sort: v }))}>
          <option value="default">上游默认顺序</option>
          <option value="rate">积分倍率 低 → 高</option>
          <option value="context">上下文长度 大 → 小</option>
          <option value="output">最大输出 大 → 小</option>
          <option value="name">模型 ID A → Z</option>
        </FilterSelect>
        <Grow />
        <BoxNote className={cn(filtered && 'text-warn')}>
          {!all.length ? '' : filtered ? '命中 ' + list.length + ' / ' + all.length + ' 个模型' : all.length + ' 个模型'}
        </BoxNote>
        <Button size="xs" variant="ghost" title="清空全部筛选条件" onClick={() => { setQ(''); setFilter(DEFAULT_MODEL_FILTER); }}>
          重置
        </Button>
      </div>

      <TableWrap>
        <Table className="min-w-[700px]">
          <thead>
            <tr>
              <Th className="w-[3px] p-0" aria-hidden />
              <Th>模型</Th>
              <Th>积分倍率</Th>
              <Th>默认档</Th>
              <Th>支持的思考档位</Th>
              <Th>上下文长度</Th>
              <Th>最大输出</Th>
            </tr>
          </thead>
          <tbody>
            {error ? (
              <tr>
                <Td colSpan={7}>
                  <Empty>{(error as Error).message}</Empty>
                </Td>
              </tr>
            ) : list.length === 0 ? (
              <tr>
                <Td colSpan={7}>
                  <Empty>{all.length ? '没有符合当前筛选条件的模型' : '正在向上游查询…'}</Empty>
                </Td>
              </tr>
            ) : (
              list.map((m) => <ModelRow key={m.id} m={m} pr={probeOf(m.id)} />)
            )}
          </tbody>
        </Table>
      </TableWrap>
    </Box>
  );
}

function FilterSelect({
  value,
  onChange,
  children,
  ...rest
}: {
  value: string;
  onChange: (v: string) => void;
  children: React.ReactNode;
  'aria-label'?: string;
}) {
  return (
    <Select
      {...rest}
      className="w-auto rounded-md px-2.5 py-[5px] text-[12.5px]"
      value={value}
      onChange={(e) => onChange(e.target.value)}
    >
      {children}
    </Select>
  );
}

function RateCell({ m }: { m: ModelEntry }) {
  const tip = m.promo_note;
  if (m.promo_factor != null && m.promo_credits) {
    return (
      <span title={tip} className="cursor-help">
        <b className="font-mono">{m.promo_credits}</b>
        {m.promo_label ? <Tag tone="ok" className="ml-1">{m.promo_label}</Tag> : null}
        {m.credits ? <s className="ml-1 text-[11.5px] text-ink-3">{m.credits}</s> : null}
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
  if (!pr) return <Num>{m.max_output_tokens ? fmtK(m.max_output_tokens) : '—'}</Num>;
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
        <Num title={tip}>
          <span className="font-semibold text-warn">{fmtK(pr.measured)} ⚠</span>
          <div className="text-[11px] text-ink-3">钳制 {xs}{stale}</div>
        </Num>
      );
    }
    return (
      <Num title={tip}>
        <span className="text-ok">
          {fmtK(pr.measured)}
          {pr.claimed && pr.measured > pr.claimed ? ' ↑' : ' ✓'}
        </span>
      </Num>
    );
  }
  if (pr.verdict === 'at_least' && pr.measured)
    return (
      <Num title={tip}>
        <span className="text-ink-3">≥{fmtK(pr.measured)}</span>
      </Num>
    );
  return (
    <Num title={tip}>
      <span className="text-ink-3">?</span>
      <div className="text-[11px] text-ink-3">未测出{stale}</div>
    </Num>
  );
}

function ModelRow({ m, pr }: { m: ModelEntry; pr?: Probe }) {
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
    <span className="text-[12.5px] text-ink-3">
      {m.supports_reasoning ? '固定档 · 默认 ' + (m.default_effort || '?') : '不支持思考'}
    </span>
  );
  const caps: React.ReactNode[] = [];
  if (m.is_default) caps.push(<Tag key="d" tone="ok">默认</Tag>);
  if (m.supports_tool_call) caps.push(<Tag key="t" tone="warn">工具</Tag>);
  if (m.supports_images) caps.push(<Tag key="v" tone="warn">视觉</Tag>);
  if (m.supports_reasoning && !m.can_disable_thinking) caps.push(<Tag key="r" tone="warn">思考常开</Tag>);
  return (
    <tr>
      <Td className="w-[3px] p-0">
        <i className="block h-full min-h-[36px] w-[3px] bg-ok" />
      </Td>
      <Td title={m.description}>
        <div className="font-medium">{m.id}</div>
        <div className="font-mono text-[11.5px] text-ink-3">{m.name || ''}</div>
        {caps.length ? <div className="mt-0.5 flex gap-1">{caps}</div> : null}
      </Td>
      <Td>
        <RateCell m={m} />
      </Td>
      <Td>{m.default_effort ? <Tag tone="ok">{m.default_effort}</Tag> : <span className="text-ink-3">—</span>}</Td>
      <Td className="min-w-[150px]">{effs}</Td>
      <Td>
        <Num>{m.context_length ? Math.round(m.context_length / 1000) + 'K' : '—'}</Num>
      </Td>
      <Td>
        <OutCell m={m} pr={pr} />
      </Td>
    </tr>
  );
}
