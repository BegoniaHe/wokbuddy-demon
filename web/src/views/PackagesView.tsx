import { useMemo, useState } from 'react';
import { useQuery } from '@tanstack/react-query';
import { api } from '@/lib/api';
import { fmtTok } from '@/lib/format';
import { dig } from '@/lib/config';
import {
  pkAccountColorMap,
  pkAccountSegments,
  pkBySource,
  pkColor,
  pkCreditOpacity,
  pkDetailGroups,
  pkDetailLimitValue,
  pkExpiryDateTime,
  pkExpiryText,
  PK_SORT_LABELS,
  summarizeCreditDays,
  type Pkg,
  type PkgAccount,
} from '@/lib/packages';
import { Button } from '@/components/ui/button';
import { Select } from '@/components/ui/field';
import { Box, BoxBody, BoxHeader, BoxNote, BoxTitle, Grow } from '@/components/ui/box';
import { Empty, RailCell, Table, TableWrap, Td, Th } from '@/components/ui/table';
import { Legend } from '@/components/ui/bar';
import { cn } from '@/lib/utils';

const LS_PK_SORT = 'pkSortMode';

export function PackagesView() {
  const [sortMode, setSortMode] = useState(() => {
    const v = localStorage.getItem(LS_PK_SORT) || 'end_asc';
    return v === 'size_desc' ? 'size_desc' : 'end_asc';
  });
  const [expanded, setExpanded] = useState<Set<string>>(new Set());

  const pkgQ = useQuery({
    queryKey: ['packages'],
    queryFn: () => api<{ accounts?: PkgAccount[] }>('packages'),
    staleTime: 2 * 60 * 1000,
  });
  const cfgQ = useQuery({
    queryKey: ['config'],
    queryFn: () => api<{ config?: unknown }>('config').catch(() => ({}) as { config?: unknown }),
    staleTime: 5 * 60 * 1000,
  });

  const detailLimit = pkDetailLimitValue(dig(cfgQ.data?.config, ['panel', 'package_detail_limit']));
  const list = pkgQ.data?.accounts ?? [];
  const now = Date.now();

  const changeSort = (v: string) => {
    setSortMode(v);
    localStorage.setItem(LS_PK_SORT, v);
  };

  const toggle = (key: string) => {
    setExpanded((prev) => {
      const next = new Set(prev);
      if (next.has(key)) next.delete(key);
      else next.add(key);
      return next;
    });
  };

  return (
    <>
      <Box>
        <BoxHeader>
          <BoxTitle>积分到期分布</BoxTitle>
          <Grow />
          <BoxNote>按批次到期日聚合，颜色区分账号</BoxNote>
        </BoxHeader>
        <BoxBody>
          <ExpiryDistribution list={list} now={now} />
        </BoxBody>
      </Box>

      <Box>
        <BoxHeader>
          <BoxTitle>账号对比</BoxTitle>
          <Grow />
          <BoxNote>{list.length ? list.length + ' 个账号 · 实时查询上游' : '—'}</BoxNote>
          <Select size="sm" value={sortMode} onChange={(e) => changeSort(e.target.value)} title="逐包明细的排序规则">
            <option value="end_asc">按到期时间</option>
            <option value="size_desc">按面额大小</option>
          </Select>
          <Button size="xs" disabled={pkgQ.isFetching} onClick={() => void pkgQ.refetch()}>
            刷新
          </Button>
        </BoxHeader>
        <BoxBody>
          {pkgQ.isLoading ? (
            <Empty>查询中…（逐账号向上游实时查询）</Empty>
          ) : pkgQ.error ? (
            <Empty>读取失败：{(pkgQ.error as Error).message}</Empty>
          ) : list.length === 0 ? (
            <Empty>没有账号</Empty>
          ) : (
            <ComparisonCards list={list} now={now} />
          )}
        </BoxBody>
      </Box>

      {list
        .filter((a) => !a.error)
        .map((a) => (
          <DetailBox
            key={a.uid}
            a={a}
            limit={detailLimit}
            mode={sortMode}
            expanded={expanded}
            onToggle={toggle}
          />
        ))}
    </>
  );
}

function ExpiryDistribution({ list, now }: { list: PkgAccount[]; now: number }) {
  const summary = useMemo(() => summarizeCreditDays(list, now), [list, now]);
  const colors = useMemo(() => pkAccountColorMap(list), [list]);

  if (!summary.rows.length) {
    return <div className="py-5 text-center text-xs text-ink-3">暂无可汇总积分</div>;
  }

  const legend = list
    .filter((a) => a && !a.error && a.uid && pkAccountSegments(a, now).some((s) => s.days != null))
    .map((a) => (
      <Legend key={a.uid} color={colors.get(String(a.uid)) || 'var(--accent)'}>
        {a.nickname || String(a.uid).slice(0, 8)}
      </Legend>
    ));

  const foot = summary.accountCount + ' 个账号' + (summary.unavailable ? ' · ' + summary.unavailable + ' 个未获取余额' : '');

  return (
    <>
      <div className="mb-1 grid grid-cols-[64px_minmax(80px,1fr)_72px] items-center gap-2.5 text-2xs text-ink-3">
        <span>剩余天数</span>
        <span className="text-center">各账号该批剩余</span>
        <b className="text-right font-semibold">剩余积分</b>
      </div>
      <div className="max-h-[260px] overflow-y-auto overflow-x-hidden pb-0 pr-2.5 [scrollbar-gutter:stable]">
        {summary.rows.map((row) => {
          const total = row.credits || 1;
          return (
            <div key={row.days} className="grid min-h-[30px] grid-cols-[64px_minmax(80px,1fr)_72px] items-center gap-2.5 pr-0.5">
              <span className="text-2xs text-ink-3">{row.days === 0 ? '已到期' : row.days + ' 天'}</span>
              <div className="flex h-[9px] items-stretch gap-0.5 overflow-hidden rounded-[5px] bg-surface-2">
                {row.segments.map((segment, i) => {
                  const color = colors.get(segment.uid) || 'var(--accent)';
                  const title =
                    segment.source +
                    '\n' +
                    fmtTok(segment.amount) +
                    ' 积分\n到期时间 ' +
                    pkExpiryDateTime(segment.expiresAt) +
                    '（' +
                    pkExpiryText(segment.expiresAt) +
                    '）\n' +
                    segment.accountName;
                  return (
                    <span
                      key={i}
                      className="block h-full min-w-[3px] transition-transform hover:scale-y-125"
                      style={{
                        background: color,
                        opacity: pkCreditOpacity(segment.days),
                        flex: Math.max(0.008, segment.amount / total).toFixed(4) + ' 1 0',
                      }}
                      title={title}
                    />
                  );
                })}
              </div>
              <b className="text-right font-mono text-2xs tabular-nums [overflow-wrap:anywhere]">{fmtTok(row.credits)}</b>
            </div>
          );
        })}
      </div>
      {legend.length ? <div className="mt-2 flex flex-wrap gap-x-3 gap-y-1 text-2xs text-ink-3">{legend}</div> : null}
      <div className="mt-2 border-t border-line-soft pt-2 text-2xs text-ink-3">{foot}</div>
    </>
  );
}

function ComparisonCards({ list, now }: { list: PkgAccount[]; now: number }) {
  const expiryColors = pkAccountColorMap(list);
  const names: string[] = [];
  for (const a of list) for (const s of pkBySource(a.packages || [])) if (!names.includes(s.key)) names.push(s.key);
  names.sort((x, y) => {
    const sz = (n: string) =>
      Math.max(...list.map((a) => {
        const f = pkBySource(a.packages || []).find((s) => s.key === n);
        return f ? f.size : 0;
      }));
    return sz(y) - sz(x);
  });
  const colorOf = (n: string) => pkColor(names.indexOf(n));
  const maxRemain = Math.max(1, ...list.map((a) => Number(a.remain || 0)));

  return (
    <div className="flex flex-wrap gap-2.5">
      {list.map((a) => {
        if (a.error) {
          return (
            <div key={a.uid} className="min-w-[200px] flex-1 rounded-inner border border-line-soft p-3">
              <div className="flex items-baseline gap-1.5">
                <span className="text-sm font-semibold">{a.nickname || String(a.uid).slice(0, 8)}</span>
                <span className="font-mono text-2xs text-ink-3">{a.realm || ''}</span>
              </div>
              <div className="mt-1 text-xs text-warn">查询失败：{a.error}</div>
            </div>
          );
        }
        const srcs = pkBySource(a.packages || []);
        const total = Math.max(1, Number(a.size || 0));
        const expiry = pkAccountSegments(a, now);
        const expiryTotal = Math.max(1, expiry.reduce((s, x) => s + x.amount, 0));
        const expiryColor = expiryColors.get(String(a.uid)) || 'var(--accent)';
        return (
          <div key={a.uid} className="min-w-[200px] flex-1 rounded-inner border border-line-soft p-3">
            <div className="mb-1.5 flex items-baseline gap-1.5">
              <span className="text-sm font-semibold">{a.nickname || String(a.uid).slice(0, 8)}</span>
              <span className="font-mono text-2xs text-ink-3">{a.realm || ''}</span>
            </div>
            <div className="font-mono text-2xl font-semibold tabular-nums">{fmtTok(a.remain)}</div>
            <div className="mt-0.5 text-2xs text-ink-3">
              共 {fmtTok(a.size)} · {(a.packages || []).length} 个包 · 占最高 {((Number(a.remain || 0) / maxRemain) * 100).toFixed(0)}%
            </div>
            <div className="mt-2 flex h-[7px] overflow-hidden rounded bg-surface-2">
              {srcs.map((s, i) => (
                <i
                  key={i}
                  className="block h-full"
                  style={{ width: ((s.size / total) * 100).toFixed(2) + '%', background: colorOf(s.key) }}
                  title={s.name + ' ' + fmtTok(s.size)}
                />
              ))}
            </div>
            {expiry.length ? (
              <div className="mt-1.5 flex h-[5px] overflow-hidden rounded bg-surface-2">
                {expiry.map((s, i) => (
                  <i
                    key={i}
                    className="block h-full min-w-[2px]"
                    style={{
                      background: expiryColor,
                      opacity: pkCreditOpacity(s.days),
                      flex: Math.max(0.008, s.amount / expiryTotal).toFixed(4) + ' 1 0',
                    }}
                    title={s.source + '\n' + fmtTok(s.amount) + ' 积分\n到期时间 ' + pkExpiryDateTime(s.expiresAt) + '（' + pkExpiryText(s.expiresAt) + '）'}
                  />
                ))}
              </div>
            ) : null}
            <div className="mt-2 flex flex-wrap gap-x-3 gap-y-1 text-2xs text-ink-3">
              {srcs.map((s, i) => (
                <Legend key={i} color={colorOf(s.key)}>
                  {s.name.replace(/^CodeBuddy/, '') + ' x' + s.n + ' · ' + fmtTok(s.size) + (s.minCreated ? ' · 首发 ' + s.minCreated.slice(5) : '')}
                </Legend>
              ))}
            </div>
          </div>
        );
      })}
    </div>
  );
}

function DetailBox({
  a,
  limit,
  mode,
  expanded,
  onToggle,
}: {
  a: PkgAccount;
  limit: number;
  mode: string;
  expanded: Set<string>;
  onToggle: (key: string) => void;
}) {
  const names: string[] = [];
  for (const s of pkBySource(a.packages || [])) {
    if (!names.includes(s.key)) names.push(s.key);
  }
  const colorOf = (k: string) => pkColor(names.indexOf(k));
  const groups = pkDetailGroups(a.packages || [], limit, mode);
  const uid = String(a.uid);

  const rowOf = (p: Pkg, i: number) => {
    const k = (p.package_code || '') + '|' + (p.name || '(未命名)');
    const sub =
      (p.sub_product_code || '').replace(/^sp_tcaca_codebuddyide_?/, '') || (p.package_code || '').replace(/^TCACA_/, '');
    return (
      <tr key={k + '-' + i}>
        <RailCell style={{ background: colorOf(k) }} />
        <Td>
          {p.name || '(未命名)'}
          {sub ? <div className="text-2xs text-ink-3">{sub}</div> : null}
        </Td>
        <Td className="text-right font-mono tabular-nums">{fmtTok(p.size)}</Td>
        <Td className="text-right font-mono tabular-nums">{fmtTok(p.remain)}</Td>
        <Td className="text-right font-mono tabular-nums">{fmtTok(p.used)}</Td>
        <Td className="text-right font-mono tabular-nums">{(p.created_at || '').slice(0, 16).replace('T', ' ') || '—'}</Td>
        <Td className="text-right font-mono tabular-nums">{(p.end_time || '').slice(0, 10) || '—'}</Td>
      </tr>
    );
  };

  const groupSummary = (group: string, count: number, size: number, remain: number) => {
    const key = uid + ':' + group;
    const on = expanded.has(key);
    const text = on
      ? group === 'rest'
        ? '收起其余未用完 ' + count + ' 个包'
        : '收起已用完 ' + count + ' 个包'
      : group === 'rest'
        ? '其余未用完 ' + count + ' 个包（面额合计 ' + fmtTok(size) + ' · 剩余 ' + fmtTok(remain) + '），展开'
        : '已用完 ' + count + ' 个包（面额合计 ' + fmtTok(size) + '），展开';
    return { key, on, text };
  };

  const rest = groups.rest.length ? groupSummary('rest', groups.rest.length, groups.restSize, groups.restRemain) : null;
  const used = groups.used.length ? groupSummary('used', groups.used.length, groups.usedSize, 0) : null;

  return (
    <Box>
      <BoxHeader>
        <BoxTitle>
          {a.nickname || String(a.uid).slice(0, 8)} · {a.realm || ''}
        </BoxTitle>
        <Grow />
        <BoxNote>
          余额 {fmtTok(a.remain)} / 总额 {fmtTok(a.size)} · 可用 {groups.visible.length + groups.rest.length} 个包
          {groups.used.length ? ' / 已用完 ' + groups.used.length + ' 个' : ''} · 默认展示最早到期 {pkDetailLimitValue(limit)} 条（{PK_SORT_LABELS[mode] || ''}）
        </BoxNote>
      </BoxHeader>
      <TableWrap>
        <Table>
          <thead>
            <tr>
              <Th className="w-[3px] p-0" aria-hidden />
              <Th>包名 / 来源</Th>
              <Th className="text-right">面额</Th>
              <Th className="text-right">剩余</Th>
              <Th className="text-right">已用</Th>
              <Th className="text-right">发放</Th>
              <Th className="text-right">到期</Th>
            </tr>
          </thead>
          <tbody>
            {groups.visible.map(rowOf)}
            {rest ? (
              <>
                <tr className="bg-surface-2">
                  <Td colSpan={7}>
                    <button
                      type="button"
                      className={cn('w-full py-1 text-left text-ink-2 hover:text-accent')}
                      onClick={() => onToggle(rest.key)}
                    >
                      {rest.text}
                    </button>
                  </Td>
                </tr>
                {rest.on ? groups.rest.map(rowOf) : null}
              </>
            ) : null}
            {used ? (
              <>
                <tr className="bg-surface-2">
                  <Td colSpan={7}>
                    <button
                      type="button"
                      className={cn('w-full py-1 text-left text-ink-2 hover:text-accent')}
                      onClick={() => onToggle(used.key)}
                    >
                      {used.text}
                    </button>
                  </Td>
                </tr>
                {used.on ? groups.used.map(rowOf) : null}
              </>
            ) : null}
          </tbody>
        </Table>
      </TableWrap>
    </Box>
  );
}
