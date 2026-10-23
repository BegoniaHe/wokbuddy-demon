// 试聊会话数据与流式增量解析（纯函数 + localStorage 持久化）。
//
// 边界：
//   - 会话只存在浏览器 localStorage，不上传、不写 reqlog、不进面板状态；
//   - 原始 SSE 帧只保留在内存（刷新即弃），避免持久化时膨胀或留存提示词；
//   - 发送时只回放 role/content，reasoning 与 tool_calls 仅用于展示。

export interface ChatUsage {
  prompt_tokens?: number;
  completion_tokens?: number;
  total_tokens?: number;
}

export interface ChatToolCall {
  id: string;
  name: string;
  arguments: string;
}

export interface ChatMessage {
  id: string;
  role: 'user' | 'assistant';
  content: string;
  reasoning?: string;
  toolCalls?: ChatToolCall[];
  usage?: ChatUsage;
  elapsedMs?: number;
  ttftMs?: number;
  error?: string;
  aborted?: boolean;
  // raw 只保留在内存：便于对照上游原始流，不写入 localStorage。
  raw?: string[];
  at: number;
}

export interface ChatSession {
  id: string;
  title: string;
  // 空串表示尚未选择（等模型目录到达后补默认值）。
  model: string;
  at: number;
  messages: ChatMessage[];
}

export const LS_CHAT_SESSIONS = 'wb2api.chat.sessions';
export const LS_CHAT_MODEL = 'wb2api.chat.model';
export const CHAT_MAX_SESSIONS = 30;
export const CHAT_MAX_MESSAGES = 200;
export const CHAT_RAW_MAX_FRAMES = 200;
// 工具调用 index 上限：上游帧可携带任意数字，未设界会让 while 循环把数组撑到 OOM。
export const CHAT_MAX_TOOL_CALLS = 128;

// newChatID 生成会话/消息 ID：优先 randomUUID，旧浏览器回落时间戳 + 随机串。
export function newChatID(): string {
  if (typeof crypto !== 'undefined' && typeof crypto.randomUUID === 'function') {
    return crypto.randomUUID();
  }
  return Date.now().toString(36) + Math.random().toString(36).slice(2, 10);
}

export function newChatSession(): ChatSession {
  const now = Date.now();
  return { id: newChatID(), title: '新对话', model: '', at: now, messages: [] };
}

export function chatTitle(text: string): string {
  const t = text.replace(/\s+/g, ' ').trim();
  if (!t) return '新对话';
  return t.length > 24 ? t.slice(0, 24) + '…' : t;
}

// chatModelParts 拆出 "[realm:]model" 的域与裸名；非 cn/global 前缀按裸名处理。
export function chatModelParts(id: string): { realm: 'cn' | 'global'; name: string } {
  const i = id.indexOf(':');
  if (i > 0) {
    const realm = id.slice(0, i);
    if (realm === 'cn' || realm === 'global') return { realm, name: id.slice(i + 1) };
  }
  return { realm: 'cn', name: id };
}

export interface WireMessage {
  role: 'user' | 'assistant';
  content: string;
}

// chatWireMessages 只回放有正文的消息：空内容（纯工具调用/失败）会把上游带崩。
export function chatWireMessages(messages: ChatMessage[]): WireMessage[] {
  const out: WireMessage[] = [];
  for (const m of messages) {
    if (!m.content.trim()) continue;
    out.push({ role: m.role, content: m.content });
  }
  return out;
}

export interface SSEFrames {
  frames: string[];
  rest: string;
}

// splitSSEFrames 按空行切出完整 SSE 帧；尾部不足一帧的部分留在 rest。
export function splitSSEFrames(buf: string): SSEFrames {
  const frames: string[] = [];
  let rest = buf;
  for (;;) {
    const m = /\r?\n\r?\n/.exec(rest);
    if (!m || m.index === undefined) break;
    frames.push(rest.slice(0, m.index));
    rest = rest.slice(m.index + m[0].length);
  }
  return { frames, rest };
}

// sseData 拼接一帧里的全部 data: 行（忽略 event/id/注释行）。
export function sseData(frame: string): string {
  const parts: string[] = [];
  for (const line of frame.split(/\r?\n/)) {
    if (!line.startsWith('data:')) continue;
    parts.push(line.slice(5).replace(/^ /, ''));
  }
  return parts.join('\n');
}

// readSSEStream 逐帧读取 SSE 响应，回调拿到 data 载荷（含尾部无空行的最后一帧）。
// 结束时取消 reader，避免错误中断后底层连接继续缓冲。
export async function readSSEStream(resp: Response, onData: (data: string) => void): Promise<void> {
  const reader = resp.body?.getReader();
  if (!reader) throw new Error('上游没有返回可读流');
  const decoder = new TextDecoder();
  let buf = '';
  try {
    for (;;) {
      const { value, done } = await reader.read();
      if (done) break;
      buf += decoder.decode(value, { stream: true });
      const split = splitSSEFrames(buf);
      buf = split.rest;
      for (const frame of split.frames) onData(sseData(frame));
    }
    onData(sseData(buf));
  } finally {
    void reader.cancel().catch(() => undefined);
  }
}

function parseUsage(v: unknown): ChatUsage | undefined {
  if (!v || typeof v !== 'object') return undefined;
  const u = v as Record<string, unknown>;
  const num = (k: string): number | undefined => (typeof u[k] === 'number' ? (u[k] as number) : undefined);
  const out: ChatUsage = {
    prompt_tokens: num('prompt_tokens'),
    completion_tokens: num('completion_tokens'),
    total_tokens: num('total_tokens'),
  };
  return Object.values(out).some((n) => n !== undefined) ? out : undefined;
}

function mergeToolCalls(msg: ChatMessage, calls: unknown[]): boolean {
  let changed = false;
  for (const raw of calls) {
    if (!raw || typeof raw !== 'object') continue;
    const c = raw as { index?: unknown; id?: unknown; function?: unknown };
    const idx = typeof c.index === 'number' && c.index >= 0 ? c.index : 0;
    if (idx >= CHAT_MAX_TOOL_CALLS) continue;
    const fn = (c.function && typeof c.function === 'object' ? c.function : {}) as {
      name?: unknown;
      arguments?: unknown;
    };
    msg.toolCalls ??= [];
    while (msg.toolCalls.length <= idx) msg.toolCalls.push({ id: '', name: '', arguments: '' });
    const t = msg.toolCalls[idx];
    if (!t) continue;
    if (typeof c.id === 'string' && c.id) {
      t.id = c.id;
      changed = true;
    }
    if (typeof fn.name === 'string' && fn.name) {
      t.name = fn.name;
      changed = true;
    }
    if (typeof fn.arguments === 'string' && fn.arguments) {
      t.arguments += fn.arguments;
      changed = true;
    }
  }
  return changed;
}

// applyChatChunk 把一个 OpenAI 兼容流式 chunk 并入消息；返回是否产生可见变化。
export function applyChatChunk(msg: ChatMessage, payload: unknown): boolean {
  if (!payload || typeof payload !== 'object') return false;
  const p = payload as { choices?: unknown; usage?: unknown };
  let changed = false;

  const usage = parseUsage(p.usage);
  if (usage) {
    msg.usage = usage;
    changed = true;
  }
  if (!Array.isArray(p.choices)) return changed;

  for (const choice of p.choices) {
    if (!choice || typeof choice !== 'object') continue;
    const delta = (choice as { delta?: unknown }).delta;
    if (!delta || typeof delta !== 'object') continue;
    const d = delta as { content?: unknown; reasoning_content?: unknown; reasoning?: unknown; tool_calls?: unknown };
    if (typeof d.content === 'string' && d.content) {
      msg.content += d.content;
      changed = true;
    }
    const reasoning =
      typeof d.reasoning_content === 'string'
        ? d.reasoning_content
        : typeof d.reasoning === 'string'
          ? d.reasoning
          : '';
    if (reasoning) {
      msg.reasoning = (msg.reasoning ?? '') + reasoning;
      changed = true;
    }
    if (Array.isArray(d.tool_calls) && mergeToolCalls(msg, d.tool_calls)) changed = true;
  }
  return changed;
}

// chatError 提取上游错误 chunk 的可读信息；无错误时返回空串。
export function chatError(payload: unknown): string {
  if (!payload || typeof payload !== 'object') return '';
  const p = payload as { error?: unknown; message?: unknown };
  if (typeof p.error === 'string') return p.error;
  if (p.error && typeof p.error === 'object') {
    const e = p.error as { message?: unknown; code?: unknown };
    if (typeof e.message === 'string' && e.message) return e.message;
    if (typeof e.code === 'string' && e.code) return e.code;
  }
  if (typeof p.message === 'string') return p.message;
  return '';
}

function isMessage(v: unknown): v is ChatMessage {
  if (!v || typeof v !== 'object') return false;
  const m = v as Record<string, unknown>;
  return (
    typeof m.id === 'string' &&
    (m.role === 'user' || m.role === 'assistant') &&
    typeof m.content === 'string' &&
    typeof m.at === 'number'
  );
}

function loadMessage(v: unknown): ChatMessage | undefined {
  if (!isMessage(v)) return undefined;
  const m: ChatMessage = {
    id: v.id,
    role: v.role,
    content: v.content,
    at: v.at,
  };
  if (typeof v.reasoning === 'string') m.reasoning = v.reasoning;
  if (typeof v.elapsedMs === 'number') m.elapsedMs = v.elapsedMs;
  if (typeof v.ttftMs === 'number') m.ttftMs = v.ttftMs;
  if (typeof v.error === 'string') m.error = v.error;
  if (v.aborted === true) m.aborted = true;
  if (Array.isArray(v.toolCalls)) {
    m.toolCalls = v.toolCalls
      .slice(0, CHAT_MAX_TOOL_CALLS)
      .filter((t): t is ChatToolCall => {
        if (!t || typeof t !== 'object') return false;
        const c = t as Partial<ChatToolCall>;
        return typeof c.id === 'string' && typeof c.name === 'string' && typeof c.arguments === 'string';
      });
  }
  if (v.usage && typeof v.usage === 'object') m.usage = v.usage as ChatUsage;
  return m;
}

function loadSession(v: unknown): ChatSession | undefined {
  if (!v || typeof v !== 'object') return undefined;
  const s = v as Record<string, unknown>;
  if (typeof s.id !== 'string' || typeof s.title !== 'string' || typeof s.model !== 'string') return undefined;
  if (!Array.isArray(s.messages)) return undefined;
  const messages = s.messages.map(loadMessage).filter((m): m is ChatMessage => !!m);
  return {
    id: s.id,
    title: s.title,
    model: s.model,
    at: typeof s.at === 'number' ? s.at : Date.now(),
    messages: messages.slice(-CHAT_MAX_MESSAGES),
  };
}

export function loadChatSessions(): ChatSession[] {
  try {
    const raw = localStorage.getItem(LS_CHAT_SESSIONS);
    if (!raw) return [];
    const list: unknown = JSON.parse(raw);
    if (!Array.isArray(list)) return [];
    return list
      .map(loadSession)
      .filter((s): s is ChatSession => !!s)
      .slice(0, CHAT_MAX_SESSIONS);
  } catch {
    // 坏数据/隐私模式：按空会话开始，不阻塞面板。
    return [];
  }
}

// storageMessage 显式挑字段，确保 raw（可能含原文帧）永不落盘。
function storageMessage(m: ChatMessage): ChatMessage {
  const out: ChatMessage = { id: m.id, role: m.role, content: m.content, at: m.at };
  if (m.reasoning) out.reasoning = m.reasoning;
  if (m.toolCalls?.length) out.toolCalls = m.toolCalls;
  if (m.usage) out.usage = m.usage;
  if (m.elapsedMs !== undefined) out.elapsedMs = m.elapsedMs;
  if (m.ttftMs !== undefined) out.ttftMs = m.ttftMs;
  if (m.error) out.error = m.error;
  if (m.aborted) out.aborted = true;
  return out;
}

export function saveChatSessions(sessions: ChatSession[]): void {
  try {
    const slim = sessions.slice(0, CHAT_MAX_SESSIONS).map((s) => ({
      ...s,
      messages: s.messages.slice(-CHAT_MAX_MESSAGES).map(storageMessage),
    }));
    localStorage.setItem(LS_CHAT_SESSIONS, JSON.stringify(slim));
  } catch {
    // 配额满/不可用：静默降级，当前会话仍在内存里。
  }
}
