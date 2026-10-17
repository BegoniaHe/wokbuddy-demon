import { useMemo, useState, type ReactNode } from 'react';
import { useQuery } from '@tanstack/react-query';
import { api, postJSON } from '@/lib/api';
import { toast } from '@/components/Toaster';
import {
  fmtCredit,
  fmtCreditRatio,
  fmtModelRate,
  fmtMs,
  fmtTok,
  trimFixed,
  usageRate,
} from '@/lib/format';
import {
  cacheRateTone,
  fmtTokTimeLabel,
  parsePointTime,
  usSortRows,
  US_DIMS,
  type CreditDim,
  type DimKey,
  type SeriesPoint,
  type UsageData,
  type UsageRow,
  type UsSort,
} from '@/lib/usage';
import { defaultTimeRangeState, trangeLabel, trangeQuery, type TimeRangeState } from '@/lib/timeRange';
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
import { Tabs, TabsList, TabsTrigger } from '@/components/ui/tabs';
import {
  Card,
  CardAction,
  CardContent,
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
import { MixBar, Legend } from '@/components/ui-kit';
import { TimeRange } from '@/components/TimeRange';

// 用量明细排序选项。
const SORT_OPTIONS = [
  { value: 'total', label: '合计 Token' },
  { value: 'requests', label: '请求数' },
  { value: 'errors', label: '失败数' },
  { value: 'latency', label: '平均延迟' },
] as const satisfies readonly SelectOption[];

function usPct(part: unknown, total: unknown): string {
  const t = Number(total || 0);
  if (!t) return '—';
  return ((Number(part || 0) / t) * 100).toFixed(1) + '%';
}

// Kpi：数值卡。用于用量页顶部指标。
function Kpi({
  value,
  label,
  sub,
  tone = 'mute',
  children,
}: {
  value: ReactNode;
  label: string;
  sub?: string;
  tone?: 'accent' | 'ok' | 'warn' | 'mute';
  children?: ReactNode;
}) {
  const toneCls =
    tone === 'accent' ? 'text-accent-foreground' : tone === 'ok' ? 'text-success' : tone === 'warn' ? 'text-warning' : '';
  return (
    <div className="min-w-0 overflow-hidden rounded-lg border border-border/60 bg-secondary py-2.5 pl-4 pr-3.5">
      <div className="truncate text-2xs text-muted-foreground">{label}</div>
      <div className={toneCls + ' mt-1 whitespace-nowrap font-mono text-[23px] font-semibold leading-tight tracking-tight tabular-nums'}>
        {value}
      </div>
      {sub ? <div className="mt-1 truncate text-2xs text-muted-foreground">{sub}</div> : null}
      {children}
    </div>
  );
}

function Kpis({ children, cols = 6, className }: { children: ReactNode; cols?: 4 | 6; className?: string }) {
  return (
    <div
      className={
        'grid gap-2.5 px-4 pt-3.5 ' +
        (cols === 4 ? 'grid-cols-2 lg:grid-cols-4' : 'grid-cols-2 sm:grid-cols-3 xl:grid-cols-6') +
        (className ? ' ' + className : '')
      }
    >
      {children}
    </div>
  );
}

export function UsageView() {
  const [range, setRange] = useState<TimeRangeState>(() => defaultTimeRangeState());
  const [dim, setDim] = useState<DimKey>('account');
  const [creditDim, setCreditDim] = useState<CreditDim>('account');
  const [sort, setSort] = useState<UsSort>('total');
  const [backfilling, setBackfilling] = useState(false);

  const rangeStr = trangeQuery(range, true).toString();

  const { data, isFetching, error, refetch } = useQuery({
    queryKey: ['usage', rangeStr],
    queryFn: async () => {
      await api('models').catch(() => undefined); // 倍率回填（可选增强）
      return api<UsageData>('usage?' + rangeStr);
    },
  });

  const t = data?.totals ?? {};
  const total = Number(t.total_tokens || 0);
  const pt = Number(t.prompt_tokens || 0);
  const ct = Number(t.completion_tokens || 0);
  const reqs = Number(t.requests || 0);
  const errs = Number(t.errors || 0);
  const okRate = reqs ? ((reqs - errs) / reqs) * 100 : null;

  const winLabel = trangeLabel(range);
  const rangeEcho = data?.window_from
    ? String(data.window_from).replace('T', ' ').slice(0, 16) +
      (data.window_to ? ' → ' + String(data.window_to).replace('T', ' ').slice(0, 16) : ' → 现在')
    : '';
  const note =
    (rangeEcho || winLabel ? (rangeEcho || winLabel) + ' · ' : '') +
    (data?.buckets || 0) +
    ' 个分桶' +
    (data?.since ? ' · 数据自 ' + data.since.replace('T', ' ') : '') +
    (data?.file_bytes ? ' · 文件 ' + (data.file_bytes / 1024).toFixed(1) + ' KB' : '');

  const m = US_DIMS[dim];
  const rows = usSortRows((data?.[m.key] as UsageRow[]) || [], sort);
  const counts = {
    account: (data?.by_account || []).length,
    model: (data?.by_model || []).length,
    realm: (data?.by_realm || []).length,
  };

  const creditModels = data?.credit_by_model || [];
  const creditAccounts = data?.credit_by_account || [];

  return (
    <>
      <Card className="mb-5">
        <CardHeader className="flex-row flex-wrap items-center gap-3">
          <CardTitle>用量总览</CardTitle>
          <CardAction>
            <div className="flex flex-wrap items-center gap-3">
              <span className="min-w-0 text-xs text-muted-foreground" title={note}>{note}</span>
              <TimeRange value={range} onChange={setRange} />
              <Button
                size="xs"
                disabled={backfilling}
                onClick={() => {
                  if (!confirm('从上游拉取最近 7 天逐笔用量并按天覆盖本地用量桶（用于对账/回填），继续？')) return;
                  setBackfilling(true);
                  void postJSON<{ records?: number; buckets?: number; failures?: Record<string, string> }>('usage/backfill', { days: 7 })
                    .then((r) => {
                      const failed = r.failures ? Object.keys(r.failures).length : 0;
                      toast('回填完成：' + (r.records ?? 0) + ' 条记录，覆盖 ' + (r.buckets ?? 0) + ' 个桶' + (failed ? '，' + failed + ' 个账号失败' : ''), failed ? 'err' : 'ok');
                      void refetch();
                    })
                    .catch((e) => toast('回填失败：' + (e as Error).message, 'err'))
                    .finally(() => setBackfilling(false));
                }}
              >
                {backfilling ? '回填中…' : '回填上游'}
              </Button>
              <Button size="xs" disabled={isFetching} onClick={() => void refetch()}>
                刷新
              </Button>
            </div>
          </CardAction>
        </CardHeader>

        {error ? (
          <CardContent className="text-sm text-destructive">读取用量失败：{(error as Error).message}</CardContent>
        ) : (
          <>
            <Kpis>
              <Kpi value={fmtTok(reqs)} label="请求数" tone="accent" sub={errs ? '其中失败 ' + errs + ' 次' : '全部成功'} />
              <Kpi value={fmtTok(total)} label="总 token" tone="accent" sub={'prompt ' + usPct(pt, total) + ' · completion ' + usPct(ct, total)}>
                <MixBar prompt={pt} completion={ct} total={total} className="mt-1.5 h-1" />
              </Kpi>
              <Kpi value={fmtTok(pt)} label="prompt" sub={'占比 ' + usPct(pt, total)} />
              <Kpi value={fmtTok(ct)} label="completion" tone="ok" sub={'占比 ' + usPct(ct, total)} />
              <Kpi value={String(errs)} label="失败尝试" tone={errs ? 'warn' : 'mute'} sub={okRate == null ? '—' : errs ? '成功率 ' + okRate.toFixed(1) + '%' : '成功率 100%'} />
              <Kpi value={fmtMs(t.avg_latency_ms)} label="平均延迟" sub={t.avg_tokens_per_second ? '吐字 ' + usageRate(t.avg_tokens_per_second) : '无速率样本'} />
            </Kpis>

            <CardContent className="pt-3.5">
              <div className="overflow-hidden rounded-lg border border-border/60 bg-secondary">
                <div className="flex flex-wrap items-center gap-2.5 border-b border-border/60 px-3.5 py-2.5 text-xs">
                  <span className="font-semibold text-muted-foreground">Token 时序</span>
                  <span className="flex-1" />
                  <span className="flex items-center gap-1.5 text-2xs text-muted-foreground">
                    <Legend color="var(--primary)">prompt</Legend>
                    <Legend color="var(--success)">completion</Legend>
                    <span className="mx-1 h-3 w-px bg-border" />
                    <Legend color="var(--warning)">均值</Legend>
                  </span>
                </div>
                <div className="px-2.5 pb-1 pt-2">
                  <UsageChart series={data?.series || []} />
                </div>
              </div>
            </CardContent>
          </>
        )}
      </Card>

      <Card className="mb-5">
        <CardHeader className="flex-row flex-wrap items-center gap-3">
          <CardTitle>用量明细</CardTitle>
          <Tabs value={dim} onValueChange={(v) => setDim(v as DimKey)}>
            <TabsList>
              <TabsTrigger value="account">按账号 {counts.account}</TabsTrigger>
              <TabsTrigger value="model">按模型 {counts.model}</TabsTrigger>
              <TabsTrigger value="realm">按域 {counts.realm}</TabsTrigger>
            </TabsList>
          </Tabs>
          <CardAction>
            <div className="flex flex-wrap items-center gap-3">
              <span className="text-xs text-muted-foreground">{rows.length + ' 行 · 请求数含失败尝试'}</span>
              <Select
                items={selectOptionItems(SORT_OPTIONS)}
                value={sort}
                onValueChange={(v) => setSort(v as UsSort)}>
                <SelectTrigger size="sm" className="w-32">
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  <SelectOptions options={SORT_OPTIONS} />
                </SelectContent>
              </Select>
            </div>
          </CardAction>
        </CardHeader>
        <div className="overflow-x-auto">
          <Table className="[&_tbody_tr:nth-child(even)]:bg-secondary/55">
            <TableHeader>
              <TableRow>
                <TableHead className="w-[3px] p-0" aria-hidden />
                <TableHead>{m.title}</TableHead>
                {m.withRealm ? <TableHead>域</TableHead> : null}
                <TableHead className="text-right">请求</TableHead>
                <TableHead className="text-right">失败</TableHead>
                <TableHead className="text-right">Prompt</TableHead>
                <TableHead className="text-right">Completion</TableHead>
                <TableHead className="text-right">合计</TableHead>
                {m.withPerf ? <TableHead className="text-right">均延迟</TableHead> : null}
                {m.withPerf ? <TableHead className="text-right">均速率</TableHead> : null}
              </TableRow>
            </TableHeader>
            <TableBody>
              {rows.length === 0 ? (
                <TableRow>
                  <TableCell colSpan={m.span} className="py-10 text-center text-muted-foreground">暂无数据</TableCell>
                </TableRow>
              ) : (
                rows.map((x, i) => (
                  <UsageDimRow key={i} x={x} dim={dim} withPerf={m.withPerf} />
                ))
              )}
            </TableBody>
          </Table>
        </div>
      </Card>

      <Card>
        <CardHeader className="flex-row flex-wrap items-center gap-3">
          <CardTitle>积分扣除历史</CardTitle>
          <Tabs value={creditDim} onValueChange={(v) => setCreditDim(v as CreditDim)}>
            <TabsList>
              <TabsTrigger value="account">按账号 {creditAccounts.length}</TabsTrigger>
              <TabsTrigger value="model">按模型 {creditModels.length}</TabsTrigger>
            </TabsList>
          </Tabs>
          <CardAction>
            <span className="text-xs text-muted-foreground">
              {creditAccounts.length + ' 个账号 · ' + creditModels.length + ' 个模型倍率分组 · 仅统计与积分同时观测到的 Token'}
            </span>
          </CardAction>
        </CardHeader>
        <Kpis cols={4}>
          <Kpi value={fmtCredit(t.credits)} label="扣除积分" tone="accent" sub="按上游 usage.credit 累计" />
          <Kpi value={fmtTok(t.credit_tokens)} label="匹配 Token" sub="与积分同时观测到的 Token" />
          <Kpi value={fmtCreditRatio(t.credits_per_1m_tokens, t.credit_samples, t.credit_tokens)} label="平均积分 / 1M Token" tone="ok" sub="越低越划算" />
          <Kpi value={String(t.credit_samples || 0)} label="有效积分样本" sub="缺字段的历史不参与折算" />
          <Kpi
            value={cacheRateText(t.cache_hit_tokens, t.cache_miss_tokens)}
            label="缓存命中率"
            sub="上游前缀缓存命中 / (命中+未命中)；低命中意味着费用数倍放大"
          />
        </Kpis>
        <div className="overflow-x-auto">
          <Table className="[&_tbody_tr:nth-child(even)]:bg-secondary/55">
            <TableHeader>
              <TableRow>
                <TableHead className="w-[3px] p-0" aria-hidden />
                <TableHead>{creditDim === 'model' ? '模型' : '账号'}</TableHead>
                {creditDim === 'model' ? <TableHead>积分倍率</TableHead> : null}
                <TableHead className="text-right">请求</TableHead>
                <TableHead className="text-right">扣除积分</TableHead>
                <TableHead className="text-right">有效样本 Token</TableHead>
                <TableHead className="text-right">积分 / 1M Token</TableHead>
                <TableHead className="text-right">缓存命中率</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {creditDim === 'model' ? (
                creditModels.length === 0 ? (
                  <TableRow>
                    <TableCell colSpan={8} className="py-10 text-center text-muted-foreground">暂无积分扣除记录；升级前仅含 Token 的历史不会伪造积分。</TableCell>
                  </TableRow>
                ) : (
                  creditModels.map((row, i) => (
                    <TableRow key={i}>
                      <TableCell className="w-[3px] p-0" />
                      <TableCell>{row.key || '—'}</TableCell>
                      <TableCell>{fmtModelRate(row.rate)}</TableCell>
                      <TableCell className="text-right"><span className="font-mono tabular-nums">{fmtTok(row.requests)}</span></TableCell>
                      <TableCell className="text-right"><span className="font-mono tabular-nums">{fmtCredit(row.credits)}</span></TableCell>
                      <TableCell className="text-right"><span className="font-mono tabular-nums">{fmtTok(row.credit_tokens)}</span></TableCell>
                      <TableCell className="text-right"><span className="font-mono tabular-nums">{fmtCreditRatio(row.credits_per_1m_tokens, row.credit_samples, row.credit_tokens)}</span></TableCell>
                      <TableCell className="text-right"><CacheRateCell hit={row.cache_hit_tokens} miss={row.cache_miss_tokens} /></TableCell>
                    </TableRow>
                  ))
                )
              ) : creditAccounts.length === 0 ? (
                <TableRow>
                  <TableCell colSpan={7} className="py-10 text-center text-muted-foreground">暂无积分扣除记录；升级前仅含 Token 的历史不会伪造积分。</TableCell>
                </TableRow>
              ) : (
                creditAccounts.map((row, i) => {
                  const uid = String(row.key || '');
                  return (
                    <TableRow key={i}>
                      <TableCell className="w-[3px] p-0" />
                      <TableCell>
                        {row.nickname || uid.slice(0, 8) || '—'}
                        <div className="text-2xs text-muted-foreground">
                          {(row.realm || '') + ' · ' + uid.slice(0, 8)}
                        </div>
                      </TableCell>
                      <TableCell className="text-right"><span className="font-mono tabular-nums">{fmtTok(row.requests)}</span></TableCell>
                      <TableCell className="text-right"><span className="font-mono tabular-nums">{fmtCredit(row.credits)}</span></TableCell>
                      <TableCell className="text-right"><span className="font-mono tabular-nums">{fmtTok(row.credit_tokens)}</span></TableCell>
                      <TableCell className="text-right"><span className="font-mono tabular-nums">{fmtCreditRatio(row.credits_per_1m_tokens, row.credit_samples, row.credit_tokens)}</span></TableCell>
                      <TableCell className="text-right"><CacheRateCell hit={row.cache_hit_tokens} miss={row.cache_miss_tokens} /></TableCell>
                    </TableRow>
                  );
                })
              )}
            </TableBody>
          </Table>
        </div>
      </Card>
    </>
  );
}

function cacheRateText(hit: unknown, miss: unknown): string {
  const h = Number(hit || 0);
  const mm = Number(miss || 0);
  const total = h + mm;
  if (!total) return '—';
  return String(Math.round((h / total) * 1000) / 10) + '%';
}

function CacheRateCell({ hit, miss }: { hit?: number; miss?: number }) {
  const h = Number(hit || 0);
  const mm = Number(miss || 0);
  const total = h + mm;
  if (!total) return <span className="text-muted-foreground">—</span>;
  const pct = (h / total) * 100;
  const tone = cacheRateTone(pct);
  const cls = tone === 'ok' ? 'text-success' : tone === 'warn' ? 'text-warning' : 'text-destructive';
  return (
    <span className={cls + ' font-mono tabular-nums'} title={'命中 ' + fmtTok(h) + ' / 未命中 ' + fmtTok(mm) + ' tok'}>
      {trimFixed(pct.toFixed(1)) + '%'}
    </span>
  );
}

function UsageDimRow({ x, dim, withPerf }: { x: UsageRow; dim: DimKey; withPerf: boolean }) {
  const name = dim === 'account' ? String(x.key || '').slice(0, 8) : (x.key ?? '');
  const sub = dim === 'account' ? x.extra || '' : '';
  return (
    <TableRow>
      <TableCell className="w-[3px] p-0" />
      <TableCell>
        {name}
        {sub ? <div className="text-2xs text-muted-foreground">{sub}</div> : null}
      </TableCell>
      {dim === 'account' ? (
        <TableCell className="text-right">
          <span className="font-mono tabular-nums">{x.realm || ''}</span>
        </TableCell>
      ) : null}
      <TableCell className="text-right"><span className="font-mono tabular-nums">{fmtTok(x.requests)}</span></TableCell>
      <TableCell className="text-right">
        {x.errors ? <span className="font-mono tabular-nums text-warning">{fmtTok(x.errors)}</span> : <span className="text-muted-foreground">—</span>}
      </TableCell>
      <TableCell className="text-right"><span className="font-mono tabular-nums">{fmtTok(x.prompt_tokens)}</span></TableCell>
      <TableCell className="text-right"><span className="font-mono tabular-nums">{fmtTok(x.completion_tokens)}</span></TableCell>
      <TableCell className="text-right">
        <span className="font-mono tabular-nums">{fmtTok(x.total_tokens)}</span>
        <MixBar prompt={x.prompt_tokens} completion={x.completion_tokens} total={x.total_tokens} className="mt-1 min-w-[60px]" />
      </TableCell>
      {withPerf ? <TableCell className="text-right"><span className="font-mono tabular-nums">{fmtMs(x.avg_latency_ms)}</span></TableCell> : null}
      {withPerf ? <TableCell className="text-right"><span className="font-mono tabular-nums">{usageRate(x.avg_tokens_per_second)}</span></TableCell> : null}
    </TableRow>
  );
}

// ── 时序图 ────────────────────────────────────────────────────────────
function UsageChart({ series }: { series: SeriesPoint[] }) {
  const model = useMemo(() => {
    const pts: { t: number; scope?: string; raw: string; pt: number; ct: number; tt: number; req: number }[] = [];
    for (const p of series) {
      const t = parsePointTime(p);
      if (t === null) continue;
      const pt = Number(p.prompt_tokens || 0);
      const ct = Number(p.completion_tokens || 0);
      pts.push({ t, scope: p.scope, raw: p.t, pt, ct, tt: Number(p.total_tokens || 0) || pt + ct, req: p.requests || 0 });
    }
    return pts;
  }, [series]);

  if (!model.length) {
    return <div className="py-8 text-center text-xs text-muted-foreground">暂无用量数据。发起一次对话后再刷新。</div>;
  }

  const W = 1200;
  const H = 200;
  const PL = 58;
  const PR = 14;
  const PT = 18;
  const PB = 30;
  const iw = W - PL - PR;
  const ih = H - PT - PB;
  const t0 = model[0]!.t;
  const t1 = model[model.length - 1]!.t;
  const span = Math.max(1, t1 - t0);
  const max = Math.max(1, ...model.map((p) => p.tt));
  const peak = model.reduce((a, b) => (b.tt > a.tt ? b : a), model[0]!);
  const avg = model.reduce((s, p) => s + p.tt, 0) / model.length;

  let minGap = Infinity;
  for (let i = 1; i < model.length; i++) minGap = Math.min(minGap, model[i]!.t - model[i - 1]!.t);
  if (!isFinite(minGap) || minGap <= 0) minGap = span;
  const slot = iw * (minGap / span);
  const bw = Math.max(2, Math.min(30, slot * 0.7));
  const xOf = (t: number) => PL + bw / 2 + ((t - t0) / span) * Math.max(1, iw - bw);
  const yOf = (v: number) => PT + ih - ih * (v / max);
  const yBase = PT + ih;

  const ticks = Math.min(6, model.length);
  const usedLabel = new Set<number>();
  const xLabels: { x: number; anchor: 'start' | 'middle' | 'end'; label: string }[] = [];
  for (let k = 0; k < ticks; k++) {
    const target = t0 + span * (ticks === 1 ? 0.5 : k / (ticks - 1));
    let bi = 0;
    let best = Infinity;
    for (let i = 0; i < model.length; i++) {
      const d = Math.abs(model[i]!.t - target);
      if (d < best) {
        best = d;
        bi = i;
      }
    }
    if (usedLabel.has(bi)) continue;
    usedLabel.add(bi);
    const p = model[bi]!;
    const cx = xOf(p.t);
    xLabels.push({
      x: Math.max(PL, Math.min(W - PR, cx)),
      anchor: cx < PL + 14 ? 'start' : cx > W - PR - 14 ? 'end' : 'middle',
      label: fmtTokTimeLabel({ t: p.raw, scope: p.scope }),
    });
  }

  const dayLines: number[] = [];
  let prevDay: number | null = null;
  for (const p of model) {
    const d = new Date(p.t).getDate();
    if (prevDay !== null && d !== prevDay) dayLines.push(xOf(p.t));
    prevDay = d;
  }

  return (
    <>
      <svg viewBox={`0 0 ${W} ${H}`} role="img" preserveAspectRatio="xMidYMid meet" className="block h-auto w-full">
        <defs>
          <linearGradient id="usGradP" x1="0" y1="0" x2="0" y2="1">
            <stop offset="0" style={{ stopColor: 'var(--primary)', stopOpacity: 1 }} />
            <stop offset="1" style={{ stopColor: 'var(--primary)', stopOpacity: 0.6 }} />
          </linearGradient>
          <linearGradient id="usGradC" x1="0" y1="0" x2="0" y2="1">
            <stop offset="0" style={{ stopColor: 'var(--success)', stopOpacity: 1 }} />
            <stop offset="1" style={{ stopColor: 'var(--success)', stopOpacity: 0.6 }} />
          </linearGradient>
        </defs>
        {[0, 1, 2, 3, 4].map((i) => {
          const y = PT + ih - (ih * i) / 4;
          return (
            <g key={i}>
              <line x1={PL} y1={y} x2={W - PR} y2={y} stroke="var(--border)" strokeWidth={1} strokeDasharray="2 3" opacity={0.75} />
              <text x={PL - 6} y={y + 3.5} textAnchor="end" fill="var(--muted-foreground)" fontSize={10} fontFamily="var(--font-mono)">
                {fmtTok((max * i) / 4)}
              </text>
            </g>
          );
        })}
        {avg > 0 && avg < max ? (
          <g>
            <line x1={PL} y1={yOf(avg)} x2={W - PR} y2={yOf(avg)} stroke="var(--warning)" strokeWidth={1} strokeDasharray="4 4" opacity={0.55} />
            <text x={PL + 5} y={yOf(avg) - 4} textAnchor="start" fill="var(--warning)" fontSize={10} fontFamily="var(--font-mono)" opacity={0.9}>
              {'均值 ' + fmtTok(avg)}
            </text>
          </g>
        ) : null}
        {model.map((p, i) => {
          const x = xOf(p.t) - bw / 2;
          const hTot = ih * (p.tt / max);
          const hP = p.tt ? hTot * (p.pt / p.tt) : 0;
          const hC = Math.max(p.tt && p.ct ? 1 : 0, hTot - hP);
          return (
            <g key={i}>
              <title>{p.raw + '  ' + fmtTok(p.pt) + ' prompt / ' + fmtTok(p.ct) + ' completion / ' + p.req + ' 次'}</title>
              {hP > 0 ? (
                <rect className="transition-opacity hover:opacity-75" x={x} y={yBase - hP} width={bw} height={hP} fill="url(#usGradP)" rx={hC > 0 ? 0 : 1.5} />
              ) : null}
              {hC > 0 ? (
                <rect className="transition-opacity hover:opacity-75" x={x} y={yBase - hP - hC} width={bw} height={hC} fill="url(#usGradC)" rx={1.5} />
              ) : null}
            </g>
          );
        })}
        <text
          x={Math.max(PL, Math.min(W - PR, xOf(peak.t)))}
          y={Math.max(10, yOf(peak.tt) - 5)}
          textAnchor={xOf(peak.t) > W - PR - 90 ? 'end' : 'middle'}
          fill="var(--muted-foreground)"
          fontSize={10}
          fontWeight={600}
          fontFamily="var(--font-mono)"
        >
          {'峰值 ' + fmtTok(peak.tt)}
        </text>
        <line x1={PL} y1={yBase} x2={W - PR} y2={yBase} stroke="var(--border)" strokeWidth={1} />
        {xLabels.map((l, i) => (
          <text key={i} x={l.x} y={PT + ih + 15} textAnchor={l.anchor} fill="var(--muted-foreground)" fontSize={10} fontFamily="var(--font-mono)">
            {l.label}
          </text>
        ))}
        {dayLines.map((x, i) => (
          <line key={i} x1={x} y1={PT} x2={x} y2={PT + ih} stroke="var(--border)" strokeWidth={1} strokeDasharray="2 3" opacity={0.45} />
        ))}
      </svg>
      <div className="pb-1 text-2xs text-muted-foreground">
        {model.length + ' 个点 · 峰值 ' + fmtTok(peak.tt) + ' @ ' + fmtTokTimeLabel({ t: peak.raw, scope: peak.scope }) + ' · 均值 ' + fmtTok(avg)}
      </div>
    </>
  );
}
