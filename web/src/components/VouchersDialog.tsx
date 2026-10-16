import { useState } from 'react';
import { useQuery } from '@tanstack/react-query';
import { api } from '@/lib/api';
import { qrMatrix, qrRects, QR_VIEW_SIZE } from '@/lib/qr';
import { toast } from '@/components/Toaster';
import { Button } from '@/components/ui/button';
import { Tag } from '@/components/ui/field';
import { Modal, ModalBody, ModalFooter, ModalHeader, ModalHint, ModalTitle } from '@/components/ui/dialog';
import { cn } from '@/lib/utils';

interface Voucher {
  prize_name?: string;
  sku_code?: string;
  code?: string;
  valid_to?: string;
  granted_at?: string;
}
interface VoucherAccount {
  uid: string;
  nickname?: string;
  error?: string;
  vouchers?: Voucher[];
}

// copyText：clipboard API 只在 secure context 可用，远程 http 面板降级 execCommand。
async function copyText(text: string): Promise<void> {
  if (navigator.clipboard && window.isSecureContext) return navigator.clipboard.writeText(text);
  return new Promise((resolve, reject) => {
    const ta = document.createElement('textarea');
    ta.value = text;
    ta.style.cssText = 'position:fixed;opacity:0';
    document.body.appendChild(ta);
    ta.select();
    try {
      document.execCommand('copy') ? resolve() : reject(new Error('copy failed'));
    } catch (e) {
      reject(e as Error);
    } finally {
      ta.remove();
    }
  });
}

function QR({ text, px = 148 }: { text: string; px?: number }) {
  try {
    const M = qrMatrix(text);
    const n = M.length;
    const rects = qrRects(M);
    return (
      <svg viewBox={`0 0 ${QR_VIEW_SIZE(n)} ${QR_VIEW_SIZE(n)}`} width={px} height={px} shapeRendering="crispEdges" role="img" style={{ background: '#fff' }}>
        {rects.map((r, i) => (
          <rect key={i} x={r.x} y={r.y} width={1} height={1} />
        ))}
      </svg>
    );
  } catch (e) {
    return <span className="text-[11px] text-ink-3">二维码生成失败：{(e as Error).message}</span>;
  }
}

export function VouchersDialog({ open, onOpenChange }: { open: boolean; onOpenChange: (v: boolean) => void }) {
  const [qrCode, setQrCode] = useState<string | null>(null);
  const { data, isLoading, error, refetch } = useQuery({
    queryKey: ['school/vouchers'],
    queryFn: () => api<{ accounts?: VoucherAccount[] }>('school/vouchers'),
    enabled: open,
  });

  const arr = data?.accounts ?? [];
  const ok = arr.filter((a) => !a.error);
  const total = ok.reduce((n, a) => n + (a.vouchers || []).length, 0);
  const errs = arr.filter((a) => a.error);
  const shown = ok.filter((a) => (a.vouchers || []).length);

  const onCopy = async (code: string) => {
    try {
      await copyText(code);
      toast('券码已复制', 'ok');
    } catch {
      toast('复制失败，请手动选择券码', 'err');
    }
  };

  return (
    <Modal open={open} onOpenChange={onOpenChange}>
      <ModalHeader>
        <ModalTitle>开学季 · 我的券码</ModalTitle>
        <ModalHint>抽奖抽中的第三方券（KFC / 瑞幸 / 酷狗等），券码到店到对应app/小程序兑换。</ModalHint>
      </ModalHeader>
      <ModalBody>
        {isLoading ? (
          <div className="text-[13px] text-ink-2">查询中…</div>
        ) : error ? (
          <div className="text-[13px] text-bad">{(error as Error).message}</div>
        ) : shown.length === 0 ? (
          <div className="py-6 text-center text-ink-3">
            <div className="mb-1 text-sm">🎟️</div>还没有抽到券
          </div>
        ) : (
          shown.map((a) => (
            <div key={a.uid}>
              <div className="mx-0.5 mb-2 mt-4 flex items-baseline gap-1.5 text-xs text-ink-3 first:mt-1">
                <span className="text-[13px] font-semibold text-ink-2">{a.nickname || a.uid}</span>
                <span>{(a.vouchers || []).length} 张</span>
              </div>
              {(a.vouchers || []).map((v, i) => {
                const expired = v.valid_to && new Date(v.valid_to) < new Date();
                return (
                  <div
                    key={i}
                    className={cn('mb-2.5 overflow-hidden rounded-xl border bg-surface', expired ? 'border-dashed border-line' : 'border-line hover:border-accent')}
                  >
                    <div className={cn('flex items-center gap-2 px-3.5 pt-3', expired && 'opacity-55')}>
                      <span className="text-sm font-semibold">{v.prize_name || v.sku_code || '券'}</span>
                      <span className="ml-auto">
                        {expired ? <Tag tone="bad">已过期</Tag> : <Tag tone="ok">可使用</Tag>}
                      </span>
                    </div>
                    <div className={cn('px-3.5 pb-2.5 text-[11.5px] text-ink-3', expired && 'opacity-55')}>
                      {v.valid_to ? '有效期至 ' + v.valid_to : '长期有效'}
                      {v.granted_at ? ' · ' + v.granted_at.slice(0, 10) + ' 抽中' : ''}
                    </div>
                    <div className="mx-3 border-t border-dashed border-line" />
                    <div className="flex items-center gap-3 px-3.5 py-2.5">
                      <span className="flex-none text-[11.5px] text-ink-3">券码</span>
                      <code className="rounded-md border border-line-soft bg-surface-2 px-3 py-1.5 font-mono text-[15px] font-semibold tracking-widest">{v.code || '-'}</code>
                      <span className="ml-auto flex flex-none gap-2">
                        {v.code ? (
                          <Button size="xs" variant="ghost" onClick={() => setQrCode((c) => (c === v.code ? null : v.code!))}>
                            二维码
                          </Button>
                        ) : null}
                        <Button size="xs" variant="ghost" onClick={() => void onCopy(v.code || '')}>
                          复制
                        </Button>
                      </span>
                    </div>
                    {qrCode === v.code ? (
                      <div className="flex justify-center border-t border-dashed border-line p-3.5">
                        <QR text={v.code || ''} />
                      </div>
                    ) : null}
                  </div>
                );
              })}
            </div>
          ))
        )}
        {errs.length ? (
          <div className="mt-2 text-[11px] text-warn">
            查询失败：{errs.map((a) => (a.nickname || a.uid.slice(0, 8)) + '（' + a.error + '）').join('、')}
          </div>
        ) : null}
      </ModalBody>
      <ModalFooter>
        <span className="text-[11px] text-ink-3">{total ? total + ' 张券 · ' + ok.filter((a) => !(a.vouchers || []).length).length + ' 个账号未抽中' : ''}</span>
        <span className="flex-1" />
        <Button size="xs" onClick={() => void refetch()}>
          刷新
        </Button>
        <Button size="xs" variant="primary" onClick={() => onOpenChange(false)}>
          关闭
        </Button>
      </ModalFooter>
    </Modal>
  );
}
