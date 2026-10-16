import { useQuery, useQueryClient } from '@tanstack/react-query';
import { api, postJSON } from '@/lib/api';
import { AUTO_TASKS } from '@/lib/autoTasks';
import { toast } from '@/components/Toaster';
import { Button } from '@/components/ui/button';
import { Table, TableWrap, Td, Th } from '@/components/ui/table';
import { Tag } from '@/components/ui/field';
import { Modal, ModalBody, ModalFooter, ModalHeader, ModalHint, ModalTitle } from '@/components/ui/dialog';

interface Task {
  task_code: string;
  title?: string;
  tag?: string;
  task_desc?: string;
  description?: string;
  jump_url?: string;
  current?: number;
  target?: number;
  credit?: number;
  energy?: number;
  reward_buddy?: boolean;
  claimed?: boolean;
  claimable?: boolean;
  locked?: boolean;
  accept_status?: string;
}

export function TasksDialog({ uid, onClose }: { uid: string | null; onClose: () => void }) {
  const qc = useQueryClient();
  const open = !!uid;

  const { data, isLoading, error, refetch } = useQuery({
    queryKey: ['tasks', uid],
    queryFn: () => api<{ tasks?: Task[] }>('accounts/' + encodeURIComponent(uid!) + '/tasks'),
    enabled: open,
  });

  const tasks = [...(data?.tasks ?? [])].sort(
    (a, b) =>
      Number(a.claimed) - Number(b.claimed) ||
      Number(b.claimable) - Number(a.claimable) ||
      String(a.task_code).localeCompare(String(b.task_code)),
  );

  const refresh = () => {
    void refetch();
    void qc.invalidateQueries({ queryKey: ['overview'] });
  };

  const acceptAll = async () => {
    if (!uid) return;
    try {
      const r = await postJSON<{ accepted?: number; failed?: unknown[]; message?: string }>(
        'accounts/' + encodeURIComponent(uid) + '/tasks/accept_all',
      );
      const n = r.accepted || 0;
      if (r.failed && r.failed.length) {
        toast(`已接受 ${n} 个，${r.failed.length} 个被上游拒绝（可重试）`, 'err');
      } else {
        toast(n ? `已接受 ${n} 个任务` : r.message || '所有任务均已接受', 'ok');
      }
    } catch (e) {
      toast((e as Error).message, 'err');
    }
    refresh();
  };

  const autoAll = async () => {
    if (!uid) return;
    if (
      !confirm(
        '将依次执行：补报对话事件、领取 Buddy、glm-5.2 对话、尝试上报。\n过程约 1-2 分钟（含真实对话），确认继续？',
      )
    )
      return;
    try {
      const r = await postJSON<{ results?: { status?: string }[] }>(
        'accounts/' + encodeURIComponent(uid) + '/tasks/auto_all',
      );
      const res = r.results || [];
      const okN = res.filter((x) => x.status === 'done').length;
      const skipN = res.filter((x) => x.status === 'skipped').length;
      const errN = res.filter((x) => x.status === 'error').length;
      toast(
        `执行完成：成功 ${okN} 项，跳过 ${skipN} 项${errN ? '，失败 ' + errN + ' 项' : ''}`,
        errN ? 'err' : 'ok',
      );
    } catch (e) {
      toast((e as Error).message, 'err');
    }
    refresh();
  };

  const act = async (kind: 'auto' | 'claim' | 'accept', code: string) => {
    if (!uid) return;
    try {
      if (kind === 'auto') {
        const r = await postJSON<{
          skipped?: boolean;
          message?: string;
          progress_before?: number;
          progress_after?: number;
          claimed?: boolean;
          claimable?: boolean;
          claim_error?: string;
          attempt?: boolean;
        }>('accounts/' + encodeURIComponent(uid) + '/tasks/auto', { task_code: code });
        if (r.skipped) {
          toast(r.message || '已跳过', 'ok');
        } else {
          const advanced = r.progress_before !== r.progress_after;
          let msg = r.message || '已执行';
          if (r.progress_after) msg += `（进度 ${r.progress_before} → ${r.progress_after}）`;
          if (r.claimed) msg += '，奖励已自动到账';
          else if (r.claimable) msg += r.claim_error ? '，可点「领取」重试' : '';
          else if (r.attempt && !advanced) msg += '；进度未动，该任务可能需要官方客户端';
          toast(msg, r.claimed || advanced ? 'ok' : 'err');
        }
      } else {
        const path =
          'accounts/' + encodeURIComponent(uid) + '/tasks/' + (kind === 'claim' ? 'claim' : 'accept');
        const body = kind === 'claim' ? { task_code: code } : { task_codes: [code] };
        await postJSON(path, body);
        toast(kind === 'claim' ? '已领取奖励' : '已接受任务', 'ok');
      }
    } catch (e) {
      toast((e as Error).message, 'err');
    }
    refresh();
  };

  return (
    <Modal open={open} onOpenChange={(v) => !v && onClose()} width={940}>
      <ModalHeader>
        <ModalTitle>积分任务 {uid ? <span className="font-mono text-xs text-ink-3">{uid.slice(0, 16)}</span> : null}</ModalTitle>
        <ModalHint>
          查询上游任务进度；「接受」为报名（幂等），「领取」在进度达标后可用。所有操作走网关，无需外部脚本。
        </ModalHint>
      </ModalHeader>
      <ModalBody>
        {isLoading ? (
          <div className="text-[13px] text-ink-2">加载中…</div>
        ) : error ? (
          <div className="text-[13px] text-bad">{(error as Error).message}</div>
        ) : tasks.length === 0 ? (
          <div className="text-[13px] text-ink-2">该账号暂无任务</div>
        ) : (
          <TableWrap>
            <Table className="min-w-[700px]">
              <thead>
                <tr>
                  <Th className="w-[3px] p-0" aria-hidden />
                  <Th>任务</Th>
                  <Th>进度</Th>
                  <Th>奖励</Th>
                  <Th>状态</Th>
                  <Th />
                </tr>
              </thead>
              <tbody>
                {tasks.map((t) => {
                  const cur = t.current ?? 0;
                  const tgt = t.target ?? 0;
                  const prog = tgt ? cur + ' / ' + tgt : tgt === 0 && cur > 0 ? String(cur) : '—';
                  const parts: string[] = [];
                  if (t.credit) parts.push('+' + t.credit + ' 分');
                  if (t.energy) parts.push('+' + t.energy + ' 能');
                  if (t.reward_buddy) parts.push('Buddy');
                  const reward = parts.length ? parts.join(' ') : '—';
                  const badge = t.claimed ? (
                    <Tag tone="ok">已领取</Tag>
                  ) : t.claimable ? (
                    <Tag tone="warn">可领取</Tag>
                  ) : t.locked ? (
                    <Tag tone="mute">未解锁</Tag>
                  ) : t.accept_status === 'accepted' ? (
                    <Tag tone="mute">进行中</Tag>
                  ) : (
                    <Tag tone="mute">未接受</Tag>
                  );
                  const tip = [t.title, t.task_desc || t.description, t.jump_url ? '跳转：' + t.jump_url : '']
                    .filter(Boolean)
                    .join('\n');
                  return (
                    <tr key={t.task_code} title={tip}>
                      <Td className="w-[3px] p-0">
                        <i className="block h-full min-h-[36px] w-[3px] bg-ok" />
                      </Td>
                      <Td>
                        <div className="font-medium">{t.title || t.task_code}</div>
                        <div className="font-mono text-[11.5px] text-ink-3">
                          {t.task_code}
                          {t.tag ? ' · ' + t.tag : ''}
                        </div>
                      </Td>
                      <Td className="font-mono tabular-nums">{prog}</Td>
                      <Td className="font-mono tabular-nums">{reward}</Td>
                      <Td>{badge}</Td>
                      <Td className="text-right">
                        {t.claimed || t.locked ? null : t.claimable ? (
                          <Button size="xs" variant="primary" onClick={() => void act('claim', t.task_code)}>
                            领取
                          </Button>
                        ) : AUTO_TASKS[t.task_code] ? (
                          <Button
                            size="xs"
                            variant="primary"
                            title={AUTO_TASKS[t.task_code]}
                            onClick={() => void act('auto', t.task_code)}
                          >
                            一键完成
                          </Button>
                        ) : t.accept_status === 'accepted' ? null : (
                          <Button size="xs" onClick={() => void act('accept', t.task_code)}>
                            接受
                          </Button>
                        )}
                      </Td>
                    </tr>
                  );
                })}
              </tbody>
            </Table>
          </TableWrap>
        )}
      </ModalBody>
      <ModalFooter>
        <span className="flex-1" />
        <Button onClick={() => void acceptAll()}>全部接受</Button>
        <Button variant="primary" onClick={() => void autoAll()}>
          一键完成可自动任务
        </Button>
        <Button onClick={() => void refetch()}>重新查询</Button>
        <Button onClick={onClose}>关闭</Button>
      </ModalFooter>
    </Modal>
  );
}
