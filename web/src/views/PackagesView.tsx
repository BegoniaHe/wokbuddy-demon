import { useMemo, useState } from 'react';
import { useQuery } from '@tanstack/react-query';
import { api } from '@/lib/api';
import { fmtTok } from '@/lib/format';
import { dig } from '@/lib/config';
import { LoadError } from '@/components/LoadError';
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
  CardContent,
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
import { Legend } from '@/components/ui-kit';
import { remarkName } from '@/lib/remarks';

const LS_PK_SORT = 'pkSortMode';

// 逐包明细排序选项（labels 与 PK_SORT_LABELS 的 key 保持一致）。
const PK_SORT_OPTIONS = [
  { value: 'end_asc', label: '按到期时间' },
  { value: 'size_desc', label: '按面额大小' },
] as const satisfies readonly SelectOption[];

function readSortMode(): string {
  const v = localStorage.getItem(LS_PK_SORT) || 'end_asc';
  return v === 'size_desc' ? 'size_desc' : 'end_asc';
}

export function PackagesView() {
  const [sortMode, setSortMode] = useState(readSortMode);
  const [expanded, setExpanded] = useState<Set<string>>(new Set());

  const pkgQ = useQuery({
    queryKey: ['packages'],
    queryFn: () => api<{ accounts?: PkgAccount[]; grants?: Grant[] }>('packages'),
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
      <Card className="mb-5">
        <CardHeader className="items-center gap-3">
          <CardTitle>积分到期分布</CardTitle>
          <CardDescription>按批次到期日聚合，颜色区分账号</CardDescription>
        </CardHeader>
        <CardContent>
          <ExpiryDistribution list={list} now={now} />
        </CardContent>
      </Card>

      <Card className="mb-5">
        <CardHeader className="items-center gap-3">
          <CardTitle>获取历史</CardTitle>
          <CardAction>
            <CardDescription>
              {(pkgQ.data?.grants || []).length} 笔 · 快照 {new Date(pkgQ.dataUpdatedAt).toLocaleString()}
            </CardDescription>
          </CardAction>
        </CardHeader>
        <CardContent>
          <GrantHistory grants={pkgQ.data?.grants || []} />
        </CardContent>
      </Card>

      <Card className="mb-5">
        <CardHeader className="items-center gap-3">
          <CardTitle>账号对比</CardTitle>
          <CardAction>
            <div className="flex flex-wrap items-center gap-3">
              <CardDescription>{list.length ? list.length + ' 个账号 · 实时查询上游' : '—'}</CardDescription>
              <Select
                items={selectOptionItems(PK_SORT_OPTIONS)}
                value={sortMode}
                onValueChange={(v) => changeSort(v ?? 'end_asc')}>
                <SelectTrigger size="sm" className="w-32" aria-label="排序规则" title="逐包明细的排序规则">
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  <SelectOptions options={PK_SORT_OPTIONS} />
                </SelectContent>
              </Select>
              <Button size="xs" disabled={pkgQ.isFetching} onClick={() => void pkgQ.refetch()}>
                刷新
              </Button>
            </div>
          </CardAction>
        </CardHeader>
        <CardContent>
          {pkgQ.data !== undefined && pkgQ.error ? (
            <LoadError
              className="mb-3"
              message={'刷新失败：' + (pkgQ.error as Error).message}
              onRetry={() => void pkgQ.refetch()}
            />
          ) : null}
          {pkgQ.isLoading ? (
            <p className="py-4 text-center text-muted-foreground">查询中…（逐账号向上游实时查询）</p>
          ) : pkgQ.error && pkgQ.data === undefined ? (
            <LoadError
              variant="page"
              message={'读取失败：' + (pkgQ.error as Error).message}
              onRetry={() => void pkgQ.refetch()}
            />
          ) : list.length === 0 ? (
            <p className="py-4 text-center text-muted-foreground">没有账号</p>
          ) : (
            <ComparisonCards list={list} now={now} />
          )}
        </CardContent>
      </Card>

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
    return <div className="py-5 text-center text-xs text-muted-foreground">暂无可汇总积分</div>;
  }

  const legend = list
    .filter((a) => a && !a.error && a.uid && pkAccountSegments(a, now).some((s) => s.days != null))
    .map((a) => (
      <Legend key={a.uid} color={colors.get(String(a.uid)) || 'var(--primary)'}>
        {remarkName(a.uid, a.nickname || '').display}
      </Legend>
    ));

  const foot = summary.accountCount + ' 个账号' + (summary.unavailable ? ' · ' + summary.unavailable + ' 个未获取余额' : '');

  return (
    <>
      <div className="mb-1 grid grid-cols-[64px_minmax(80px,1fr)_72px] items-center gap-2.5 text-2xs text-muted-foreground">
        <span>剩余天数</span>
        <span className="text-center">各账号该批剩余</span>
        <b className="text-right font-semibold">剩余积分</b>
      </div>
      <div className="max-h-[260px] overflow-y-auto overflow-x-hidden pb-0 pr-2.5 [scrollbar-gutter:stable]">
        {summary.rows.map((row) => {
          const total = row.credits || 1;
          return (
            <div key={row.days} className="grid min-h-[30px] grid-cols-[64px_minmax(80px,1fr)_72px] items-center gap-2.5 pr-0.5">
              <span className="text-2xs text-muted-foreground">{row.days === 0 ? '已到期' : row.days + ' 天'}</span>
              <div className="flex h-[9px] items-stretch gap-0.5 overflow-hidden rounded-[5px] bg-secondary">
                {row.segments.map((segment, i) => {
                  const color = colors.get(segment.uid) || 'var(--primary)';
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
      {legend.length ? <div className="mt-2 flex flex-wrap gap-x-3 gap-y-1 text-2xs text-muted-foreground">{legend}</div> : null}
      <div className="mt-2 border-t border-border/60 pt-2 text-2xs text-muted-foreground">{foot}</div>
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
            <div key={a.uid} className="min-w-[200px] flex-1 rounded-lg border border-border/60 p-3">
              <div className="flex items-baseline gap-1.5">
                <span className="truncate text-sm font-semibold">{remarkName(a.uid, a.nickname || '').display}</span>
                <span className="font-mono text-2xs text-muted-foreground">{a.realm || ''}</span>
              </div>
              <div className="mt-1 text-xs text-warning">查询失败：{a.error}</div>
            </div>
          );
        }
        const srcs = pkBySource(a.packages || []);
        const total = Math.max(1, Number(a.size || 0));
        const expiry = pkAccountSegments(a, now);
        const expiryTotal = Math.max(1, expiry.reduce((s, x) => s + x.amount, 0));
        const expiryColor = expiryColors.get(String(a.uid)) || 'var(--primary)';
        return (
          <div key={a.uid} className="min-w-[200px] flex-1 rounded-lg border border-border/60 p-3">
            <div className="mb-1.5 flex items-center gap-1.5">
              <span className="truncate text-sm font-semibold" title={a.nickname || String(a.uid).slice(0, 8)}>
                {remarkName(a.uid, a.nickname || '').display}
              </span>
              {a.realm ? (
                <span className="flex-none rounded-md border border-border bg-secondary px-1.5 py-px text-2xs text-muted-foreground">
                  {a.realm}
                </span>
              ) : null}
            </div>
            <div className="font-mono text-2xl font-semibold tabular-nums">{fmtTok(a.remain)}</div>
            <div className="mt-0.5 text-2xs text-muted-foreground">
              共 {fmtTok(a.size)} · {(a.packages || []).length} 个包 · 占最高 {((Number(a.remain || 0) / maxRemain) * 100).toFixed(0)}%
            </div>
            <div className="mt-2 flex h-[7px] overflow-hidden rounded bg-secondary">
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
              <div className="mt-1.5 flex h-[5px] overflow-hidden rounded bg-secondary">
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
            <div className="mt-2 flex flex-col gap-y-0.5 text-2xs text-muted-foreground">
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
      <TableRow key={k + '-' + i}>
        <TableCell className="w-[3px] p-0">
          <span className="block h-full min-h-[36px] w-[3px]" style={{ background: colorOf(k) }} />
        </TableCell>
        <TableCell>
          {p.name || '(未命名)'}
          {sub ? <div className="text-2xs text-muted-foreground">{sub}</div> : null}
        </TableCell>
        <TableCell className="text-right font-mono tabular-nums">{fmtTok(p.size)}</TableCell>
        <TableCell className="text-right font-mono tabular-nums">{fmtTok(p.remain)}</TableCell>
        <TableCell className="text-right font-mono tabular-nums">{fmtTok(p.used)}</TableCell>
        <TableCell className="text-right font-mono tabular-nums">{(p.created_at || '').slice(0, 16).replace('T', ' ') || '—'}</TableCell>
        <TableCell className="text-right font-mono tabular-nums">{(p.end_time || '').slice(0, 10) || '—'}</TableCell>
      </TableRow>
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
    <Card className="mb-5">
      <CardHeader className="items-center gap-3">
        <CardTitle>
          {remarkName(a.uid, a.nickname || '').display} · {a.realm || ''}
        </CardTitle>
        <CardDescription>
          余额 {fmtTok(a.remain)} / 总额 {fmtTok(a.size)} · 可用 {groups.visible.length + groups.rest.length} 个包
          {groups.used.length ? ' / 已用完 ' + groups.used.length + ' 个' : ''} · 默认展示最早到期 {pkDetailLimitValue(limit)} 条（{PK_SORT_LABELS[mode] || ''}）
        </CardDescription>
      </CardHeader>
      <div className="overflow-x-auto" tabIndex={0} aria-label="逐包明细，可横向滚动">
        <Table containerClassName="overflow-visible">
          <TableHeader>
            <TableRow>
              <TableHead className="w-[3px] p-0" aria-hidden />
              <TableHead>包名 / 来源</TableHead>
              <TableHead className="text-right">面额</TableHead>
              <TableHead className="text-right">剩余</TableHead>
              <TableHead className="text-right">已用</TableHead>
              <TableHead className="text-right">发放</TableHead>
              <TableHead className="text-right">到期</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {groups.visible.map(rowOf)}
            {rest ? (
              <>
                <TableRow className="bg-secondary">
                  <TableCell colSpan={7}>
                    <button
                      type="button"
                      className="w-full py-1 text-left text-muted-foreground hover:text-foreground"
                      onClick={() => onToggle(rest.key)}
                    >
                      {rest.text}
                    </button>
                  </TableCell>
                </TableRow>
                {rest.on ? groups.rest.map(rowOf) : null}
              </>
            ) : null}
            {used ? (
              <>
                <TableRow className="bg-secondary">
                  <TableCell colSpan={7}>
                    <button
                      type="button"
                      className="w-full py-1 text-left text-muted-foreground hover:text-foreground"
                      onClick={() => onToggle(used.key)}
                    >
                      {used.text}
                    </button>
                  </TableCell>
                </TableRow>
                {used.on ? groups.used.map(rowOf) : null}
              </>
            ) : null}
          </TableBody>
        </Table>
      </div>
    </Card>
  );
}

type Grant = {
  uid: string;
  nickname?: string;
  realm?: string;
  name?: string;
  source?: string;
  inferred?: boolean;
  size?: number;
  remain?: number;
  created_at?: string;
  expires_at?: number;
  status?: string;
};

// GrantHistory 只读积分包获取历史（issue #100），数据来自现有积分包快照。
function GrantHistory({ grants }: { grants: Grant[] }) {
  if (!grants.length) return <p className="text-sm text-muted-foreground">暂无积分包数据。</p>;
  return (
    <div className="overflow-x-auto" tabIndex={0} aria-label="获取历史，可横向滚动">
      <Table containerClassName="overflow-visible">
        <TableHeader>
          <TableRow>
            <TableHead>发放时间</TableHead>
            <TableHead>账号</TableHead>
            <TableHead>realm</TableHead>
            <TableHead>名称</TableHead>
            <TableHead>来源</TableHead>
            <TableHead className="text-right">面额</TableHead>
            <TableHead className="text-right">剩余</TableHead>
            <TableHead>到期</TableHead>
            <TableHead>状态</TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {grants.map((g, i) => (
            <TableRow key={g.uid + '|' + (g.name || '') + '|' + (g.created_at || '') + '|' + i}>
              <TableCell className="whitespace-nowrap text-xs text-muted-foreground">
                {g.created_at ? g.created_at.replace('T', ' ').slice(0, 16) : '—'}
              </TableCell>
              <TableCell>{remarkName(g.uid, g.nickname || '').display}</TableCell>
              <TableCell className="text-muted-foreground">{g.realm || '—'}</TableCell>
              <TableCell>{g.name || '—'}</TableCell>
              <TableCell>
                {g.source || '未知'}
                {g.inferred ? <span className="ml-1 text-2xs text-muted-foreground">推断</span> : null}
              </TableCell>
              <TableCell className="text-right tabular-nums">{fmtTok(g.size || 0)}</TableCell>
              <TableCell className="text-right tabular-nums">{fmtTok(g.remain || 0)}</TableCell>
              <TableCell className="whitespace-nowrap text-xs text-muted-foreground">
                {g.expires_at ? new Date(g.expires_at).toLocaleDateString() : '不过期'}
              </TableCell>
              <TableCell className="text-xs">{g.status || '—'}</TableCell>
            </TableRow>
          ))}
        </TableBody>
      </Table>
    </div>
  );
}
