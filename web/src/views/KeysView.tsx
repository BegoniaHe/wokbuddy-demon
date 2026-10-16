import { useState } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { api, postJSON } from '@/lib/api';
import { ago, fmtTok } from '@/lib/format';
import { toast } from '@/components/Toaster';
import type { ApiKey } from '@/lib/types';
import { Button } from '@/components/ui/button';
import { Field, Input } from '@/components/ui/field';
import { Box, BoxBody, BoxHeader, BoxNote, BoxTitle, Grow } from '@/components/ui/box';
import { Empty, Num, Table, TableWrap, Td, Th } from '@/components/ui/table';
import { Modal, ModalBody, ModalFooter, ModalHeader, ModalHint, ModalTitle } from '@/components/ui/dialog';

// KeysView：命名 API 密钥管理。命名密钥只用于网关 /v1/*（chat/models）；
// 面板与 /status、/metrics 仍只认 config.json 的默认 api_key。
export function KeysView() {
  const qc = useQueryClient();
  const [createOpen, setCreateOpen] = useState(false);
  const [name, setName] = useState('');
  const [rawKey, setRawKey] = useState('');

  const keysQ = useQuery({
    queryKey: ['keys'],
    queryFn: () => api<{ keys?: ApiKey[] }>('keys'),
  });
  const list = keysQ.data?.keys ?? [];

  const invalidate = () => qc.invalidateQueries({ queryKey: ['keys'] });

  const createM = useMutation({
    mutationFn: (n: string) => postJSON<{ key: string; record: ApiKey }>('keys', { name: n }),
    onSuccess: (r) => {
      setRawKey(r.key);
      void invalidate();
    },
    onError: (e) => toast('创建失败：' + (e as Error).message, 'err'),
  });

  const revokeM = useMutation({
    mutationFn: (id: string) => postJSON('keys/' + id + '/revoke'),
    onSuccess: () => {
      toast('已撤销，该密钥立即失效', 'ok');
      void invalidate();
    },
    onError: (e) => toast('撤销失败：' + (e as Error).message, 'err'),
  });

  const deleteM = useMutation({
    mutationFn: (id: string) => postJSON('keys/' + id + '/delete'),
    onSuccess: () => {
      toast('已删除', 'ok');
      void invalidate();
    },
    onError: (e) => toast('删除失败：' + (e as Error).message, 'err'),
  });

  const openCreate = () => {
    setName('');
    setRawKey('');
    setCreateOpen(true);
  };

  const copyKey = (k: string) =>
    void navigator.clipboard.writeText(k).then(
      () => toast('密钥已复制', 'ok'),
      () => toast('复制失败，请手动选择复制', 'err'),
    );

  return (
    <>
      <Box>
        <BoxHeader>
          <BoxTitle>命名密钥</BoxTitle>
          <Grow />
          <BoxNote>仅用于 /v1/*（chat / models）；面板与 /status、/metrics 仍只认默认 api_key</BoxNote>
          <Button variant="primary" size="xs" onClick={openCreate}>
            创建密钥
          </Button>
        </BoxHeader>
        <BoxBody className="p-0">
          {keysQ.isLoading ? (
            <Empty>加载中…</Empty>
          ) : keysQ.error ? (
            <Empty>读取失败：{(keysQ.error as Error).message}</Empty>
          ) : list.length === 0 ? (
            <Empty>暂无命名密钥。创建后即可分发给不同客户端，并在此查看各自的请求与 token 用量。</Empty>
          ) : (
            <TableWrap>
              <Table>
                <thead>
                  <tr>
                    <Th>名称</Th>
                    <Th>前缀</Th>
                    <Th className="text-right">请求数</Th>
                    <Th className="text-right">Token</Th>
                    <Th>创建</Th>
                    <Th>最近使用</Th>
                    <Th>状态</Th>
                    <Th className="text-right">操作</Th>
                  </tr>
                </thead>
                <tbody>
                  {list.map((k) => (
                    <tr key={k.id}>
                      <Td>{k.name}</Td>
                      <Td className="font-mono text-ink-3">{k.prefix}…</Td>
                      <Td className="text-right">
                        <Num>{k.requests ?? 0}</Num>
                      </Td>
                      <Td className="text-right">
                        <Num>{fmtTok(k.total_tokens ?? 0)}</Num>
                      </Td>
                      <Td className="text-ink-3">{ago(k.created_at)}</Td>
                      <Td className="text-ink-3">{k.last_used_at ? ago(k.last_used_at) : '—'}</Td>
                      <Td>
                        {k.revoked ? (
                          <span className="text-bad">已撤销</span>
                        ) : (
                          <span className="text-ok">有效</span>
                        )}
                      </Td>
                      <Td className="text-right">
                        {k.revoked ? null : (
                          <Button
                            size="xs"
                            disabled={revokeM.isPending}
                            onClick={() => revokeM.mutate(k.id)}
                          >
                            撤销
                          </Button>
                        )}
                        <Button
                          size="xs"
                          variant="danger"
                          className="ml-1.5"
                          disabled={deleteM.isPending}
                          onClick={() => {
                            if (!confirm('删除密钥「' + k.name + '」将同时清除其用量记录，且不可恢复。确认删除？')) return;
                            deleteM.mutate(k.id);
                          }}
                        >
                          删除
                        </Button>
                      </Td>
                    </tr>
                  ))}
                </tbody>
              </Table>
            </TableWrap>
          )}
        </BoxBody>
      </Box>

      <Modal open={createOpen} onOpenChange={setCreateOpen} width={480}>
        <ModalHeader>
          <ModalTitle>创建命名密钥</ModalTitle>
          <ModalHint>密钥明文仅创建时显示一次，请立即复制保存。</ModalHint>
        </ModalHeader>
        <ModalBody>
          {rawKey ? (
            <>
              <Field label="密钥（仅此一次显示）">
                <Input readOnly value={rawKey} className="font-mono" onFocus={(e) => e.target.select()} />
              </Field>
              <div className="text-2xs text-warn">关闭后无法再次查看。如丢失，请撤销后重新创建。</div>
            </>
          ) : (
            <Field label="名称" hint="用于区分客户端，例如「笔记本」「手机」。">
              <Input
                autoFocus
                placeholder="客户端名称"
                value={name}
                onChange={(e) => setName(e.target.value)}
                onKeyDown={(e) => {
                  if (e.key === 'Enter') createM.mutate(name);
                }}
              />
            </Field>
          )}
        </ModalBody>
        <ModalFooter>
          <span className="flex-1" />
          {rawKey ? (
            <>
              <Button onClick={() => copyKey(rawKey)}>复制密钥</Button>
              <Button variant="primary" onClick={() => setCreateOpen(false)}>
                完成
              </Button>
            </>
          ) : (
            <>
              <Button onClick={() => setCreateOpen(false)}>取消</Button>
              <Button variant="primary" disabled={createM.isPending} onClick={() => createM.mutate(name)}>
                {createM.isPending ? '创建中…' : '创建'}
              </Button>
            </>
          )}
        </ModalFooter>
      </Modal>
    </>
  );
}
