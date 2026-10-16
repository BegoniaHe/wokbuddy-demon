import { useEffect, useMemo, useRef, useState } from 'react';
import { useQuery } from '@tanstack/react-query';
import { api } from '@/lib/api';
import { fmtMs, fmtTok } from '@/lib/format';
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
import { Input, Select, Tag } from '@/components/ui/field';
import { Box, BoxHeader, BoxHint, BoxNote, BoxTitle, Grow, Toolbar } from '@/components/ui/box';
import { Empty, Num, Table, TableWrap, Td, Th } from '@/components/ui/table';
import { Tabs } from '@/components/ui/tabs';
import { TimeRange } from '@/components/TimeRange';
import { cn } from '@/lib/utils';

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
      <Box>
        <BoxHeader>
          <BoxTitle>
            请求记录 <BoxHint>调用来源：IP 取 X-Forwarded-For 首段，无代理头时回落 TCP 对端</BoxHint>
          </BoxTitle>
          <Grow />
          <BoxNote className="flex-[1_1_320px] text-left">{reqSummary}</BoxNote>
          <BoxNote className="flex-[0_1_260px] text-right">{reqNote}</BoxNote>
        </BoxHeader>
        <Toolbar>
          <Input
            type="search"
            size="sm"
            className="min-w-[200px] flex-[0_1_280px]"
            placeholder="搜索 IP / UA / 模型 / 账号 / 请求 ID"
            aria-label="搜索请求记录"
            value={q}
            onChange={(e) => setQ(e.target.value)}
          />
          <Select
            size="sm"
            aria-label="按结果筛选"
            value={outcome}
            onChange={(e) => setOutcome(e.target.value)}
          >
            <option value="">全部结果</option>
            <option value="success">成功</option>
            <option value="http_error">HTTP 错误</option>
            <option value="stream_error">流错误</option>
            <option value="interrupted">中断</option>
          </Select>
          <TimeRange value={range} onChange={setRange} />
          <Select
            size="sm"
            aria-label="读取条数"
            value={limit}
            onChange={(e) => setLimit(e.target.value)}
          >
            <option value="100">最近 100 条</option>
            <option value="300">最近 300 条</option>
            <option value="1000">最近 1000 条</option>
          </Select>
          <Grow />
          <BoxNote className={cn((filtered.length !== entries.length || !hasSource) && 'text-warn')}>
            {!entries.length
              ? ''
              : (filtered.length !== entries.length
                  ? '命中 ' + filtered.length + ' / ' + entries.length + ' 条'
                  : entries.length + ' 条') + (hasSource ? '' : ' · 来源未记录')}
          </BoxNote>
          <Button size="xs" disabled={reqQ.isFetching} onClick={() => void reqQ.refetch()}>
            重新读取
          </Button>
        </Toolbar>
        <TableWrap className="max-h-[460px] overflow-auto">
          <Table className="min-w-[1080px]">
            <thead className="sticky top-0 z-[1] bg-surface">
              <tr>
                <Th>时间</Th>
                <Th>结果</Th>
                <Th>模型</Th>
                <Th>账号</Th>
                <Th>来源 IP</Th>
                <Th>User-Agent</Th>
                <Th className="text-right">耗时</Th>
                <Th className="text-right">Token</Th>
                <Th className="text-right">积分</Th>
                <Th>请求 ID</Th>
              </tr>
            </thead>
            <tbody>
              {filtered.length === 0 ? (
                <tr>
                  <Td colSpan={10}>
                    <Empty>{entries.length ? '没有符合当前筛选条件的请求记录' : '暂无请求记录'}</Empty>
                  </Td>
                </tr>
              ) : (
                filtered.map((e, i) => <RequestRow key={e.request_id || i} e={e} />)
              )}
            </tbody>
          </Table>
        </TableWrap>
      </Box>

      <Box>
        <BoxHeader>
          <BoxTitle>运行日志</BoxTitle>
          <Grow />
          <Tabs
            active={ch}
            onSelect={setCh}
            items={[
              { value: 'all', label: '全部' },
              { value: 'task', label: CH_LABEL.task },
              { value: 'chat', label: CH_LABEL.chat },
              { value: 'sys', label: CH_LABEL.sys },
            ]}
          />
          <BoxNote>{logNote}</BoxNote>
          <Button size="xs" onClick={() => setPin((p) => !p)}>
            自动滚动：{pin ? '开' : '关'}
          </Button>
        </BoxHeader>
        <pre
          ref={logBoxRef}
          className="m-0 max-h-[420px] overflow-auto whitespace-pre-wrap break-all px-4 py-3.5 font-mono text-xs leading-[1.75] text-ink-2"
        >
          {logEntries.length === 0 ? (
            <span className="text-ink-3">暂无日志</span>
          ) : (
            logEntries.map((e, i) => {
              const lvl = /error|失败|错误/.test(e.text) ? 'text-bad' : /warn|冷却|熔断/.test(e.text) ? 'text-warn' : '';
              const t = e.ts ? new Date(e.ts).toLocaleTimeString('zh-CN', { hour12: false }) : '';
              return (
                <span key={i} className={cn('block', lvl)}>
                  {ch === 'all' ? (
                    <i
                      className={cn(
                        'mr-2 inline-block rounded px-1.5 py-0.5 align-[1px] text-3xs not-italic leading-none',
                        e.ch === 'task' && 'bg-accent-soft text-accent',
                        e.ch === 'chat' && 'bg-ok-soft text-ok',
                        e.ch === 'sys' && 'bg-surface-2 text-ink-3',
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
      </Box>
    </>
  );
}

function RequestRow({ e }: { e: RequestEntry }) {
  const when = e.time ? new Date(e.time).toLocaleTimeString('zh-CN', { hour12: false }) : '—';
  const tone = reqOutcomeTone(e);
  const muted = <span className="text-ink-3">—</span>;
  return (
    <tr title={requestLogText(e)}>
      <Td>
        <Num>{when}</Num>
      </Td>
      <Td>
        <Tag tone={tone}>{String(e.status ?? '—') + ' ' + reqOutcomeLabel(e)}</Tag>
      </Td>
      <Td>{e.model || '—'}</Td>
      <Td>{e.account || '—'}</Td>
      <Td>{e.client_ip ? <Clip className="max-w-[150px] font-mono text-xs" text={e.client_ip} /> : muted}</Td>
      <Td>{e.user_agent ? <Clip className="max-w-[300px]" text={e.user_agent} /> : muted}</Td>
      <Td className="text-right">
        <Num>{fmtMs(e.duration_ms)}</Num>
      </Td>
      <Td className="text-right">
        <Num>{reqTokenCell(e)}</Num>
      </Td>
      <Td className="text-right">
        <Num>{reqCreditCell(e)}</Num>
      </Td>
      <Td>{e.request_id ? <Clip className="max-w-[150px] font-mono text-2xs text-ink-3" text={e.request_id} /> : muted}</Td>
    </tr>
  );
}

function Clip({ text, className }: { text: string; className?: string }) {
  return (
    <span className={cn('block overflow-hidden text-ellipsis whitespace-nowrap', className)} title={text}>
      {text}
    </span>
  );
}
