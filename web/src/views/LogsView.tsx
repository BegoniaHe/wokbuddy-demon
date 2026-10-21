import { useEffect, useMemo, useRef, useState } from 'react';
import { useQuery } from '@tanstack/react-query';
import { api } from '@/lib/api';
import { fmtClockTime, fmtMs, fmtTok } from '@/lib/format';
import {
  fmtBytes,
  reqCreditCell,
  reqMatch,
  reqOutcomeLabel,
  reqOutcomeTone,
  reqTokenCell,
  requestLogText,
  type RequestEntry,
} from '@/lib/reqlog';
import { defaultTimeRangeState, trangeQuery, type TimeRangeState } from '@/lib/timeRange';
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
import { Tabs, TabsList, TabsTrigger } from '@/components/ui/tabs';
import { Tag } from '@/components/ui-kit';
import { TimeRange } from '@/components/TimeRange';
import { Input } from '@/components/ui/input';
import { cn } from '@/lib/utils';
import { chDelta, chName, chTime, chTone, type CreditHistoryEntry } from '@/lib/creditHistory';

interface LogEntry {
  ts?: string;
  text: string;
  ch?: string;
}

interface RequestMetrics {
  completed?: number;
  success_rate?: number;
  http_success_rate?: number;
  avg_duration_ms?: number;
  in_flight?: number;
  archive?: { enabled?: boolean; bytes?: number; dropped_writes?: number; last_error?: string };
  recent?: RequestEntry[];
}

const CH_LABEL: Record<string, string> = { task: '任务', chat: '对话', sys: '系统' };

// 请求记录筛选选项：首项为「不筛选」哨兵，选中即空筛选条件。
const OUTCOME_OPTIONS = [
  { value: '__all__', label: '全部结果' },
  { value: 'success', label: '成功' },
  { value: 'http_error', label: 'HTTP 错误' },
  { value: 'stream_error', label: '流错误' },
  { value: 'interrupted', label: '中断' },
] as const satisfies readonly SelectOption[];

const LIMIT_OPTIONS = [
  { value: '100', label: '最近 100 条' },
  { value: '300', label: '最近 300 条' },
  { value: '1000', label: '最近 1000 条' },
] as const satisfies readonly SelectOption[];

const OUTCOME_ALL = OUTCOME_OPTIONS[0].value;

export function LogsView() {
  const [range, setRange] = useState<TimeRangeState>(() => defaultTimeRangeState('0'));
  const [limit, setLimit] = useState('100');
  const [ch, setCh] = useState('all');
  const [q, setQ] = useState('');
  const [outcome, setOutcome] = useState('');
  const [qDebounced, setQDebounced] = useState('');
  const [pin, setPin] = useState(true);
  const logBoxRef = useRef<HTMLPreElement>(null);

  useEffect(() => {
    const t = setTimeout(() => setQDebounced(q.trim()), 150);
    return () => clearTimeout(t);
  }, [q]);

  const rangeStr = trangeQuery(range, false).toString();

  const logsQ = useQuery({
    queryKey: ['logs'],
    queryFn: () => api<{ entries?: LogEntry[] }>('logs'),
    refetchInterval: 5000,
  });

  // 积分历史（本地账本）：进页面取一次，**不挂 5s 轮询**——账本变动低频，
  // 轮询只会白白增加面板负载。
  const creditHistQ = useQuery({
    queryKey: ['credit_history'],
    queryFn: () =>
      api<{ entries?: CreditHistoryEntry[] }>('credit_history?limit=100').catch((): { entries?: CreditHistoryEntry[] } => ({})),
    refetchInterval: false,
    staleTime: 30000,
  });

  const reqQ = useQuery({
    queryKey: ['reqlog', rangeStr, limit],
    queryFn: async () => {
      const rq = trangeQuery(range, false);
      rq.set('limit', limit);
      const [metrics, requestRows] = await Promise.all([
        api<RequestMetrics>('request_metrics').catch(() => ({}) as RequestMetrics),
        api<{ entries?: RequestEntry[] }>('request_logs?' + rq.toString()).catch(() => ({ entries: [] })),
      ]);
      const archiveOn = !!metrics.archive?.enabled;
      const recent = archiveOn ? requestRows.entries || [] : metrics.recent || [];
      return { metrics, entries: recent };
    },
  });

  const entries = reqQ.data?.entries ?? [];
  const metrics = reqQ.data?.metrics ?? {};

  const filtered = useMemo(
    () => entries.filter((e) => reqMatch(e, { q: qDebounced, outcome })),
    [entries, qDebounced, outcome],
  );
  const hasSource = entries.some((e) => e.client_ip || e.user_agent);

  // 终端日志：自动滚动到底部（仅当已在底部）。
  const logEntries = (logsQ.data?.entries ?? []).filter((e) => ch === 'all' || e.ch === ch);
  useEffect(() => {
    const box = logBoxRef.current;
    if (!box || !pin) return;
    const atEnd = box.scrollTop + box.clientHeight >= box.scrollHeight - 24;
    if (atEnd) box.scrollTop = box.scrollHeight;
  }, [logEntries, pin]);

  const counts: Record<string, number> = {};
  for (const e of logsQ.data?.entries ?? []) counts[e.ch || ''] = (counts[e.ch || ''] || 0) + 1;
  const logNote =
    ch === 'all'
      ? '任务 ' + (counts.task || 0) + ' · 对话 ' + (counts.chat || 0) + ' · 系统 ' + (counts.sys || 0)
      : (CH_LABEL[ch] || ch) + ' ' + logEntries.length + ' 行';

  const archive = metrics.archive || {};
  const reqSummary =
    '已完成 ' +
    fmtTok(metrics.completed) +
    ' · 成功 ' +
    (metrics.success_rate == null ? '—' : Number(metrics.success_rate).toFixed(1) + '%') +
    ' · HTTP ' +
    (metrics.http_success_rate == null ? '—' : Number(metrics.http_success_rate).toFixed(1) + '%') +
    ' · 平均 ' +
    fmtMs(metrics.avg_duration_ms) +
    ' · 进行中 ' +
    String(metrics.in_flight || 0);
  const reqNote = archive.enabled
    ? 'JSONL 归档 ' +
      fmtBytes(archive.bytes) +
      (archive.dropped_writes ? ' · 丢弃 ' + archive.dropped_writes + ' 条' : '') +
      (archive.last_error ? ' · 错误：' + archive.last_error : '')
    : '仅内存指标，JSONL 归档已关闭';

  return (
    <>
      <Card className="mb-5">
        <CardHeader className="flex flex-wrap items-center justify-between gap-3">
          <CardTitle className="flex items-baseline">
            请求记录
            <span className="ml-1.5 text-2xs font-normal text-muted-foreground">调用来源：IP 取 X-Forwarded-For 首段，无代理头时回落 TCP 对端</span>
          </CardTitle>
          <CardAction>
            <div className="flex flex-wrap items-center gap-3">
              <span className="text-xs text-muted-foreground">{reqSummary}</span>
              <span className="text-xs text-muted-foreground">{reqNote}</span>
            </div>
          </CardAction>
        </CardHeader>
        <div className="flex flex-wrap items-center gap-2 border-y border-border/60 bg-secondary px-4 py-2.5">
          <Input
            type="search"
            className="h-7 min-w-[200px] flex-[0_1_280px] text-xs"
            placeholder="搜索 IP / UA / 模型 / 账号 / 请求 ID"
            aria-label="搜索请求记录"
            value={q}
            onChange={(e) => setQ(e.target.value)}
          />
          <Select
            items={selectOptionItems(OUTCOME_OPTIONS)}
            value={outcome || OUTCOME_ALL}
            onValueChange={(v) => setOutcome(v === OUTCOME_ALL ? '' : (v ?? ''))}>
            <SelectTrigger size="sm" className="w-28" aria-label="按结果筛选">
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              <SelectOptions options={OUTCOME_OPTIONS} />
            </SelectContent>
          </Select>
          <TimeRange value={range} onChange={setRange} />
          <Select
            items={selectOptionItems(LIMIT_OPTIONS)}
            value={limit}
            onValueChange={(v) => v && setLimit(v)}>
            <SelectTrigger size="sm" className="w-28" aria-label="读取条数">
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              <SelectOptions options={LIMIT_OPTIONS} />
            </SelectContent>
          </Select>
          <span className="flex-1" />
          <span className={cn('text-xs text-muted-foreground', (filtered.length !== entries.length || !hasSource) && 'text-warning')}>
            {!entries.length
              ? ''
              : (filtered.length !== entries.length
                  ? '命中 ' + filtered.length + ' / ' + entries.length + ' 条'
                  : entries.length + ' 条') + (hasSource ? '' : ' · 来源未记录')}
          </span>
          <Button size="xs" disabled={reqQ.isFetching} onClick={() => void reqQ.refetch()}>
            重新读取
          </Button>
        </div>
        <div className="max-h-[460px] overflow-auto">
          <Table className="min-w-[1080px]">
            <TableHeader className="sticky top-0 z-[1] bg-card">
              <TableRow>
                <TableHead>时间</TableHead>
                <TableHead>结果</TableHead>
                <TableHead>模型</TableHead>
                <TableHead>账号</TableHead>
                <TableHead>来源 IP</TableHead>
                <TableHead>User-Agent</TableHead>
                <TableHead className="text-right">耗时</TableHead>
                <TableHead className="text-right">Token</TableHead>
                <TableHead className="text-right">积分</TableHead>
                <TableHead>请求 ID</TableHead>
                <TableHead>决策</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {filtered.length === 0 ? (
                <TableRow>
                  <TableCell colSpan={11} className="py-10 text-center text-muted-foreground">{entries.length ? '没有符合当前筛选条件的请求记录' : '暂无请求记录'}</TableCell>
                </TableRow>
              ) : (
                filtered.map((e, i) => <RequestRow key={e.request_id || i} e={e} />)
              )}
            </TableBody>
          </Table>
        </div>
      </Card>

      <Card className="mt-5">
        <CardHeader className="flex flex-wrap items-center justify-between gap-3">
          <CardTitle>积分历史</CardTitle>
          <CardAction>
            <span className="text-xs text-muted-foreground">本地账本 · 每次余额变动记一条（最近 100 条）</span>
          </CardAction>
        </CardHeader>
        <div className="overflow-x-auto">
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>时间</TableHead>
                <TableHead>账号</TableHead>
                <TableHead className="text-right">变动</TableHead>
                <TableHead className="text-right">变动后余额</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {(creditHistQ.data?.entries || []).length === 0 ? (
                <TableRow>
                  <TableCell colSpan={4} className="py-8 text-center text-muted-foreground">
                    暂无余额变动记录（账本随余额查询累积，首次观测只建基线）
                  </TableCell>
                </TableRow>
              ) : (
                [...(creditHistQ.data?.entries || [])].reverse().map((entry, i) => (
                  // 账本时间只到秒：同一秒内多账号变动会撞车，必须再拼下标保唯一
                  // （`entry.time || '' + i` 的运算符优先级会把下标并进右操作数，等于没拼）。
                  <TableRow key={(entry.time || '') + '#' + i}>
                    <TableCell className="font-mono text-xs">{chTime(entry.time) || '—'}</TableCell>
                    <TableCell>{chName(entry)}</TableCell>
                    <TableCell className="text-right font-mono tabular-nums">
                      <Tag tone={chTone(entry.delta)}>{chDelta(entry.delta)}</Tag>
                    </TableCell>
                    <TableCell className="text-right font-mono tabular-nums">{entry.remain ?? '—'}</TableCell>
                  </TableRow>
                ))
              )}
            </TableBody>
          </Table>
        </div>
      </Card>

      <Card className="mt-5">
        <CardHeader className="flex flex-wrap items-center justify-between gap-3">
          <CardTitle>运行日志</CardTitle>
          <Tabs value={ch} onValueChange={(v) => setCh(v ?? 'all')}>
            <TabsList>
              <TabsTrigger value="all">全部</TabsTrigger>
              <TabsTrigger value="task">{CH_LABEL.task}</TabsTrigger>
              <TabsTrigger value="chat">{CH_LABEL.chat}</TabsTrigger>
              <TabsTrigger value="sys">{CH_LABEL.sys}</TabsTrigger>
            </TabsList>
          </Tabs>
          <CardAction>
            <div className="flex flex-wrap items-center gap-3">
              <span className="text-xs text-muted-foreground">{logNote}</span>
              <Button size="xs" onClick={() => setPin((p) => !p)}>
                自动滚动：{pin ? '开' : '关'}
              </Button>
            </div>
          </CardAction>
        </CardHeader>
        <pre
          ref={logBoxRef}
          className="m-0 max-h-[420px] overflow-auto whitespace-pre-wrap break-all px-4 py-3.5 font-mono text-xs leading-[1.75] text-muted-foreground"
        >
          {logEntries.length === 0 ? (
            <span className="text-muted-foreground/70">暂无日志</span>
          ) : (
            logEntries.map((e, i) => {
              const lvl = /error|失败|错误/.test(e.text) ? 'text-destructive' : /warn|冷却|熔断/.test(e.text) ? 'text-warning' : '';
              const t = e.ts ? fmtClockTime(e.ts) : '';
              return (
                <span key={i} className={cn('block', lvl)}>
                  {ch === 'all' ? (
                    <i
                      className={cn(
                        'mr-2 inline-block rounded px-1.5 py-0.5 align-[1px] text-3xs not-italic leading-none',
                        e.ch === 'task' && 'bg-accent-soft text-accent-foreground',
                        e.ch === 'chat' && 'bg-success-soft text-success',
                        e.ch === 'sys' && 'bg-secondary text-muted-foreground',
                      )}
                    >
                      {CH_LABEL[e.ch || ''] || e.ch}
                    </i>
                  ) : null}
                  {t + ' ' + e.text}
                </span>
              );
            })
          )}
        </pre>
      </Card>
    </>
  );
}

function RequestRow({ e }: { e: RequestEntry }) {
  const [open, setOpen] = useState(false);
  const when = fmtClockTime(e.time);
  const tone = reqOutcomeTone(e);
  const muted = <span className="text-muted-foreground">—</span>;
  return (
    <>
    <TableRow title={requestLogText(e)}>
      <TableCell>
        <span className="font-mono tabular-nums">{when}</span>
      </TableCell>
      <TableCell>
        <Tag tone={tone}>{String(e.status ?? '—') + ' ' + reqOutcomeLabel(e)}</Tag>
      </TableCell>
      <TableCell>{e.model || '—'}</TableCell>
      <TableCell>{e.account || '—'}</TableCell>
      <TableCell>{e.client_ip ? <Clip className="max-w-[150px] font-mono text-xs" text={e.client_ip} /> : muted}</TableCell>
      <TableCell>{e.user_agent ? <Clip className="max-w-[300px]" text={e.user_agent} /> : muted}</TableCell>
      <TableCell className="text-right">
        <span className="font-mono tabular-nums">{fmtMs(e.duration_ms)}</span>
      </TableCell>
      <TableCell className="text-right">
        <span className="font-mono tabular-nums">{reqTokenCell(e)}</span>
      </TableCell>
      <TableCell className="text-right">
        <span className="font-mono tabular-nums">{reqCreditCell(e)}</span>
      </TableCell>
      <TableCell>{e.request_id ? <Clip className="max-w-[150px] font-mono text-2xs text-muted-foreground" text={e.request_id} /> : muted}</TableCell>
      <TableCell>
        {e.decision ? (
          <Button size="xs" variant="ghost" onClick={() => setOpen((v) => !v)}>
            {open ? '收起' : '展开'}
          </Button>
        ) : muted}
      </TableCell>
    </TableRow>
    {open && e.decision ? (
      <TableRow>
        <TableCell colSpan={11} className="bg-muted/30">
          <pre className="m-0 max-h-64 overflow-auto whitespace-pre-wrap break-all font-mono text-2xs text-muted-foreground">
            {JSON.stringify(e.decision, null, 2)}
          </pre>
        </TableCell>
      </TableRow>
    ) : null}
    </>
  );
}

function Clip({ text, className }: { text: string; className?: string }) {
  return (
    <span className={cn('block truncate', className)} title={text}>
      {text}
    </span>
  );
}
