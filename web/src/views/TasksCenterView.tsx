import { useEffect, useRef, useState } from 'react';
import { api, postJSON } from '@/lib/api';
import { toast } from '@/components/Toaster';
import { Button } from '@/components/ui/button';
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from '@/components/ui/select';
import {
  Card,
  CardAction,
  CardHeader,
  CardTitle,
} from '@/components/ui/card';
import { Bar, Dot, Tag, type Tone } from '@/components/ui-kit';
import { VouchersDialog } from '@/components/VouchersDialog';

interface QRow {
  kind: string;
  code: string;
  prog?: string;
  status: string;
  message?: string;
}
interface Group {
  uid: string;
  nick?: string;
  rows: QRow[];
}
interface Queue {
  started?: boolean;
  running?: boolean;
  seq?: number;
  items?: { uid: string; nickname?: string; kind: string; code: string; status: string; message?: string }[];
}

const ST_WORDS: Record<string, string> = {
  done: '完成',
  running: '执行中',
  error: '失败',
  skipped: '跳过',
  pending: '排队',
  scan: '待执行',
};

export function TasksCenterView() {
  const [groups, setGroups] = useState<Group[] | null>(null);
  const [progress, setProgress] = useState<Queue | null>(null);
  const [conc, setConc] = useState('1');
  const [scanning, setScanning] = useState(false);
  const [starting, setStarting] = useState(false);
  const [vouchersOpen, setVouchersOpen] = useState(false);
  const titlesRef = useRef<Record<string, string>>({});
  const seqRef = useRef(0);
  const timerRef = useRef<number | null>(null);

  const stopPolling = () => {
    if (timerRef.current) {
      clearInterval(timerRef.current);
      timerRef.current = null;
    }
  };

  useEffect(() => stopPolling, []);

  const scan = async () => {
    stopPolling();
    setScanning(true);
    try {
      const d = await postJSON<{ accounts?: { uid: string; nickname?: string; growth?: { task_code: string; title?: string; current?: number; target?: number }[] }[] }>(
        'tasks/scan_all',
      );
      const gs: Group[] = [];
      for (const a of d.accounts || []) {
        const rows: QRow[] = [];
        for (const t of a.growth || []) {
          titlesRef.current[t.task_code] = t.title || t.task_code;
          rows.push({ kind: 'growth', code: t.task_code, prog: t.target ? (t.current ?? 0) + '/' + t.target : '—', status: 'scan' });
        }
        if (rows.length) gs.push({ uid: a.uid, nick: a.nickname, rows });
      }
      setGroups(gs);
      setProgress(null);
    } catch (e) {
      toast((e as Error).message, 'err');
    } finally {
      setScanning(false);
    }
  };

  const startPolling = () => {
    if (timerRef.current) clearInterval(timerRef.current);
    timerRef.current = window.setInterval(async () => {
      let q: Queue;
      try {
        q = await api<Queue>('tasks/queue');
      } catch {
        return;
      }
      if (!q.started) return;
      if (seqRef.current && q.seq !== seqRef.current) return;
      setGroups(groupsFromQueue(q.items || []));
      setProgress(q);
      if (!q.running) {
        stopPolling();
        toast('任务队列执行结束', 'ok');
      }
    }, 3000);
  };

  const runQueue = async () => {
    const c = Number(conc) || 1;
    if (!confirm('扫描全部账号待办并排队执行（账号并发 ' + c + '，账号内串行）。\n含真实对话的任务耗时较长，确认继续？')) return;
    setStarting(true);
    try {
      const r = await postJSON<{ started?: boolean; message?: string; seq?: number; total?: number }>('tasks/run_queue', { concurrency: c });
      if (!r.started) {
        toast(r.message || '没有待办任务', 'ok');
        return;
      }
      seqRef.current = r.seq || 0;
      toast('队列已启动：' + r.total + ' 项（并发 ' + c + '）', 'ok');
      startPolling();
    } catch (e) {
      toast((e as Error).message, 'err');
    } finally {
      setStarting(false);
    }
  };

  const groupsFromQueue = (items: NonNullable<Queue['items']>): Group[] => {
    const by = new Map<string, Group>();
    for (const it of items) {
      if (!by.has(it.uid)) by.set(it.uid, { uid: it.uid, nick: it.nickname, rows: [] });
      by.get(it.uid)!.rows.push({
        kind: it.kind,
        code: it.code,
        prog: it.kind === 'school' ? '—' : '',
        status: it.status,
        message: it.message,
      });
    }
    return [...by.values()];
  };

  const total = groups ? groups.reduce((n, g) => n + g.rows.length, 0) : 0;
  const done = progress?.items
    ? progress.items.filter((it) => it.status === 'done' || it.status === 'error' || it.status === 'skipped').length
    : 0;
  const pct = progress?.items && progress.items.length ? Math.round((done / progress.items.length) * 100) : 0;

  return (
    <>
      <Card>
        <CardHeader className="flex-row flex-wrap items-center gap-3">
          <CardTitle className="flex items-baseline">
            成长任务队列
            <span className="ml-1.5 text-2xs font-normal text-muted-foreground">{total ? total + ' 项' : ''}</span>
          </CardTitle>
          <CardAction>
            <div className="flex flex-wrap items-center gap-2">
              <Button size="xs" title="开学季抽奖抽中的第三方券码（活动已结束，历史券码仍可查）" onClick={() => setVouchersOpen(true)}>
                查询券码
              </Button>
              <Select value={conc} onValueChange={(v) => setConc(v ?? '1')}>
                <SelectTrigger size="sm" className="w-16">
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  <SelectItem value="1">并发 1</SelectItem>
                  <SelectItem value="2">并发 2</SelectItem>
                  <SelectItem value="3">并发 3</SelectItem>
                </SelectContent>
              </Select>
              <Button size="xs" disabled={scanning} onClick={() => void scan()}>
                {scanning ? '扫描中…' : '扫描待办'}
              </Button>
              <Button size="xs" disabled={starting} onClick={() => void runQueue()}>
                {starting ? '启动中…' : '执行全部待办'}
              </Button>
            </div>
          </CardAction>
        </CardHeader>

        {progress?.items ? (
          <div className="flex items-center gap-3 px-4 pt-3">
            <Bar pct={pct} className="h-1 flex-1" />
            <span className="whitespace-nowrap text-xs tabular-nums text-muted-foreground">
              {(progress.running ? '执行中 ' : '已结束 ') + done + ' / ' + progress.items.length}
            </span>
          </div>
        ) : null}

        {!groups || groups.length === 0 ? (
          <div className="flex flex-col items-center gap-1.5 px-4 py-10 text-muted-foreground">
            <svg viewBox="0 0 48 48" fill="none" stroke="currentColor" strokeWidth={2} className="mb-1.5 size-10 opacity-55" aria-hidden>
              <rect x="8" y="6" width="24" height="36" rx="3" />
              <path d="M14 16h12M14 24h12M14 32h7" opacity=".45" />
              <circle cx="35" cy="33" r="8" />
              <path d="m40 38 5 5" />
            </svg>
            <div className="font-medium text-foreground">
              {groups ? '没有待办任务 🎉' : '还没有扫描过'}
            </div>
            <div className="max-w-[34em] text-center text-xs">
              {groups
                ? '全部账号的成长任务与开学季活动都已完成，明日再来。'
                : '扫描所有账号的成长任务与开学季待办，把没做的排成一列，一键执行。'}
            </div>
          </div>
        ) : (
          <div className="pb-2.5">
            {groups.map((g) => (
              <div key={g.uid} className="border-t border-border/60 py-1 first:border-t-0">
                <header className="flex items-baseline gap-2.5 px-4 pb-1.5 pt-2.5">
                  <span className="text-sm font-semibold">{g.nick || g.uid.slice(0, 12)}</span>
                  <span className="text-xs tabular-nums text-muted-foreground">{g.rows.length} 项待办</span>
                </header>
                {g.rows.map((it, i) => (
                  <QueueRow key={i} it={it} title={it.kind === 'school' ? '开学季闭环' : titlesRef.current[it.code] || it.code} />
                ))}
              </div>
            ))}
          </div>
        )}
      </Card>

      <VouchersDialog open={vouchersOpen} onOpenChange={setVouchersOpen} />
    </>
  );
}

function QueueRow({ it, title }: { it: QRow; title: string }) {
  const isSchool = it.kind === 'school';
  const known =
    it.status === 'running' || it.status === 'error' || it.status === 'skipped' || it.status === 'done';
  const dotTone: Tone =
    it.status === 'running' ? 'accent' : it.status === 'error' ? 'bad' : it.status === 'done' ? 'ok' : 'mute';
  const dotExtra = it.status === 'running' ? 'animate-pulse' : known ? '' : 'border-[1.5px] border-muted-foreground bg-transparent';
  const stWord = it.status === 'scan' ? '待执行' : ST_WORDS[it.status] || it.status;
  return (
    <div
      className="grid grid-cols-[118px_1fr_52px_84px_minmax(120px,240px)] items-center gap-2.5 px-4 py-[7.5px] text-sm hover:bg-secondary"
      title={it.message || ''}
    >
      <span className="truncate font-mono text-2xs text-muted-foreground">{it.code}</span>
      <span className="flex min-w-0 items-center gap-1.5">
        <span className="truncate">{title}</span>
        {isSchool ? <Tag tone="mute">开学季</Tag> : null}
      </span>
      <span className="text-right font-mono tabular-nums text-muted-foreground">{it.prog || ''}</span>
      <span className="inline-flex items-center gap-1.5 whitespace-nowrap">
        <Dot tone={dotTone} className={dotExtra} />
        {stWord}
      </span>
      <span className="truncate text-xs text-muted-foreground">{it.message || ''}</span>
    </div>
  );
}
