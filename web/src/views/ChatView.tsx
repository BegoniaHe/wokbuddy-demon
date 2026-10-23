import { useEffect, useMemo, useRef, useState } from 'react';
import { useQuery } from '@tanstack/react-query';
import {
  Check,
  ChevronDown,
  Copy,
  Eraser,
  MessagesSquare,
  Plus,
  RefreshCw,
  Send,
  Sparkles,
  Square,
  Trash2,
} from 'lucide-react';
import { api, getKey, notifyUnauthorized } from '@/lib/api';
import { toast } from '@/components/Toaster';
import { useConfirm } from '@/components/ConfirmDialog';
import { Markdown } from '@/components/Markdown';
import { Button } from '@/components/ui/button';
import { Input } from '@/components/ui/input';
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogHeader,
  DialogTitle,
} from '@/components/ui/dialog';
import {
  Select,
  SelectContent,
  SelectOptions,
  SelectTrigger,
  SelectValue,
  selectOptionItems,
} from '@/components/ui/select';
import { Tag } from '@/components/ui-kit';
import { cn } from '@/lib/utils';
import { ago, fmtMs, fmtTok } from '@/lib/format';
import { mdSearchText, type ModelEntry } from '@/lib/models';
import {
  CHAT_MAX_SESSIONS,
  CHAT_RAW_MAX_FRAMES,
  LS_CHAT_MODEL,
  applyChatChunk,
  chatError,
  chatModelParts,
  chatTitle,
  chatWireMessages,
  loadChatSessions,
  readSSEStream,
  newChatID,
  newChatSession,
  saveChatSessions,
  type ChatMessage,
  type ChatSession,
} from '@/lib/chat';

const EXAMPLES = ['用一句话介绍这个网关', '写一个快速排序，并解释复杂度', '把这段 JSON 格式化并指出问题'];

// ChatView 试聊：直接打本机 /v1/chat/completions，验证网关、账号池与模型路由。
//
// 边界：会话只落浏览器 localStorage；原始 SSE 帧只留内存；面板与服务端不新增
// 任何对话存储端点，提示词不进入 reqlog 归档（归档原本只写元数据）。
export function ChatView() {
  const [sessions, setSessions] = useState<ChatSession[]>(() => {
    const saved = loadChatSessions();
    return saved.length ? saved : [newChatSession()];
  });
  const [activeId, setActiveId] = useState(() => sessions[0]?.id ?? '');
  const [input, setInput] = useState('');
  const [busy, setBusy] = useState(false);
  const [pickerOpen, setPickerOpen] = useState(false);
  const [stick, setStick] = useState(true);

  // 全局只跑一条流，但必须记住它属于哪个会话：清空/删除/停止只能作用于该会话。
  const streamRef = useRef<{ sessionId: string; ctrl: AbortController } | null>(null);
  const scrollRef = useRef<HTMLDivElement>(null);
  const inputRef = useRef<HTMLTextAreaElement>(null);
  const { confirm, confirmDialog } = useConfirm();

  const modelsQ = useQuery({
    queryKey: ['models'],
    queryFn: () => api<{ models?: ModelEntry[] }>('models'),
  });
  const models = modelsQ.data?.models ?? [];

  const active = sessions.find((s) => s.id === activeId) ?? sessions[0];
  const activeMessages = active?.messages;

  const patch = (id: string, fn: (s: ChatSession) => void) => {
    setSessions((prev) =>
      prev.map((s) => {
        if (s.id !== id) return s;
        const next: ChatSession = { ...s, messages: s.messages };
        fn(next);
        return next;
      }),
    );
  };

  useEffect(() => {
    if (!sessions.some((s) => s.id === activeId)) setActiveId(sessions[0]?.id ?? '');
  }, [sessions, activeId]);

  useEffect(() => {
    const t = setTimeout(() => saveChatSessions(sessions), 400);
    return () => clearTimeout(t);
  }, [sessions]);

  useEffect(() => () => streamRef.current?.ctrl.abort(), []);

  // 模型目录到达后补默认值：记住上次选择 → 官方默认 → 首个。
  useEffect(() => {
    if (!active || active.model || !models.length) return;
    let remembered: string | null = null;
    try {
      remembered = localStorage.getItem(LS_CHAT_MODEL);
    } catch {
      // 隐私模式/禁用存储：读偏好失败不应中断试聊。
    }
    const pick = models.find((m) => m.id === remembered) ?? models.find((m) => m.is_default) ?? models[0];
    if (!pick) return;
    setSessions((prev) => prev.map((s) => (s.id === active.id && !s.model ? { ...s, model: pick.id } : s)));
  }, [active, models]);

  useEffect(() => {
    const el = inputRef.current;
    if (!el) return;
    el.style.height = 'auto';
    el.style.height = Math.min(el.scrollHeight, 180) + 'px';
  }, [input]);

  useEffect(() => {
    if (!stick) return;
    const el = scrollRef.current;
    if (el) el.scrollTop = el.scrollHeight;
  }, [activeMessages, stick]);

  const onScroll = () => {
    const el = scrollRef.current;
    if (!el) return;
    setStick(el.scrollHeight - el.scrollTop - el.clientHeight < 80);
  };

  const addSession = () => {
    const s = newChatSession();
    if (active?.model) s.model = active.model;
    setSessions((prev) => [s, ...prev].slice(0, CHAT_MAX_SESSIONS));
    setActiveId(s.id);
    setStick(true);
  };

  const removeSession = async (id: string) => {
    const target = sessions.find((s) => s.id === id);
    if (!target) return;
    if (
      !(await confirm({
        title: '删除对话「' + target.title + '」？',
        description: '对话只存在本机浏览器，删除后不可恢复。',
        confirmText: '删除',
        destructive: true,
      }))
    )
      return;
    if (streamRef.current?.sessionId === id) streamRef.current.ctrl.abort();
    setSessions((prev) => {
      const next = prev.filter((s) => s.id !== id);
      return next.length ? next : [newChatSession()];
    });
  };

  const clearMessages = async () => {
    if (!active || !active.messages.length) return;
    if (
      !(await confirm({
        title: '清空当前对话？',
        description: '消息只存在本机浏览器，清空后不可恢复。',
        confirmText: '清空',
        destructive: true,
      }))
    )
      return;
    if (streamRef.current?.sessionId === active.id) streamRef.current.ctrl.abort();
    patch(active.id, (s) => {
      s.messages = [];
      s.title = '新对话';
    });
    setStick(true);
  };

  const pickModel = (id: string) => {
    if (!active) return;
    patch(active.id, (s) => {
      s.model = id;
    });
    try {
      localStorage.setItem(LS_CHAT_MODEL, id);
    } catch {
      // 偏好写不进不影响本次选择。
    }
    setPickerOpen(false);
  };

  const run = async (sessionId: string, base: ChatMessage[], model: string) => {
    const asst: ChatMessage = { id: newChatID(), role: 'assistant', content: '', at: Date.now() };
    patch(sessionId, (s) => {
      s.messages = [...base, asst];
      s.at = Date.now();
    });
    setStick(true);

    const ctrl = new AbortController();
    streamRef.current = { sessionId, ctrl };
    setBusy(true);
    const started = performance.now();
    let firstAt = 0;
    let raf = 0;
    let pending: ChatMessage | null = null;

    const snapshot = (): ChatMessage => ({
      ...asst,
      toolCalls: asst.toolCalls?.map((t) => ({ ...t })),
      raw: asst.raw ? [...asst.raw] : undefined,
    });
    const flush = (m: ChatMessage) => {
      setSessions((prev) =>
        prev.map((s) =>
          s.id !== sessionId
            ? s
            : { ...s, messages: s.messages.map((x) => (x.id === asst.id ? m : x)) },
        ),
      );
    };
    const schedule = () => {
      pending = snapshot();
      if (raf) return;
      raf = requestAnimationFrame(() => {
        raf = 0;
        if (pending) {
          flush(pending);
          pending = null;
        }
      });
    };

    try {
      const key = getKey();
      const resp = await fetch('/v1/chat/completions', {
        method: 'POST',
        headers: {
          'Content-Type': 'application/json',
          ...(key ? { Authorization: 'Bearer ' + key } : {}),
        },
        body: JSON.stringify({ model, messages: chatWireMessages(base), stream: true }),
        signal: ctrl.signal,
      });
      if (resp.status === 401) notifyUnauthorized();
      if (!resp.ok) {
        const text = await resp.text();
        let message = 'HTTP ' + resp.status;
        try {
          const parsed: unknown = JSON.parse(text);
          message = chatError(parsed) || message;
        } catch {
          // 非 JSON 错误体：保留状态码。
        }
        throw new Error(message);
      }

      const consume = (data: string) => {
        if (!data || data === '[DONE]') return;
        let payload: unknown;
        try {
          payload = JSON.parse(data);
        } catch {
          return;
        }
        if (payload && typeof payload === 'object' && !('choices' in payload)) {
          const message = chatError(payload);
          if (message) throw new Error(message);
        }
        if (!firstAt) firstAt = performance.now();
        applyChatChunk(asst, payload);
        if (!asst.raw) asst.raw = [];
        if (asst.raw.length < CHAT_RAW_MAX_FRAMES) {
          asst.raw.push(data.length > 4000 ? data.slice(0, 4000) + '…' : data);
        }
        schedule();
      };

      await readSSEStream(resp, consume);

      if (!asst.content && !asst.toolCalls?.length && !asst.error) {
        asst.error = '上游没有返回内容（可能被拦截或模型未产出）。';
      }
    } catch (e) {
      const err = e as Error;
      if (err.name === 'AbortError') asst.aborted = true;
      else asst.error = err.message || '请求失败';
    } finally {
      if (raf) cancelAnimationFrame(raf);
      asst.elapsedMs = Math.round(performance.now() - started);
      if (firstAt) asst.ttftMs = Math.round(firstAt - started);
      flush(snapshot());
      // 只清理自己：被 abort 后用户可能已切到别的会话并开了新流。
      if (streamRef.current?.ctrl === ctrl) {
        streamRef.current = null;
        setBusy(false);
      }
    }
  };

  const send = () => {
    const text = input.trim();
    if (!text || busy || !active) return;
    if (!active.model) {
      toast('请先选择模型', 'err');
      setPickerOpen(true);
      return;
    }
    const user: ChatMessage = { id: newChatID(), role: 'user', content: text, at: Date.now() };
    const next = [...active.messages, user];
    const title = active.messages.length ? active.title : chatTitle(text);
    setInput('');
    patch(active.id, (s) => {
      s.messages = next;
      s.title = title;
      s.at = Date.now();
    });
    void run(active.id, next, active.model);
  };

  const regenerate = (msg: ChatMessage) => {
    if (!active || busy || !active.model) return;
    const at = active.messages.findIndex((m) => m.id === msg.id);
    if (at < 0) return;
    for (let i = at - 1; i >= 0; i--) {
      const m = active.messages[i];
      if (m?.role === 'user') {
        void run(active.id, active.messages.slice(0, i + 1), active.model);
        return;
      }
    }
  };

  const stop = () => streamRef.current?.ctrl.abort();
  // 流属于别的会话时，当前会话不应显示「生成中」或停止按钮。
  const streaming = busy && streamRef.current?.sessionId === active?.id;
  const liveId = streaming ? active?.messages[active.messages.length - 1]?.id : undefined;
  const sessionOptions = useMemo(
    () => sessions.map((s) => ({ value: s.id, label: s.title })),
    [sessions],
  );

  return (
    <>
      <div className="flex h-[calc(100dvh-9rem)] min-h-[480px] gap-4">
        <aside className="hidden w-[216px] flex-none flex-col overflow-hidden rounded-xl border border-border bg-card shadow-panel md:flex">
          <div className="flex items-center gap-2 border-b border-border px-3 py-2">
            <span className="text-xs font-medium">对话</span>
            <span className="flex-1" />
            <Button
              variant="ghost"
              size="icon-xs"
              title="新建对话"
              aria-label="新建对话"
              onClick={addSession}>
              <Plus />
            </Button>
          </div>
          <div className="min-h-0 flex-1 overflow-y-auto p-1.5">
            {sessions.map((s) => (
              <div
                key={s.id}
                className={cn(
                  'group flex items-center gap-1 rounded-lg px-1.5 py-1.5 transition-colors',
                  s.id === activeId ? 'bg-sidebar-active' : 'hover:bg-secondary/70',
                )}>
                <button
                  type="button"
                  className="min-w-0 flex-1 text-left"
                  onClick={() => {
                    setActiveId(s.id);
                    setStick(true);
                  }}>
                  <div className="truncate text-xs font-medium">{s.title}</div>
                  <div className="truncate text-3xs text-muted-foreground">
                    {s.model ? chatModelParts(s.model).name : '未选模型'} · {ago(new Date(s.at).toISOString())}
                  </div>
                </button>
                <Button
                  variant="ghost"
                  size="icon-xs"
                  className="opacity-0 transition-opacity group-hover:opacity-100"
                  title="删除对话"
                  aria-label="删除对话"
                  onClick={() => void removeSession(s.id)}>
                  <Trash2 />
                </Button>
              </div>
            ))}
          </div>
        </aside>

        <section className="flex min-w-0 flex-1 flex-col overflow-hidden rounded-xl border border-border bg-card shadow-panel">
          <header className="flex items-center gap-2 border-b border-border px-3 py-2 md:px-4">
            <div className="md:hidden">
              <Select
                items={selectOptionItems(sessionOptions)}
                value={activeId}
                onValueChange={(v) => {
                  if (typeof v === 'string' && v) setActiveId(v);
                }}>
                <SelectTrigger size="sm" className="max-w-[118px] min-w-0" aria-label="切换对话">
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  <SelectOptions options={sessionOptions} />
                </SelectContent>
              </Select>
            </div>
            <Button
              variant="outline"
              size="sm"
              className="min-w-0 max-w-[126px] sm:max-w-[260px]"
              onClick={() => setPickerOpen(true)}>
              <span className="truncate font-mono text-xs">
                {active?.model ? chatModelParts(active.model).name : '选择模型'}
              </span>
              <ChevronDown className="text-muted-foreground" />
            </Button>
            {active?.model ? (
              <Tag className="hidden sm:inline-flex" tone={chatModelParts(active.model).realm === 'global' ? 'accent' : 'mute'}>
                {chatModelParts(active.model).realm === 'global' ? 'Global' : 'CN'}
              </Tag>
            ) : null}
            <span className="flex-1" />
            <Button
              variant="ghost"
              size="icon-sm"
              title="清空当前对话"
              aria-label="清空当前对话"
              disabled={!active?.messages.length}
              onClick={() => void clearMessages()}>
              <Eraser />
            </Button>
            <Button
              variant="ghost"
              size="icon-sm"
              title="新建对话"
              aria-label="新建对话"
              className="md:hidden"
              onClick={addSession}>
              <Plus />
            </Button>
            <Button
              variant="ghost"
              size="icon-sm"
              title="删除当前对话"
              aria-label="删除当前对话"
              className="md:hidden"
              onClick={() => active && void removeSession(active.id)}>
              <Trash2 />
            </Button>
          </header>

          <div ref={scrollRef} onScroll={onScroll} className="min-h-0 flex-1 overflow-y-auto">
            {active && active.messages.length ? (
              <div className="mx-auto flex w-full max-w-3xl flex-col gap-5 px-4 py-5 md:px-6">
                {active.messages.map((m, i) => (
                  <MessageRow
                    key={m.id}
                    message={m}
                    live={busy && m.id === liveId}
                    canRegenerate={!busy && i === active.messages.length - 1 && m.role === 'assistant'}
                    onRegenerate={() => regenerate(m)}
                  />
                ))}
              </div>
            ) : (
              <div className="flex h-full items-center justify-center p-6">
                <div className="max-w-md text-center">
                  <span className="mx-auto mb-3 grid size-10 place-items-center rounded-xl bg-brand-soft text-brand">
                    <MessagesSquare className="size-5" />
                  </span>
                  <div className="text-sm font-medium">从网关发起一次真实对话</div>
                  <p className="mt-1 text-xs leading-relaxed text-muted-foreground">
                    请求走本机 <code className="font-mono">/v1/chat/completions</code>，按账号池调度。
                    内容只保存在这台浏览器，不写服务端对话记录。
                  </p>
                  <div className="mt-4 flex flex-wrap justify-center gap-1.5">
                    {EXAMPLES.map((t) => (
                      <button
                        key={t}
                        type="button"
                        className="rounded-full border border-border px-2.5 py-1 text-2xs text-muted-foreground transition-colors hover:bg-secondary hover:text-foreground"
                        onClick={() => {
                          setInput(t);
                          inputRef.current?.focus();
                        }}>
                        {t}
                      </button>
                    ))}
                  </div>
                </div>
              </div>
            )}
          </div>

          <div className="border-t border-border px-3 py-3 md:px-4">
            <div className="mx-auto w-full max-w-3xl">
              <div className="rounded-xl border border-input bg-background p-2 transition-[border-color,box-shadow] focus-within:border-ring focus-within:ring-2 focus-within:ring-ring/20">
                <textarea
                  ref={inputRef}
                  rows={1}
                  value={input}
                  aria-label="输入消息"
                  placeholder={
                    busy && !streaming
                      ? '另一个对话正在生成，请稍候'
                      : active?.model
                        ? '输入消息，Enter 发送'
                        : '先选择模型'
                  }
                  className="max-h-[180px] w-full resize-none bg-transparent px-1.5 py-1 text-base outline-none placeholder:text-muted-foreground"
                  onChange={(e) => setInput(e.target.value)}
                  onKeyDown={(e) => {
                    if (e.key === 'Enter' && !e.shiftKey && !e.nativeEvent.isComposing) {
                      e.preventDefault();
                      send();
                    }
                  }}
                />
                <div className="flex items-center gap-2 px-1 pt-1">
                  <span className="min-w-0 truncate text-3xs text-muted-foreground">
                    Enter 发送 · Shift+Enter 换行 · 仅存本机浏览器
                  </span>
                  <span className="flex-1" />
                  {streaming ? (
                    <Button size="sm" variant="outline" onClick={stop}>
                      <Square />
                      停止
                    </Button>
                  ) : (
                    <Button size="sm" disabled={busy || !input.trim() || !active?.model} onClick={send}>
                      <Send />
                      发送
                    </Button>
                  )}
                </div>
              </div>
            </div>
          </div>
        </section>
      </div>

      <ModelPicker
        open={pickerOpen}
        onOpenChange={setPickerOpen}
        models={models}
        loading={modelsQ.isFetching}
        error={modelsQ.error ? (modelsQ.error as Error).message : ''}
        value={active?.model ?? ''}
        onRetry={() => void modelsQ.refetch()}
        onPick={pickModel}
      />
      {confirmDialog}
    </>
  );
}

function MessageRow({
  message,
  live,
  canRegenerate,
  onRegenerate,
}: {
  message: ChatMessage;
  live: boolean;
  canRegenerate: boolean;
  onRegenerate: () => void;
}) {
  const copy = async () => {
    try {
      await navigator.clipboard.writeText(message.content);
      toast('已复制', 'ok');
    } catch {
      toast('复制失败：浏览器未授权剪贴板', 'err');
    }
  };

  if (message.role === 'user') {
    return (
      <div className="flex justify-end">
        <div className="max-w-[85%] rounded-xl rounded-br-sm bg-secondary px-3.5 py-2 text-base whitespace-pre-wrap break-words">
          {message.content}
        </div>
      </div>
    );
  }

  const done = !live;
  return (
    <div className="min-w-0">
      <div className="mb-1 flex flex-wrap items-center gap-2 text-3xs text-muted-foreground">
        <span className="grid size-5 place-items-center rounded-md bg-brand-soft text-brand">
          <Sparkles className="size-3" />
        </span>
        <span>助手</span>
        {message.error ? <Tag tone="bad">失败</Tag> : null}
        {message.aborted ? <Tag tone="warn">已停止</Tag> : null}
        {live ? <span>生成中…</span> : null}
        <span className="flex-1" />
        {message.usage?.total_tokens !== undefined ? (
          <span className="font-mono">{fmtTok(message.usage.total_tokens)} tok</span>
        ) : null}
        {message.elapsedMs !== undefined ? <span className="font-mono">{fmtMs(message.elapsedMs)}</span> : null}
      </div>

      {message.reasoning ? (
        <details className="mb-1.5 rounded-lg border border-border/70 bg-secondary/40">
          <summary className="cursor-pointer px-3 py-1.5 text-2xs text-muted-foreground select-none">
            思考过程 · {message.reasoning.length} 字
          </summary>
          <div className="max-h-60 overflow-y-auto border-t border-border/60 px-3 py-2 text-xs whitespace-pre-wrap text-muted-foreground">
            {message.reasoning}
          </div>
        </details>
      ) : null}

      {message.toolCalls?.length ? (
        <div className="mb-1.5 flex flex-col gap-1">
          {message.toolCalls.map((t, i) => (
            <details key={t.id || i} className="rounded-lg border border-border/70 bg-secondary/40">
              <summary className="cursor-pointer px-3 py-1.5 text-2xs text-muted-foreground select-none">
                工具调用 · {t.name || '未命名'}
              </summary>
              <pre className="max-h-60 overflow-auto border-t border-border/60 px-3 py-2 font-mono text-xs leading-relaxed">
                {t.arguments || '（无参数）'}
              </pre>
            </details>
          ))}
        </div>
      ) : null}

      {message.content ? <Markdown text={message.content} /> : null}
      {live && message.content ? (
        <span className="ml-0.5 inline-block h-4 w-[2px] animate-pulse bg-primary align-[-2px]" />
      ) : null}

      {message.error ? (
        <div className="mt-1.5 rounded-lg bg-destructive-soft px-3 py-2 text-sm text-destructive">
          {message.error}
        </div>
      ) : null}

      {done && (message.content || message.toolCalls?.length) ? (
        <div className="mt-1.5 flex flex-wrap items-center gap-1 text-2xs text-muted-foreground">
          <Button variant="ghost" size="xs" onClick={() => void copy()}>
            <Copy />
            复制
          </Button>
          {canRegenerate ? (
            <Button variant="ghost" size="xs" onClick={onRegenerate}>
              <RefreshCw />
              重发
            </Button>
          ) : null}
          {message.ttftMs !== undefined ? (
            <span className="ml-1 font-mono">首字 {fmtMs(message.ttftMs)}</span>
          ) : null}
        </div>
      ) : null}

      {message.raw?.length ? (
        <details className="mt-1.5 rounded-lg border border-border/70 bg-secondary/40">
          <summary className="cursor-pointer px-3 py-1.5 text-2xs text-muted-foreground select-none">
            原始流 · {message.raw.length} 帧（仅本次会话内存）
          </summary>
          <pre className="max-h-72 overflow-auto border-t border-border/60 px-3 py-2 font-mono text-3xs leading-relaxed whitespace-pre-wrap text-muted-foreground">
            {message.raw.join('\n')}
          </pre>
        </details>
      ) : null}
    </div>
  );
}

function ModelPicker({
  open,
  onOpenChange,
  models,
  loading,
  error,
  value,
  onRetry,
  onPick,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  models: ModelEntry[];
  loading: boolean;
  error: string;
  value: string;
  onRetry: () => void;
  onPick: (id: string) => void;
}) {
  const [q, setQ] = useState('');

  useEffect(() => {
    if (open) setQ('');
  }, [open]);

  const list = useMemo(() => {
    const needle = q.trim().toLowerCase();
    if (!needle) return models;
    return models.filter((m) => mdSearchText(m).includes(needle));
  }, [models, q]);

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-2xl">
        <DialogHeader>
          <DialogTitle>选择模型</DialogTitle>
          <DialogDescription>
            目录来自网关；CN / Global 前缀决定路由域，试聊与 /v1 请求使用同一套解析。
          </DialogDescription>
        </DialogHeader>
        <Input
          autoFocus
          type="search"
          placeholder="搜索模型 ID / 名称 / 厂商"
          aria-label="搜索模型"
          value={q}
          onChange={(e) => setQ(e.target.value)}
        />
        <div className="-mx-1 max-h-[52vh] overflow-y-auto">
          {list.map((m) => {
            const parts = chatModelParts(m.id);
            return (
              <button
                key={m.id}
                type="button"
                className={cn(
                  'flex w-full items-center gap-2 rounded-lg px-2 py-1.5 text-left transition-colors hover:bg-secondary',
                  m.id === value && 'bg-accent-soft',
                )}
                onClick={() => onPick(m.id)}>
                <div className="min-w-0 flex-1">
                  <div className="flex flex-wrap items-center gap-1.5">
                    <span className="truncate font-mono text-xs">{parts.name}</span>
                    {m.is_default ? <Tag tone="accent">默认</Tag> : null}
                    {m.busy ? <Tag tone="warn">{m.busy}</Tag> : null}
                    {m.supports_reasoning ? <Tag tone="mute">思考</Tag> : null}
                    {m.supports_images ? <Tag tone="mute">视觉</Tag> : null}
                    {m.supports_tool_call ? <Tag tone="mute">工具</Tag> : null}
                  </div>
                  <div className="truncate text-2xs text-muted-foreground">
                    {[m.name, m.vendor].filter(Boolean).join(' · ') || m.id}
                  </div>
                </div>
                <Tag tone={parts.realm === 'global' ? 'accent' : 'mute'}>
                  {parts.realm === 'global' ? 'GL' : 'CN'}
                </Tag>
                {m.id === value ? <Check className="size-4 text-primary" /> : null}
              </button>
            );
          })}
          {!list.length && !loading ? (
            <div className="flex items-center justify-between gap-3 px-2 py-6 text-xs text-muted-foreground">
              <span>{error ? '模型目录读取失败：' + error : models.length ? '没有匹配的模型' : '上游未返回模型'}</span>
              <Button variant="outline" size="xs" onClick={onRetry}>
                重试
              </Button>
            </div>
          ) : null}
          {!list.length && loading ? (
            <div className="px-2 py-6 text-center text-xs text-muted-foreground">正在向上游查询模型…</div>
          ) : null}
        </div>
      </DialogContent>
    </Dialog>
  );
}
