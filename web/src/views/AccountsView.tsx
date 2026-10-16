import { useQuery, useQueryClient } from '@tanstack/react-query';
import { api, postJSON } from '@/lib/api';
import type { Account, ModelLock, Overview } from '@/lib/types';
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
import { Badge, Chip, Tag } from '@/components/ui/field';
import { Bar } from '@/components/ui/bar';
import { Box, BoxBody, BoxHeader, BoxHint, BoxNote, BoxTitle, Dot, Grow, type DotTone } from '@/components/ui/box';
import { Empty, Num, RailCell, Table, TableWrap, Td, Th, type RailTone } from '@/components/ui/table';
import { Stat, Stats } from '@/components/ui/stat';
import { TasksDialog } from '@/components/TasksDialog';
import { useState } from 'react';

// 账号池视图：统计条 + 积分到期提醒 + 账号表 + 模型锁池。
export function AccountsView() {
  const qc = useQueryClient();
  const [taskUID, setTaskUID] = useState<string | null>(null);

  const { data, error } = useQuery({
    queryKey: ['overview'],
    queryFn: () => api<Overview>('overview'),
    refetchInterval: 5000,
  });

  const pkgQuery = useQuery({
    queryKey: ['packages'],
    queryFn: () => api<{ accounts?: PackageAccount[] }>('packages'),
    enabled: false,
    staleTime: 2 * 60 * 1000,
  });

  if (error && !data) {
    return <Empty>读取失败：{(error as Error).message}</Empty>;
  }
  if (!data) {
    return <Empty>加载中…</Empty>;
  }

  const accounts = data.accounts ?? [];
  const remSum = accounts.reduce((a, s) => a + (s.credits || 0), 0);
  const totSum = accounts.reduce((a, s) => a + (s.credits_total || 0), 0);
  const creditsText = totSum > 0 ? remSum + ' / ' + totSum : String(remSum);

  const afterAction = () => {
    void qc.invalidateQueries({ queryKey: ['overview'] });
  };

  const run = async (fn: () => Promise<void>) => {
    try {
      await fn();
    } catch (e) {
      toast((e as Error).message, 'err');
    }
    afterAction();
  };

  return (
    <>
      <Stats>
        <Stat value={data.total} label="账号总数" />
        <Stat value={data.healthy} label="可用" tone="good" />
        <Stat value={data.cooling} label="冷却中" tone="warn" />
        <Stat value={data.disabled} label="已禁用" tone="bad" />
        <Stat value={data.paused ?? '-'} label="已暂停选号" tone="warn" />
        <Stat value={creditsText} label="积分剩余 / 总额" />
        <Stat value={data.sticky_sessions} label="粘性会话" />
      </Stats>

      <ExpiryBox
        overview={data}
        detail={pkgQuery.data}
        fetchedAt={pkgQuery.dataUpdatedAt}
        checking={pkgQuery.isFetching}
        onCheck={() => void pkgQuery.refetch()}
      />

      <Box>
        <BoxHeader>
          <BoxTitle>账号池</BoxTitle>
          <Grow />
          <BoxNote>{data.in_flight_full ? data.in_flight_full + ' 个账号在途占满' : ''}</BoxNote>
          <Button size="xs" onClick={() => void run(async () => { await postJSON('checkin_all'); toast('全部签到已开始，结果见日志', 'ok'); })}>全部签到</Button>
          <Button size="xs" onClick={() => void run(async () => { await postJSON('travel_all'); toast('旅行巡检已开始（含领养链路），结果见日志', 'ok'); })}>旅行巡检</Button>
          <Button size="xs" onClick={() => void run(async () => { await postJSON('activity_all'); toast('活跃上报已开始，结果见日志', 'ok'); })}>活跃上报</Button>
          <Button size="xs" onClick={() => void run(async () => { await postJSON('keepalive_all'); toast('全部保活已开始，结果见日志', 'ok'); })}>全部保活</Button>
        </BoxHeader>
        <TableWrap>
          <Table>
            <thead>
              <tr>
                <Th className="w-[3px] p-0" aria-hidden />
                <Th>账号</Th>
                <Th>状态</Th>
                <Th>积分</Th>
                <Th>成功 / 失败</Th>
                <Th>在途</Th>
                <Th>用量</Th>
                <Th>最近成功</Th>
                <Th />
              </tr>
            </thead>
            <tbody>
              {accounts.length === 0 ? (
                <tr>
                  <Td colSpan={9}>
                    <Empty>
                      <div className="mb-1.5 text-sm text-ink-2">账号池是空的</div>
                      点击右上角「添加账号」，用浏览器登录一个 WorkBuddy 账号
                    </Empty>
                  </Td>
                </tr>
              ) : (
                accounts.map((s) => (
                  <AccountRow
                    key={s.uid}
                    s={s}
                    maxCred={Math.max(1, ...accounts.map((x) => x.credits || 0))}
                    onOpenTasks={() => setTaskUID(s.uid)}
                    onDone={afterAction}
                  />
                ))
              )}
            </tbody>
          </Table>
        </TableWrap>
      </Box>

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
          <div key={nm} className="flex items-start gap-2.5 border-t border-line-soft py-2 text-xs first:border-t-0">
            <Dot tone="mute" className="mt-1 h-2.5 w-2.5" />
            <span className="min-w-[9em] flex-none font-semibold">{nm}</span>
            <span className="text-bad">查询失败：{a.error}</span>
          </div>
        );
      }
      const bs = expBatches(a.packages).filter((b) => expDaysLeft(b.date, today) >= 0);
      if (!bs.length) {
        return (
          <div key={nm} className="flex items-start gap-2.5 border-t border-line-soft py-2 text-xs first:border-t-0">
            <Dot tone="ok" className="mt-1 h-2.5 w-2.5" />
            <span className="min-w-[9em] flex-none font-semibold">{nm}</span>
            <span className="text-ink-2">7 天内无到期积分</span>
          </div>
        );
      }
      const first = bs[0]!;
      const days = expDaysLeft(first.date, today);
      const daily = Math.ceil(first.remain / Math.max(1, days));
      const week = bs.filter((b) => expDaysLeft(b.date, today) <= 7).reduce((s, b) => s + b.remain, 0);
      const dotTone: DotTone = days <= 3 ? 'bad' : days <= 7 ? 'warn' : 'ok';
      const dayWord = days === 0 ? '今天到期' : days === 1 ? '明天到期' : days + ' 天后到期';
      const more = bs.length > 4 ? '　等 ' + bs.length + ' 批' : '';
      const rest = bs.slice(1, 4).map((b) => '随后 ' + b.date.slice(5) + ' · ' + fmtTok(b.remain)).join('　') + more;
      return (
        <div key={nm} className="flex items-start gap-2.5 border-t border-line-soft py-2 text-xs first:border-t-0">
          <Dot tone={dotTone} className="mt-1 h-2.5 w-2.5" />
          <span className="min-w-[9em] flex-none font-semibold">{nm}</span>
          <span className="text-ink-2">
            最近到期 <b className="font-mono">{first.date}</b>（{dayWord}）· 该批{' '}
            <b className="font-mono">{fmtTok(first.remain)}</b> 积分 · 到期前日均需耗 ≥
            <b className="font-mono">{fmtTok(daily)}</b>
            {week > first.remain ? ' · 7 天内合计 ' + fmtTok(week) : ''}
            {rest ? <div className="mt-0.5 text-2xs text-ink-3">{rest}</div> : null}
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
      return x.key - y.key;
    });
    rows = keyed.map(({ a, ms }) => {
      const nm = a.nickname || String(a.uid || '').slice(0, 8);
      if (!ms || !a.credits_earliest_remaining) {
        return (
          <div key={nm} className="flex items-start gap-2.5 border-t border-line-soft py-2 text-xs first:border-t-0">
            <Dot tone="ok" className="mt-1 h-2.5 w-2.5" />
            <span className="min-w-[9em] flex-none font-semibold">{nm}</span>
            <span className="text-ink-2">无近期到期批次</span>
          </div>
        );
      }
      const days = Math.max(0, Math.ceil((ms - today.getTime()) / 86400000));
      const remain = Number(a.credits_earliest_remaining || 0);
      const daily = Math.ceil(remain / Math.max(1, days));
      const dotTone: DotTone = days <= 3 ? 'bad' : days <= 7 ? 'warn' : 'ok';
      const dayWord = days === 0 ? '今天到期' : days === 1 ? '明天到期' : days + ' 天后到期';
      const when = new Date(ms).toISOString().slice(0, 10);
      return (
        <div key={nm} className="flex items-start gap-2.5 border-t border-line-soft py-2 text-xs first:border-t-0">
          <Dot tone={dotTone} className="mt-1 h-2.5 w-2.5" />
          <span className="min-w-[9em] flex-none font-semibold">{nm}</span>
          <span className="text-ink-2">
            最近到期 <b className="font-mono">{when}</b>（{dayWord}）· 该批{' '}
            <b className="font-mono">{fmtTok(remain)}</b> 积分 · 到期前日均需耗 ≥
            <b className="font-mono">{fmtTok(daily)}</b>
            <div className="mt-0.5 text-2xs text-ink-3">仅最早一批，点「检查」看全部到期批次</div>
          </span>
        </div>
      );
    });
    note = list.length + ' 个账号 · 快照（来自本地状态，点「检查」拉实时明细）';
  }

  return (
    <Box>
      <BoxHeader>
        <BoxTitle>
          积分到期提醒
          <BoxHint>按最近到期批次估算 · 日均需耗 = 批次剩余 ÷ 距到期天数 · 上游按失效时刻先后自动优先扣减（FEFO）</BoxHint>
        </BoxTitle>
        <Grow />
        <BoxNote>{note}</BoxNote>
        <Button size="xs" disabled={checking} onClick={onCheck}>
          {checking ? '查询中…' : '检查'}
        </Button>
      </BoxHeader>
      <BoxBody>{rows && (rows as unknown[])?.length ? rows : <Empty>没有账号</Empty>}</BoxBody>
    </Box>
  );
}

function AccountRow({
  s,
  maxCred,
  onOpenTasks,
  onDone,
}: {
  s: Account;
  maxCred: number;
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
  const railTone: RailTone = s.disabled || s.paused ? 'bad' : cool > 0 ? 'warn' : 'ok';

  return (
    <tr className={cls} title={'uid: ' + s.uid}>
      <RailCell tone={railTone} />
      <Td>
        <div className="font-medium">
          {s.nickname ? s.nickname : <span className="text-ink-3">未命名</span>}
          {s.realm === 'global' ? <Badge className="ml-1.5">国际版</Badge> : null}
          {s.enterprise ? <Badge className="ml-1.5">企业版</Badge> : null}
        </div>
        <div className="mt-px truncate font-mono text-2xs text-ink-3">{short}</div>
      </Td>
      <Td>
        {tag}
        {s.reason ? <div className="mt-0.5 text-2xs text-ink-3">{s.reason}</div> : null}
        {limits.length ? (
          <div className="mt-1.5 flex min-w-[210px] flex-col gap-1">
            {limits.map((row, i) => {
              const m = rateLimitMeta(row as RateLimitRow, now);
              return (
                <div
                  key={i}
                  title={m.title}
                  className={
                    'flex items-baseline gap-1.5 rounded-control border px-1.5 py-1 text-2xs leading-tight ' +
                    (m.kind === 'model_unavailable'
                      ? 'border-line bg-surface-2 text-ink-3'
                      : 'border-warn/25 bg-warn-soft')
                  }
                >
                  <b className="font-mono font-semibold text-ink">{m.model}</b>
                  <span className="text-ink-2">{m.detail}</span>
                </div>
              );
            })}
          </div>
        ) : null}
      </Td>
      <Td title={credTip}>
        <div className="font-mono text-sm tabular-nums">{cred}</div>
        <Bar pct={pct} className="mt-1 max-w-[88px]" />
      </Td>
      <Td>
        <Num>
          {s.success_count || 0} <span className="text-ink-3">/</span>{' '}
          <span className="text-bad">{s.err_total || 0}</span>
        </Num>
      </Td>
      <Td>
        <Num>{s.in_flight || 0}</Num>
      </Td>
      <Td title={usageTitle}>
        <span className="inline-flex flex-wrap items-center gap-1">
          <Chip tone="accent" unit="次">{req}</Chip>
          <Chip tone="accent" unit={totalTok !== '—' ? 'tok' : undefined}>{totalTok}</Chip>
          <Chip tone="warn">{latency}</Chip>
          <Chip tone="ok">{rate}</Chip>
        </span>
      </Td>
      <Td>
        <Num className="text-ink-3">{ago(s.last_success)}</Num>
      </Td>
      <Td>
        <div className="flex justify-end gap-1.5 whitespace-nowrap">
          {!s.enterprise ? (
            <Button size="xs" variant="ghost" title={s.checkin_done ? '今日已签到；点击可重新签到并刷新余额' : undefined} onClick={() => void act('checkin')}>
              {s.checkin_done ? '已签' : '签到'}
            </Button>
          ) : null}
          <Button size="xs" variant="ghost" title={s.enterprise ? '刷新企业版已分配额度（上游 get-enterprise-user-usage）' : undefined} onClick={() => void act('balance')}>
            {s.enterprise ? '额度' : '余额'}
          </Button>
          {!s.enterprise ? (
            <Button size="xs" variant="ghost" onClick={onOpenTasks}>
              任务
            </Button>
          ) : null}
          {frozen ? (
            <Button size="xs" variant="primary" onClick={() => void act('revive')}>
              解冻
            </Button>
          ) : s.paused ? (
            <Button size="xs" variant="primary" onClick={() => void act('resume')}>
              恢复选号
            </Button>
          ) : (
            <Button size="xs" variant="ghost" title={s.enterprise ? '退出选号，但照常保活 / 刷新额度' : '退出选号，但照常签到 / 活跃上报 / 保活 / 刷新余额'} onClick={() => void act('pause')}>
              暂停选号
            </Button>
          )}
          {s.disabled ? null : (
            <Button size="xs" variant="ghost" onClick={() => void act('disable')}>
              禁用
            </Button>
          )}
          <Button size="xs" variant="danger" onClick={() => void act('remove')}>
            移除
          </Button>
        </div>
      </Td>
    </tr>
  );
}

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
    <Box>
      <BoxHeader>
        <BoxTitle>
          模型锁池 <BoxHint>哪些模型不能用、还要锁多久</BoxHint>
        </BoxTitle>
        <Grow />
        <BoxNote>{list.length ? (bad ? bad + ' 个模型整池不可用' : list.length + ' 个模型部分限流') : ''}</BoxNote>
      </BoxHeader>
      <TableWrap>
        <Table>
          <thead>
            <tr>
              <Th>模型</Th>
              <Th>域</Th>
              <Th>状态</Th>
              <Th>可选 / 总数</Th>
              <Th>锁定账号</Th>
              <Th>最早解锁</Th>
              <Th>全池解锁</Th>
              <Th>原因</Th>
            </tr>
          </thead>
          <tbody>
            {list.length === 0 ? (
              <tr>
                <Td colSpan={8}>
                  <Empty>当前没有模型级限流 —— 所有模型均可选</Empty>
                </Td>
              </tr>
            ) : (
              list.map((r, i) => {
                const st = STATE[r.state || ''] || ['mute', r.state || '—'];
                const realm = r.realm === 'global' ? '国际版' : '国内版';
                return (
                  <tr key={i}>
                    <Td>{r.model}</Td>
                    <Td>
                      <Badge>{realm}</Badge>
                    </Td>
                    <Td>
                      <Tag tone={st[0] as 'bad' | 'warn' | 'mute'}>{st[1]}</Tag>
                    </Td>
                    <Td>
                      <Num>
                        {r.servable || 0} / {r.total || 0}
                      </Num>
                    </Td>
                    <Td>
                      <Num>{r.locked || 0}</Num>
                    </Td>
                    <Td>
                      <Num>{left(r.unlock_at || r.fully_unlock_at)}</Num>
                    </Td>
                    <Td>
                      <Num>{left(r.fully_unlock_at)}</Num>
                    </Td>
                    <Td>{r.reason ? <span className="text-ink-2">{r.reason}</span> : '—'}</Td>
                  </tr>
                );
              })
            )}
          </tbody>
        </Table>
      </TableWrap>
    </Box>
  );
}
