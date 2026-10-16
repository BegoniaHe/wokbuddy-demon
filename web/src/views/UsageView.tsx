import { useMemo, useState } from 'react';
import { useQuery } from '@tanstack/react-query';
import { api } from '@/lib/api';
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
import { Select } from '@/components/ui/field';
import { Box, BoxBody, BoxHeader, BoxNote, BoxTitle, Grow } from '@/components/ui/box';
import { Empty, Num, RailCell, Table, TableWrap, Td, Th } from '@/components/ui/table';
import { Kpi, Kpis } from '@/components/ui/stat';
import { MixBar, Legend } from '@/components/ui/bar';
import { Tabs } from '@/components/ui/tabs';
import { TimeRange } from '@/components/TimeRange';

function usPct(part: unknown, total: unknown): string {
  const t = Number(total || 0);
  if (!t) return '—';
  return ((Number(part || 0) / t) * 100).toFixed(1) + '%';
}

export function UsageView() {
  const [range, setRange] = useState<TimeRangeState>(() => defaultTimeRangeState());
  const [dim, setDim] = useState<DimKey>('account');
  const [creditDim, setCreditDim] = useState<CreditDim>('account');
  const [sort, setSort] = useState<UsSort>('total');

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
      <Box>
        <BoxHeader>
          <BoxTitle>用量总览</BoxTitle>
          <Grow />
          <BoxNote className="min-w-0 text-left" title={note}>
            {note}
          </BoxNote>
          <TimeRange value={range} onChange={setRange} />
          <Button size="xs" disabled={isFetching} onClick={() => void refetch()}>
            刷新
          </Button>
        </BoxHeader>

        {error ? (
          <div className="p-4 text-sm text-bad">读取用量失败：{(error as Error).message}</div>
        ) : (
          <>
            <Kpis>
              <Kpi value={fmtTok(reqs)} label="请求数" tone="accent" sub={errs ? '其中失败 ' + errs + ' 次' : '全部成功'} />
              <Kpi value={fmtTok(total)} label="总 token" tone="accent" sub={'prompt ' + usPct(pt, total) + ' · completion ' + usPct(ct, total)}>
                <MixBar prompt={pt} completion={ct} total={total} className="mt-1.5 h-1" />
              </Kpi>
              <Kpi value={fmtTok(pt)} label="prompt" tone="soft" sub={'占比 ' + usPct(pt, total)} />
              <Kpi value={fmtTok(ct)} label="completion" tone="ok" sub={'占比 ' + usPct(ct, total)} />
              <Kpi value={String(errs)} label="失败尝试" tone={errs ? 'warn' : 'mute'} sub={okRate == null ? '—' : errs ? '成功率 ' + okRate.toFixed(1) + '%' : '成功率 100%'} />
              <Kpi value={fmtMs(t.avg_latency_ms)} label="平均延迟" tone="soft" sub={t.avg_tokens_per_second ? '吐字 ' + usageRate(t.avg_tokens_per_second) : '无速率样本'} />
            </Kpis>

            <BoxBody>
              <div className="overflow-hidden rounded-inner border border-line-soft bg-surface-2">
                <div className="flex flex-wrap items-center gap-2.5 border-b border-line-soft px-3.5 py-2.5 text-xs">
                  <span className="font-semibold text-ink-2">Token 时序</span>
                  <Grow />
                  <span className="flex items-center gap-1.5 text-2xs text-ink-3">
                    <Legend color="var(--accent)">prompt</Legend>
                    <Legend color="var(--ok)">completion</Legend>
                    <span className="mx-1 h-3 w-px bg-line" />
                    <Legend color="var(--warn)">均值</Legend>
                  </span>
                </div>
                <div className="px-2.5 pb-1 pt-2">
                  <UsageChart series={data?.series || []} />
                </div>
              </div>
            </BoxBody>
          </>
        )}
      </Box>

      <Box>
        <BoxHeader>
          <BoxTitle>用量明细</BoxTitle>
          <Tabs
            active={dim}
            onSelect={(v) => setDim(v as DimKey)}
            items={[
              { value: 'account', label: '按账号', count: counts.account },
              { value: 'model', label: '按模型', count: counts.model },
              { value: 'realm', label: '按域', count: counts.realm },
            ]}
          />
          <Grow />
          <BoxNote>{rows.length + ' 行 · 请求数含失败尝试'}</BoxNote>
          <label className="inline-flex items-center gap-1.5 whitespace-nowrap text-xs text-ink-3">
            排序
            <Select size="sm" value={sort} onChange={(e) => setSort(e.target.value as UsSort)}>
              <option value="total">合计 Token</option>
              <option value="requests">请求数</option>
              <option value="errors">失败数</option>
              <option value="latency">平均延迟</option>
            </Select>
          </label>
        </BoxHeader>
        <TableWrap>
          <Table className="[&_tbody_tr:nth-child(even)]:bg-surface-2/55">
            <thead>
              <tr>
                <Th className="w-[3px] p-0" aria-hidden />
                <Th>{m.title}</Th>
                {m.withRealm ? <Th>域</Th> : null}
                <Th className="text-right">请求</Th>
                <Th className="text-right">失败</Th>
                <Th className="text-right">Prompt</Th>
                <Th className="text-right">Completion</Th>
                <Th className="text-right">合计</Th>
                {m.withPerf ? <Th className="text-right">均延迟</Th> : null}
                {m.withPerf ? <Th className="text-right">均速率</Th> : null}
              </tr>
            </thead>
            <tbody>
              {rows.length === 0 ? (
                <tr>
                  <Td colSpan={m.span}>
                    <Empty>暂无数据</Empty>
                  </Td>
                </tr>
              ) : (
                rows.map((x, i) => (
                  <UsageDimRow key={i} x={x} dim={dim} withPerf={m.withPerf} />
                ))
              )}
            </tbody>
          </Table>
        </TableWrap>
      </Box>

      <Box>
        <BoxHeader>
          <BoxTitle>积分扣除历史</BoxTitle>
          <Tabs
            active={creditDim}
            onSelect={(v) => setCreditDim(v as CreditDim)}
            items={[
              { value: 'account', label: '按账号', count: creditAccounts.length },
              { value: 'model', label: '按模型', count: creditModels.length },
            ]}
          />
          <Grow />
          <BoxNote>
            {creditAccounts.length +
              ' 个账号 · ' +
              creditModels.length +
              ' 个模型倍率分组 · 仅统计与积分同时观测到的 Token'}
          </BoxNote>
        </BoxHeader>
        <Kpis cols={4}>
          <Kpi value={fmtCredit(t.credits)} label="扣除积分" tone="accent" sub="按上游 usage.credit 累计" />
          <Kpi value={fmtTok(t.credit_tokens)} label="匹配 Token" tone="mute" sub="与积分同时观测到的 Token" />
          <Kpi value={fmtCreditRatio(t.credits_per_1m_tokens, t.credit_samples, t.credit_tokens)} label="平均积分 / 1M Token" tone="ok" sub="越低越划算" />
          <Kpi value={String(t.credit_samples || 0)} label="有效积分样本" tone="mute" sub="缺字段的历史不参与折算" />
          <Kpi
            value={cacheRateText(t.cache_hit_tokens, t.cache_miss_tokens)}
            label="缓存命中率"
            tone="mute"
            sub="上游前缀缓存命中 / (命中+未命中)；低命中意味着费用数倍放大"
          />
        </Kpis>
        <TableWrap>
          <Table className="[&_tbody_tr:nth-child(even)]:bg-surface-2/55">
            <thead>
              <tr>
                <Th className="w-[3px] p-0" aria-hidden />
                <Th>{creditDim === 'model' ? '模型' : '账号'}</Th>
                {creditDim === 'model' ? <Th>积分倍率</Th> : null}
                <Th className="text-right">请求</Th>
                <Th className="text-right">扣除积分</Th>
                <Th className="text-right">有效样本 Token</Th>
                <Th className="text-right">积分 / 1M Token</Th>
                <Th className="text-right">缓存命中率</Th>
              </tr>
            </thead>
            <tbody>
              {creditDim === 'model' ? (
                creditModels.length === 0 ? (
                  <tr>
                    <Td colSpan={8}>
                      <Empty>暂无积分扣除记录；升级前仅含 Token 的历史不会伪造积分。</Empty>
                    </Td>
                  </tr>
                ) : (
                  creditModels.map((row, i) => (
                    <tr key={i}>
                      <RailCell />
                      <Td>{row.key || '—'}</Td>
                      <Td>{fmtModelRate(row.rate)}</Td>
                      <Td className="text-right"><Num>{fmtTok(row.requests)}</Num></Td>
                      <Td className="text-right"><Num>{fmtCredit(row.credits)}</Num></Td>
                      <Td className="text-right"><Num>{fmtTok(row.credit_tokens)}</Num></Td>
                      <Td className="text-right"><Num>{fmtCreditRatio(row.credits_per_1m_tokens, row.credit_samples, row.credit_tokens)}</Num></Td>
                      <Td className="text-right"><CacheRateCell hit={row.cache_hit_tokens} miss={row.cache_miss_tokens} /></Td>
                    </tr>
                  ))
                )
              ) : creditAccounts.length === 0 ? (
                <tr>
                  <Td colSpan={7}>
                    <Empty>暂无积分扣除记录；升级前仅含 Token 的历史不会伪造积分。</Empty>
                  </Td>
                </tr>
              ) : (
                creditAccounts.map((row, i) => {
                  const uid = String(row.key || '');
                  return (
                    <tr key={i}>
                      <RailCell />
                      <Td>
                        {row.nickname || uid.slice(0, 8) || '—'}
                        <div className="text-2xs text-ink-3">
                          {(row.realm || '') + ' · ' + uid.slice(0, 8)}
                        </div>
                      </Td>
                      <Td className="text-right"><Num>{fmtTok(row.requests)}</Num></Td>
                      <Td className="text-right"><Num>{fmtCredit(row.credits)}</Num></Td>
                      <Td className="text-right"><Num>{fmtTok(row.credit_tokens)}</Num></Td>
                      <Td className="text-right"><Num>{fmtCreditRatio(row.credits_per_1m_tokens, row.credit_samples, row.credit_tokens)}</Num></Td>
                      <Td className="text-right"><CacheRateCell hit={row.cache_hit_tokens} miss={row.cache_miss_tokens} /></Td>
                    </tr>
                  );
                })
              )}
            </tbody>
          </Table>
        </TableWrap>
      </Box>
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
  if (!total) return <span className="text-ink-3">—</span>;
  const pct = (h / total) * 100;
  const tone = cacheRateTone(pct);
  const cls = tone === 'ok' ? 'text-ok' : tone === 'warn' ? 'text-warn' : 'text-bad';
  return (
    <Num className={cls} title={'命中 ' + fmtTok(h) + ' / 未命中 ' + fmtTok(mm) + ' tok'}>
      {trimFixed(pct.toFixed(1)) + '%'}
    </Num>
  );
}

function UsageDimRow({ x, dim, withPerf }: { x: UsageRow; dim: DimKey; withPerf: boolean }) {
  const name = dim === 'account' ? String(x.key || '').slice(0, 8) : (x.key ?? '');
  const sub = dim === 'account' ? x.extra || '' : '';
  return (
    <tr>
      <RailCell />
      <Td>
        {name}
        {sub ? <div className="text-2xs text-ink-3">{sub}</div> : null}
      </Td>
      {dim === 'account' ? (
        <Td className="text-right">
          <Num>{x.realm || ''}</Num>
        </Td>
      ) : null}
      <Td className="text-right"><Num>{fmtTok(x.requests)}</Num></Td>
      <Td className="text-right">
        {x.errors ? <Num className="text-warn">{fmtTok(x.errors)}</Num> : <span className="text-ink-3">—</span>}
      </Td>
      <Td className="text-right"><Num>{fmtTok(x.prompt_tokens)}</Num></Td>
      <Td className="text-right"><Num>{fmtTok(x.completion_tokens)}</Num></Td>
      <Td className="text-right">
        <Num>{fmtTok(x.total_tokens)}</Num>
        <MixBar prompt={x.prompt_tokens} completion={x.completion_tokens} total={x.total_tokens} className="mt-1 min-w-[60px]" />
      </Td>
      {withPerf ? <Td className="text-right"><Num>{fmtMs(x.avg_latency_ms)}</Num></Td> : null}
      {withPerf ? <Td className="text-right"><Num>{usageRate(x.avg_tokens_per_second)}</Num></Td> : null}
    </tr>
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
    return <div className="py-8 text-center text-xs text-ink-3">暂无用量数据。发起一次对话后再刷新。</div>;
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
            <stop offset="0" style={{ stopColor: 'var(--accent)', stopOpacity: 1 }} />
            <stop offset="1" style={{ stopColor: 'var(--accent)', stopOpacity: 0.6 }} />
          </linearGradient>
          <linearGradient id="usGradC" x1="0" y1="0" x2="0" y2="1">
            <stop offset="0" style={{ stopColor: 'var(--ok)', stopOpacity: 1 }} />
            <stop offset="1" style={{ stopColor: 'var(--ok)', stopOpacity: 0.6 }} />
          </linearGradient>
        </defs>
        {[0, 1, 2, 3, 4].map((i) => {
          const y = PT + ih - (ih * i) / 4;
          return (
            <g key={i}>
              <line x1={PL} y1={y} x2={W - PR} y2={y} stroke="var(--line-soft)" strokeWidth={1} strokeDasharray="2 3" opacity={0.75} />
              <text x={PL - 6} y={y + 3.5} textAnchor="end" fill="var(--ink-3)" fontSize={10} fontFamily="var(--mono)">
                {fmtTok((max * i) / 4)}
              </text>
            </g>
          );
        })}
        {avg > 0 && avg < max ? (
          <g>
            <line x1={PL} y1={yOf(avg)} x2={W - PR} y2={yOf(avg)} stroke="var(--warn)" strokeWidth={1} strokeDasharray="4 4" opacity={0.55} />
            <text x={PL + 5} y={yOf(avg) - 4} textAnchor="start" fill="var(--warn)" fontSize={10} fontFamily="var(--mono)" opacity={0.9}>
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
          fill="var(--ink-2)"
          fontSize={10}
          fontWeight={600}
          fontFamily="var(--mono)"
        >
          {'峰值 ' + fmtTok(peak.tt)}
        </text>
        <line x1={PL} y1={yBase} x2={W - PR} y2={yBase} stroke="var(--line)" strokeWidth={1} />
        {xLabels.map((l, i) => (
          <text key={i} x={l.x} y={PT + ih + 15} textAnchor={l.anchor} fill="var(--ink-3)" fontSize={10} fontFamily="var(--mono)">
            {l.label}
          </text>
        ))}
        {dayLines.map((x, i) => (
          <line key={i} x1={x} y1={PT} x2={x} y2={PT + ih} stroke="var(--line-soft)" strokeWidth={1} strokeDasharray="2 3" opacity={0.45} />
        ))}
      </svg>
      <div className="pb-1 text-2xs text-ink-3">
        {model.length + ' 个点 · 峰值 ' + fmtTok(peak.tt) + ' @ ' + fmtTokTimeLabel({ t: peak.raw, scope: peak.scope }) + ' · 均值 ' + fmtTok(avg)}
      </div>
    </>
  );
}
