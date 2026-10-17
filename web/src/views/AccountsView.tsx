import { useQuery, useQueryClient } from '@tanstack/react-query';
import { api, postJSON } from '@/lib/api';
import type { Account, ModelLock, Overview, TaskRun } from '@/lib/types';
import {
  ago,
  dur,
  formatLatency,
  formatRate,
  formatTokenCount,
  parseAPITime,
  rateLimitMeta,
  type RateLimitRow,
} from '@/lib/format';
import { expBatches, expDaysLeft, type Package } from '@/lib/expiry';
import { fmtTok } from '@/lib/format';
import { toast } from '@/components/Toaster';
import { Button } from '@/components/ui/button';
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
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from '@/components/ui/dropdown-menu';
import { Tag, Chip, Bar, Dot, type Tone } from '@/components/ui-kit';
import { TasksDialog } from '@/components/TasksDialog';
import { useState } from 'react';
import { cn } from '@/lib/utils';
import { MoreHorizontal } from 'lucide-react';

// 账号池视图：统计条 + 积分到期提醒 + 账号表 + 模型锁池。
export function AccountsView() {
  const qc = useQueryClient();
  const [taskUID, setTaskUID] = useState<string | null>(null);

  const { data, error } = useQuery({
    queryKey: ['overview'],
    queryFn: () => api<Overview>('overview'),
    refetchInterval: 5000,
  });

  const runsQ = useQuery({
    queryKey: ['tasks/runs'],
    queryFn: () => api<{ runs?: TaskRun[] }>('tasks/runs').catch((): { runs?: TaskRun[] } => ({ runs: [] })),
    refetchInterval: 3000,
  });
  const runOf = (task: string): TaskRun | undefined => (runsQ.data?.runs || []).find((r) => r.task === task);
  const startBatch = async (path: string, label: string) => {
    try {
      await postJSON(path);
      toast(label + '已开始，结果见日志', 'ok');
    } catch (e) {
      toast((e as Error).message, 'err');
    } finally {
      void runsQ.refetch();
    }
  };

  const pkgQuery = useQuery({
    queryKey: ['packages'],
    queryFn: () => api<{ accounts?: PackageAccount[] }>('packages'),
    enabled: false,
    staleTime: 2 * 60 * 1000,
  });

  if (error && !data) {
    return <Card><CardContent className="py-10 text-center text-muted-foreground">读取失败：{(error as Error).message}</CardContent></Card>;
  }
  if (!data) {
    return <Card><CardContent className="py-10 text-center text-muted-foreground">加载中…</CardContent></Card>;
  }

  const accounts = data.accounts ?? [];
  const remSum = accounts.reduce((a, s) => a + (s.credits || 0), 0);
  const totSum = accounts.reduce((a, s) => a + (s.credits_total || 0), 0);
  const creditsText = totSum > 0 ? remSum + ' / ' + totSum : String(remSum);

  const afterAction = () => {
    void qc.invalidateQueries({ queryKey: ['overview'] });
  };

  return (
    <>
      <Stats
        stats={[
          { value: data.total, label: '账号总数' },
          { value: data.healthy, label: '可用', tone: 'good' },
          { value: data.cooling, label: '冷却中', tone: 'warn' },
          { value: data.disabled, label: '已禁用', tone: 'bad' },
          { value: data.paused ?? '-', label: '已暂停选号', tone: 'warn' },
          { value: creditsText, label: '积分剩余 / 总额' },
          { value: data.sticky_sessions, label: '粘性会话' },
        ]}
      />

      <ExpiryBox
        overview={data}
        detail={pkgQuery.data}
        fetchedAt={pkgQuery.dataUpdatedAt}
        checking={pkgQuery.isFetching}
        onCheck={() => void pkgQuery.refetch()}
      />

      <Card>
        <CardHeader className="flex-row flex-wrap items-center gap-3">
          <CardTitle>账号池</CardTitle>
          <CardAction>
            <div className="flex items-center gap-2">
              {data.in_flight_full ? (
                <span className="text-xs text-muted-foreground">{data.in_flight_full} 个账号在途占满</span>
              ) : null}
              <BatchButton run={runOf('checkin')} label="全部签到" onClick={() => void startBatch('checkin_all', '全部签到')} />
              <BatchButton run={runOf('travel')} label="旅行巡检" onClick={() => void startBatch('travel_all', '旅行巡检')} />
              <BatchButton run={runOf('activity')} label="活跃上报" onClick={() => void startBatch('activity_all', '活跃上报')} />
              <BatchButton run={runOf('keepalive')} label="全部保活" onClick={() => void startBatch('keepalive_all', '全部保活')} />
              <BatchButton run={runOf('blackcat')} label="夜猫子" onClick={() => void startBatch('blackcat_all', '夜猫子补足')} />
              <BatchButton run={runOf('streak')} label="连登管家" onClick={() => void startBatch('streak_bonus_all', '连登管家')} />
            </div>
          </CardAction>
        </CardHeader>
        <div className="overflow-x-auto">
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead className="w-[3px] p-0" aria-hidden />
                <TableHead>账号</TableHead>
                <TableHead>状态</TableHead>
                <TableHead>积分</TableHead>
                <TableHead>成功 / 失败</TableHead>
                <TableHead>在途</TableHead>
                <TableHead>用量</TableHead>
                <TableHead>最近成功</TableHead>
                <TableHead />
              </TableRow>
            </TableHeader>
            <TableBody>
              {accounts.length === 0 ? (
                <TableRow>
                  <TableCell colSpan={9} className="py-10 text-center text-muted-foreground">
                    <div className="mb-1.5 text-sm text-foreground">账号池是空的</div>
                    点击右上角「添加账号」，用浏览器登录一个 WorkBuddy 账号
                  </TableCell>
                </TableRow>
              ) : (
                accounts.map((s) => (
                  <AccountRow
                    key={s.uid}
                    s={s}
                    maxCred={Math.max(1, ...accounts.map((x) => x.credits || 0))}
                    proxySlots={data.proxy_slots || []}
                    onOpenTasks={() => setTaskUID(s.uid)}
                    onDone={afterAction}
                  />
                ))
              )}
            </TableBody>
          </Table>
        </div>
      </Card>

      <ModelLocks rows={data.model_locks} />

      <TasksDialog uid={taskUID} onClose={() => setTaskUID(null)} />
    </>
  );
}

interface PackageAccount {
  uid?: string;
  nickname?: string;
  error?: string;
  packages?: Package[];
}

interface StatItem {
  value: React.ReactNode;
  label: string;
  tone?: 'default' | 'good' | 'warn' | 'bad';
}

// Stats：顶部统计条。窄屏降为 3 / 2 列。
function Stats({ stats }: { stats: StatItem[] }) {
  return (
    <div className="mb-5 grid grid-cols-2 overflow-hidden rounded-xl border border-border bg-card sm:grid-cols-3 lg:grid-cols-7">
      {stats.map((s) => (
        <div key={s.label} className="border-b border-r border-border/60 px-4 py-3.5 last:border-b-0 lg:border-b-0 lg:last:border-r-0">
          <div
            className={cn(
              'whitespace-nowrap font-mono text-[26px] font-semibold leading-tight tracking-tight tabular-nums',
              s.tone === 'good' && 'text-success',
              s.tone === 'warn' && 'text-warning',
              s.tone === 'bad' && 'text-destructive',
            )}
          >
            {s.value}
          </div>
          <div className="mt-1.5 text-2xs text-muted-foreground">{s.label}</div>
        </div>
      ))}
    </div>
  );
}

function ExpiryBox({
  overview,
  detail,
  fetchedAt,
  checking,
  onCheck,
}: {
  overview: Overview;
  detail?: { accounts?: PackageAccount[] };
  fetchedAt: number;
  checking: boolean;
  onCheck: () => void;
}) {
  const today = new Date();
  today.setHours(0, 0, 0, 0);

  let rows: React.ReactNode;
  let note: string;

  if (detail) {
    const list = detail.accounts ?? [];
    const keyed = list.map((a) => {
      let key: string | null = null;
      if (!a.error) {
        const bs = expBatches(a.packages).filter((b) => expDaysLeft(b.date, today) >= 0);
        if (bs.length) key = bs[0]!.date;
      }
      return { a, key };
    });
    keyed.sort((x, y) => {
      if (x.key === y.key) return 0;
      if (x.key === null) return 1;
      if (y.key === null) return -1;
      return x.key < y.key ? -1 : 1;
    });
    rows = keyed.map(({ a }) => {
      const nm = a.nickname || String(a.uid || '').slice(0, 8);
      if (a.error) {
        return (
          <div key={nm} className="flex items-start gap-2.5 border-t border-border/60 py-2 text-xs first:border-t-0">
            <Dot tone="mute" className="mt-1 size-2.5" />
            <span className="min-w-[9em] flex-none font-semibold">{nm}</span>
            <span className="text-destructive">查询失败：{a.error}</span>
          </div>
        );
      }
      const bs = expBatches(a.packages).filter((b) => expDaysLeft(b.date, today) >= 0);
      if (!bs.length) {
        return (
          <div key={nm} className="flex items-start gap-2.5 border-t border-border/60 py-2 text-xs first:border-t-0">
            <Dot tone="ok" className="mt-1 size-2.5" />
            <span className="min-w-[9em] flex-none font-semibold">{nm}</span>
            <span className="text-muted-foreground">7 天内无到期积分</span>
          </div>
        );
      }
      const first = bs[0]!;
      const days = expDaysLeft(first.date, today);
      const daily = Math.ceil(first.remain / Math.max(1, days));
      const week = bs.filter((b) => expDaysLeft(b.date, today) <= 7).reduce((s, b) => s + b.remain, 0);
      const dotTone: Tone = days <= 3 ? 'bad' : days <= 7 ? 'warn' : 'ok';
      const dayWord = days === 0 ? '今天到期' : days === 1 ? '明天到期' : days + ' 天后到期';
      const more = bs.length > 4 ? '　等 ' + bs.length + ' 批' : '';
      const rest = bs.slice(1, 4).map((b) => '随后 ' + b.date.slice(5) + ' · ' + fmtTok(b.remain)).join('　') + more;
      return (
        <div key={nm} className="flex items-start gap-2.5 border-t border-border/60 py-2 text-xs first:border-t-0">
          <Dot tone={dotTone} className="mt-1 size-2.5" />
          <span className="min-w-[9em] flex-none font-semibold">{nm}</span>
          <span className="text-muted-foreground">
            最近到期 <b className="font-mono">{first.date}</b>（{dayWord}）· 该批{' '}
            <b className="font-mono">{fmtTok(first.remain)}</b> 积分 · 到期前日均需耗 ≥
            <b className="font-mono">{fmtTok(daily)}</b>
            {week > first.remain ? ' · 7 天内合计 ' + fmtTok(week) : ''}
            {rest ? <div className="mt-0.5 text-2xs text-muted-foreground/80">{rest}</div> : null}
          </span>
        </div>
      );
    });
    const ageMin = fetchedAt ? Math.floor((Date.now() - fetchedAt) / 60000) : 0;
    note = ageMin > 0 ? list.length + ' 个账号 · ' + ageMin + ' 分钟前的数据，可点「检查」刷新' : list.length + ' 个账号 · 实时查询上游';
  } else {
    const list = overview.accounts ?? [];
    const keyed = list.map((a) => {
      let key: number | null = null;
      let ms = 0;
      if (a.credits_earliest_expiry) {
        const t = new Date(a.credits_earliest_expiry);
        if (!isNaN(t.getTime())) {
          ms = t.getTime();
          key = ms;
        }
      }
      return { a, key, ms };
    });
    keyed.sort((x, y) => {
      if (x.key === null && y.key === null) return 0;
      if (x.key === null) return 1;
      if (y.key === null) return -1;
      return x.key! - y.key!;
    });
    rows = keyed.map(({ a, ms }) => {
      const nm = a.nickname || String(a.uid || '').slice(0, 8);
      if (!ms || !a.credits_earliest_remaining) {
        return (
          <div key={nm} className="flex items-start gap-2.5 border-t border-border/60 py-2 text-xs first:border-t-0">
            <Dot tone="ok" className="mt-1 size-2.5" />
            <span className="min-w-[9em] flex-none font-semibold">{nm}</span>
            <span className="text-muted-foreground">无近期到期批次</span>
          </div>
        );
      }
      const days = Math.max(0, Math.ceil((ms - today.getTime()) / 86400000));
      const remain = Number(a.credits_earliest_remaining || 0);
      const daily = Math.ceil(remain / Math.max(1, days));
      const dotTone: Tone = days <= 3 ? 'bad' : days <= 7 ? 'warn' : 'ok';
      const dayWord = days === 0 ? '今天到期' : days === 1 ? '明天到期' : days + ' 天后到期';
      const when = new Date(ms).toISOString().slice(0, 10);
      return (
        <div key={nm} className="flex items-start gap-2.5 border-t border-border/60 py-2 text-xs first:border-t-0">
          <Dot tone={dotTone} className="mt-1 size-2.5" />
          <span className="min-w-[9em] flex-none font-semibold">{nm}</span>
          <span className="text-muted-foreground">
            最近到期 <b className="font-mono">{when}</b>（{dayWord}）· 该批{' '}
            <b className="font-mono">{fmtTok(remain)}</b> 积分 · 到期前日均需耗 ≥
            <b className="font-mono">{fmtTok(daily)}</b>
            <div className="mt-0.5 text-2xs text-muted-foreground/80">仅最早一批，点「检查」看全部到期批次</div>
          </span>
        </div>
      );
    });
    note = list.length + ' 个账号 · 快照（来自本地状态，点「检查」拉实时明细）';
  }

  return (
    <Card className="mb-5">
      <CardHeader className="flex-row flex-wrap items-center gap-3">
        <CardTitle className="flex items-baseline">
          积分到期提醒
          <CardDescription className="ml-1.5">按最近到期批次估算 · 日均需耗 = 批次剩余 ÷ 距到期天数 · 上游按失效时刻先后自动优先扣减（FEFO）</CardDescription>
        </CardTitle>
        <CardAction>
          <div className="flex items-center gap-3">
            <span className="text-xs text-muted-foreground">{note}</span>
            <Button size="xs" disabled={checking} onClick={onCheck}>
              {checking ? '查询中…' : '检查'}
            </Button>
          </div>
        </CardAction>
      </CardHeader>
      <CardContent>{rows && (rows as unknown[])?.length ? rows : <p className="py-4 text-center text-muted-foreground">没有账号</p>}</CardContent>
    </Card>
  );
}

function AccountRow({
  s,
  maxCred,
  proxySlots,
  onOpenTasks,
  onDone,
}: {
  s: Account;
  maxCred: number;
  proxySlots: { id: string; name: string }[];
  onOpenTasks: () => void;
  onDone: () => void;
}) {
  const now = Date.now();
  const bl = (new Date(s.breaker_until || 0).getTime() - now) / 1000;
  const dg = (new Date(s.degrade_until || 0).getTime() - now) / 1000;
  const cool = Math.max(s.cool_remaining_sec || 0, bl > 0 ? bl : 0, dg > 0 ? dg : 0);
  let cls = '';
  let tag: React.ReactNode;
  if (s.disabled) {
    cls = 'opacity-70';
    tag = <Tag tone="bad">已禁用</Tag>;
  } else if (s.paused) {
    cls = 'opacity-70';
    tag = <Tag tone="warn">已暂停选号</Tag>;
  } else if (cool > 0) {
    cls = '';
    const kind =
      bl > Math.max(s.cool_remaining_sec || 0, dg > 0 ? dg : 0)
        ? '熔断'
        : dg > (s.cool_remaining_sec || 0)
          ? '连败降权'
          : s.cool_kind === 'hard_credit'
            ? '积分冷却'
            : '限流冷却';
    tag = (
      <Tag tone="warn">
        {kind} · {dur(cool)}
      </Tag>
    );
  } else tag = <Tag tone="ok">可用</Tag>;

  const short = s.uid.length > 16 ? s.uid.slice(0, 16) + '…' : s.uid;
  const unlimited = s.credits_total === -1;
  const cred = unlimited
    ? '不限'
    : s.credits == null
      ? '—'
      : s.credits_total && s.credits_total > 0
        ? s.credits + '/' + s.credits_total
        : String(s.credits);
  const pct = unlimited
    ? 100
    : s.credits_total && s.credits_total > 0
      ? Math.min(100, Math.round(((s.credits || 0) / s.credits_total) * 100))
      : Math.round(((s.credits || 0) / maxCred) * 100);

  let credTip: string;
  if (unlimited) credTip = '企业版不限量（上游 limitNum=-1）';
  else if (s.credits_total && s.credits_total > 0) {
    credTip =
      (s.enterprise ? '企业版剩余额度 ' : '剩余 ') +
      s.credits +
      ' / ' +
      (s.enterprise ? '分配 ' : '总额 ') +
      s.credits_total +
      '（' +
      pct +
      '%）';
  } else credTip = '积分（相对池内最高）';
  const costs = (s.model_costs || []).filter((c) => c.model);
  if (costs.length) {
    credTip +=
      '\n实测单价（credits/1K）：\n' +
      costs.map((c) => '  ' + c.model + '：' + ((c.cost_per_1k ?? 0) <= 0 ? '免费' : c.cost_per_1k)).join('\n');
  }

  const frozen = s.disabled || cool > 0;
  // 单号任务入口能力门控（issue #108）：global 无 CN 签到/任务体系，企业版无个人
  // 成长/签到体系；与后端 taskDenyReason、任务中心的扫描/队列门控同口径。
  const canUseTasks = !s.enterprise && s.realm !== 'global';
  const tu = s.token_usage || {};
  const req = tu.request_count || 0;
  const totalTok = formatTokenCount(tu.total_tokens);
  const latency = formatLatency(tu.last_latency_ms);
  const rate = formatRate(tu.last_tokens_per_second);
  const usageTitle = '最近一次：' + req + ' 次 / ' + totalTok + ' / 延迟 ' + latency + ' / ' + rate;

  const act = async (a: string) => {
    const u = s.uid;
    if (a === 'remove' && !confirm('移除账号将删除池状态与 auths/ 下的凭证文件，且不可恢复。确认移除？')) return;
    if (
      a === 'disable' &&
      !confirm(
        '禁用后该账号不再参与选号（保号任务默认也跳过），需手动解冻才能恢复。若只是想临时让位、仍要保号，请改用「暂停选号」。确认禁用？',
      )
    )
      return;
    try {
      if (a === 'checkin') {
        const r = await postJSON<{ credits?: number; credits_total?: number; checkin_message?: string }>(
          'accounts/' + encodeURIComponent(u) + '/checkin',
        );
        toast(
          '签到完成' +
            (r.credits != null
              ? '，积分 ' + r.credits + (r.credits_total && r.credits_total > 0 ? '/' + r.credits_total : '')
              : '') +
            (r.checkin_message ? '（' + r.checkin_message + '）' : ''),
          'ok',
        );
      } else if (a === 'balance') {
        const r = await postJSON<{ credits?: number; credits_total?: number }>(
          'accounts/' + encodeURIComponent(u) + '/balance',
        );
        toast('余额已更新：' + r.credits + (r.credits_total && r.credits_total > 0 ? ' / ' + r.credits_total : ''), 'ok');
      } else if (a === 'revive') {
        await postJSON('accounts/' + encodeURIComponent(u) + '/revive');
        toast('已解冻', 'ok');
      } else if (a === 'disable') {
        await postJSON('accounts/' + encodeURIComponent(u) + '/disable');
        toast('已禁用', 'ok');
      } else if (a === 'pause') {
        await postJSON('accounts/' + encodeURIComponent(u) + '/pause');
        toast('已暂停选号（签到 / 保活照常）', 'ok');
      } else if (a === 'resume') {
        await postJSON('accounts/' + encodeURIComponent(u) + '/resume');
        toast('已恢复选号', 'ok');
      } else if (a === 'remove') {
        const r = await postJSON<{ file_error?: string }>('accounts/' + encodeURIComponent(u) + '/remove');
        toast(r.file_error ? '已移除（凭证文件删除失败：' + r.file_error + '）' : '已移除', 'ok');
      }
    } catch (e) {
      toast((e as Error).message, 'err');
    }
    onDone();
  };

  const limits = (s.rate_limited_models || []).filter((r) => r && r.model);
  const railTone = s.disabled || s.paused ? 'bad' : cool > 0 ? 'warn' : 'ok';

  return (
    <TableRow className={cls} title={'uid: ' + s.uid}>
      <TableCell className="w-[3px] p-0">
        <span className={cn('block h-full min-h-[36px] w-[3px]', railBg[railTone])} />
      </TableCell>
      <TableCell>
        <div className="flex items-center gap-1.5">
          <span className="truncate font-medium" title={s.nickname || undefined}>
            {s.nickname ? s.nickname : <span className="text-muted-foreground">未命名</span>}
          </span>
          {s.realm === 'global' ? <span className="flex-none rounded-md border border-border bg-secondary px-1.5 py-px text-2xs text-muted-foreground">国际版</span> : null}
          {s.enterprise ? <span className="flex-none rounded-md border border-border bg-secondary px-1.5 py-px text-2xs text-muted-foreground">企业版</span> : null}
          {proxySlots.length ? (
            <select
              className="flex-none rounded-md border border-border bg-secondary px-1 py-px text-2xs text-muted-foreground"
              title="出站代理槽"
              value={s.proxy_slot || ''}
              onChange={(e) => void bindProxySlot(s.uid, e.target.value, onDone)}
            >
              <option value="">默认出口</option>
              {proxySlots.map((slot) => (
                <option key={slot.id} value={slot.id}>
                  {slot.name || slot.id}
                </option>
              ))}
            </select>
          ) : null}
        </div>
        <div className="mt-px truncate font-mono text-2xs text-muted-foreground">{short}</div>
      </TableCell>
      <TableCell>
        {tag}
        {s.reason ? <div className="mt-0.5 text-2xs text-muted-foreground">{s.reason}</div> : null}
        {limits.length ? (
          <div className="mt-1.5 flex min-w-[210px] flex-col gap-1">
            {limits.map((row, i) => {
              const m = rateLimitMeta(row as RateLimitRow, now);
              return (
                <div
                  key={i}
                  title={m.title}
                  className={
                    'flex items-baseline gap-1.5 rounded-md border px-1.5 py-1 text-2xs leading-tight ' +
                    (m.kind === 'model_unavailable'
                      ? 'border-border bg-secondary text-muted-foreground'
                      : 'border-warning/25 bg-warning-soft')
                  }
                >
                  <b className="font-mono font-semibold text-foreground">{m.model}</b>
                  <span className="text-muted-foreground">{m.detail}</span>
                </div>
              );
            })}
          </div>
        ) : null}
      </TableCell>
      <TableCell title={credTip}>
        <div className="font-mono text-sm tabular-nums">{cred}</div>
        <Bar pct={pct} className="mt-1 max-w-[88px]" />
      </TableCell>
      <TableCell>
        <span className="font-mono tabular-nums whitespace-nowrap">
          {s.success_count || 0} <span className="text-muted-foreground">/</span>{' '}
          <span className="text-destructive">{s.err_total || 0}</span>
        </span>
      </TableCell>
      <TableCell>
        <span className="font-mono tabular-nums">{s.in_flight || 0}</span>
      </TableCell>
      <TableCell title={usageTitle}>
        <span className="inline-flex flex-wrap items-center gap-1">
          <Chip tone="accent" unit="次">{req}</Chip>
          <Chip tone="accent" unit={totalTok !== '—' ? 'tok' : undefined}>{totalTok}</Chip>
          <Chip tone="warn">{latency}</Chip>
          <Chip tone="ok">{rate}</Chip>
        </span>
      </TableCell>
      <TableCell>
        <span className="font-mono text-xs tabular-nums text-muted-foreground">{ago(s.last_success)}</span>
      </TableCell>
      <TableCell>
        <div className="flex justify-end items-center gap-1.5 whitespace-nowrap">
          {canUseTasks ? (
            <Button size="xs" variant="ghost" title={s.checkin_done ? '今日已签到；点击可重新签到并刷新余额' : undefined} onClick={() => void act('checkin')}>
              {s.checkin_done ? '已签' : '签到'}
            </Button>
          ) : null}
          <Button size="xs" variant="ghost" title={s.enterprise ? '刷新企业版已分配额度（上游 get-enterprise-user-usage）' : undefined} onClick={() => void act('balance')}>
            {s.enterprise ? '额度' : '余额'}
          </Button>
          {canUseTasks ? (
            <Button size="xs" variant="ghost" onClick={onOpenTasks}>
              任务
            </Button>
          ) : null}
          <DropdownMenu>
            <DropdownMenuTrigger
              render={
                <Button size="xs" variant="ghost" title="更多操作">
                  <MoreHorizontal />
                </Button>
              }
            />
            <DropdownMenuContent align="end">
              {frozen ? (
                <DropdownMenuItem onClick={() => void act('revive')}>解冻</DropdownMenuItem>
              ) : s.paused ? (
                <DropdownMenuItem onClick={() => void act('resume')}>恢复选号</DropdownMenuItem>
              ) : (
                <DropdownMenuItem onClick={() => void act('pause')} title={s.enterprise ? '退出选号，但照常保活 / 刷新额度' : '退出选号，但照常签到 / 活跃上报 / 保活 / 刷新余额'}>
                  暂停选号
                </DropdownMenuItem>
              )}
              {s.disabled ? null : (
                <DropdownMenuItem onClick={() => void act('disable')}>禁用</DropdownMenuItem>
              )}
              <DropdownMenuSeparator />
              <DropdownMenuItem variant="destructive" onClick={() => void act('remove')}>
                移除
              </DropdownMenuItem>
            </DropdownMenuContent>
          </DropdownMenu>
        </div>
      </TableCell>
    </TableRow>
  );
}

const railBg = {
  ok: 'bg-success',
  warn: 'bg-warning',
  bad: 'bg-destructive',
} as const;

function ModelLocks({ rows }: { rows?: ModelLock[] }) {
  const list = rows ?? [];
  const STATE: Record<string, [string, string]> = {
    locked: ['bad', '整池不可用'],
    starved: ['warn', '没号可用'],
    partial: ['warn', '部分限流'],
  };
  const left = (iso?: string) => {
    const ms = parseAPITime(iso);
    return ms ? dur(Math.max(0, Math.round((ms - Date.now()) / 1000))) : '—';
  };
  const bad = list.filter((r) => r.state === 'locked' || r.state === 'starved').length;
  return (
    <Card className="mt-5">
      <CardHeader className="flex-row flex-wrap items-center gap-3">
        <CardTitle className="flex items-baseline">
          模型锁池
          <CardDescription className="ml-1.5">哪些模型不能用、还要锁多久</CardDescription>
        </CardTitle>
        <CardAction>
          <span className="text-xs text-muted-foreground">{list.length ? (bad ? bad + ' 个模型整池不可用' : list.length + ' 个模型部分限流') : ''}</span>
        </CardAction>
      </CardHeader>
      <div className="overflow-x-auto">
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead>模型</TableHead>
              <TableHead>域</TableHead>
              <TableHead>状态</TableHead>
              <TableHead>可选 / 总数</TableHead>
              <TableHead>锁定账号</TableHead>
              <TableHead>最早解锁</TableHead>
              <TableHead>全池解锁</TableHead>
              <TableHead>原因</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {list.length === 0 ? (
              <TableRow>
                <TableCell colSpan={8} className="py-10 text-center text-muted-foreground">当前没有模型级限流 —— 所有模型均可选</TableCell>
              </TableRow>
            ) : (
              list.map((r, i) => {
                const st = STATE[r.state || ''] || ['mute', r.state || '—'];
                const realm = r.realm === 'global' ? '国际版' : '国内版';
                return (
                  <TableRow key={i}>
                    <TableCell>{r.model}</TableCell>
                    <TableCell>
                      <span className="rounded-md border border-border bg-secondary px-1.5 py-px text-2xs text-muted-foreground">{realm}</span>
                    </TableCell>
                    <TableCell>
                      <Tag tone={st[0] as 'bad' | 'warn' | 'mute'}>{st[1]}</Tag>
                    </TableCell>
                    <TableCell>
                      <span className="font-mono tabular-nums">
                        {r.servable || 0} / {r.total || 0}
                      </span>
                    </TableCell>
                    <TableCell>
                      <span className="font-mono tabular-nums">{r.locked || 0}</span>
                    </TableCell>
                    <TableCell>
                      <span className="font-mono tabular-nums">{left(r.unlock_at || r.fully_unlock_at)}</span>
                    </TableCell>
                    <TableCell>
                      <span className="font-mono tabular-nums">{left(r.fully_unlock_at)}</span>
                    </TableCell>
                    <TableCell>{r.reason ? <span className="text-muted-foreground">{r.reason}</span> : '—'}</TableCell>
                  </TableRow>
                );
              })
            )}
          </TableBody>
        </Table>
      </div>
    </Card>
  );
}

// BatchButton：批量任务按钮。运行中禁用并显示当前轮来源/账号数；结束后 title
// 展示最近一轮的成功/失败摘要（issue #112）。
function BatchButton({
  run,
  label,
  onClick,
}: {
  run?: TaskRun;
  label: string;
  onClick: () => void;
}) {
  const running = !!run?.running;
  const summary = run && !running && (run.total ?? 0) > 0
    ? '最近一轮：' + (run.total ?? 0) + ' 个账号，成功 ' + (run.success ?? 0) + ' / 失败 ' + (run.failure ?? 0)
    : undefined;
  return (
    <Button
      size="xs"
      variant={running ? 'secondary' : 'default'}
      disabled={running}
      title={running ? '运行中（' + (run?.trigger || 'manual') + ' 触发）' : summary}
      onClick={onClick}>
      {running ? label + '中…' : label}
    </Button>
  );
}

// bindProxySlot 绑定账号的出站代理槽（issue #96）。
async function bindProxySlot(uid: string, slot: string, onDone: () => void) {
  try {
    await postJSON('accounts/' + encodeURIComponent(uid) + '/proxy_slot', { slot });
    toast(slot ? '已绑定代理槽' : '已恢复默认出口', 'ok');
  } catch (e) {
    toast('绑定失败：' + (e as Error).message, 'err');
  }
  onDone();
}
