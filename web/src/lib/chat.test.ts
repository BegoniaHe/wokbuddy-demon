import { afterEach, describe, expect, it, vi } from 'vitest';
import {
  applyChatChunk,
  CHAT_MAX_TOOL_CALLS,
  chatError,
  LS_CHAT_SESSIONS,
  chatModelParts,
  chatTitle,
  chatWireMessages,
  loadChatSessions,
  readSSEStream,
  splitSSEFrames,
  sseData,
  type ChatMessage,
} from './chat';

function assistant(content = ''): ChatMessage {
  return { id: 'a', role: 'assistant', content, at: 0 };
}

describe('splitSSEFrames', () => {
  it('切出完整帧并保留尾部', () => {
    const r = splitSSEFrames('data: a\n\ndata: b\n\npartial');
    expect(r.frames).toEqual(['data: a', 'data: b']);
    expect(r.rest).toBe('partial');
  });

  it('兼容 CRLF', () => {
    const r = splitSSEFrames('data: a\r\n\r\ndata: b\r\n\r\n');
    expect(r.frames).toEqual(['data: a', 'data: b']);
    expect(r.rest).toBe('');
  });

  it('跨块时不丢半帧', () => {
    const first = splitSSEFrames('data: {"x":');
    expect(first.frames).toEqual([]);
    const second = splitSSEFrames(first.rest + '1}\n\n');
    expect(second.frames).toEqual(['data: {"x":1}']);
  });
});

describe('sseData', () => {
  it('拼接多行 data 并忽略其它字段', () => {
    expect(sseData('event: message\ndata: a\ndata: b\n: ping')).toBe('a\nb');
  });

  it('无 data 时返回空串', () => {
    expect(sseData('event: done')).toBe('');
  });
});

describe('readSSEStream', () => {
  it('跨 chunk 拼接、回调完整帧并补尾部帧', async () => {
    const enc = new TextEncoder();
    const stream = new ReadableStream<Uint8Array>({
      start(ctrl) {
        ctrl.enqueue(enc.encode('data: {"a":'));
        ctrl.enqueue(enc.encode('1}\n\ndata: [DONE]'));
        ctrl.close();
      },
    });
    const got: string[] = [];
    await readSSEStream(new Response(stream), (d) => got.push(d));
    expect(got).toEqual(['{"a":1}', '[DONE]']);
  });

  it('无流时报错', async () => {
    await expect(readSSEStream(new Response(null), () => undefined)).rejects.toThrow('可读流');
  });
});

describe('applyChatChunk', () => {
  it('累加正文与思考', () => {
    const m = assistant();
    applyChatChunk(m, { choices: [{ delta: { reasoning_content: '想' } }] });
    applyChatChunk(m, { choices: [{ delta: { content: '你' } }] });
    applyChatChunk(m, { choices: [{ delta: { content: '好' } }] });
    expect(m.content).toBe('你好');
    expect(m.reasoning).toBe('想');
  });

  it('识别 reasoning 别名', () => {
    const m = assistant();
    applyChatChunk(m, { choices: [{ delta: { reasoning: 'plan' } }] });
    expect(m.reasoning).toBe('plan');
  });

  it('按 index 合并工具调用增量', () => {
    const m = assistant();
    applyChatChunk(m, { choices: [{ delta: { tool_calls: [{ index: 0, id: 't1', function: { name: 'lookup' } }] } }] });
    applyChatChunk(m, { choices: [{ delta: { tool_calls: [{ index: 0, function: { arguments: '{"q":' } }] } }] });
    applyChatChunk(m, { choices: [{ delta: { tool_calls: [{ index: 0, function: { arguments: '1}' } }] } }] });
    expect(m.toolCalls).toEqual([{ id: 't1', name: 'lookup', arguments: '{"q":1}' }]);
  });

  it('忽略越界工具调用 index，避免撑爆数组', () => {
    const m = assistant();
    const changed = applyChatChunk(m, {
      choices: [{ delta: { tool_calls: [{ index: 1_000_000_000, function: { name: 'boom' } }] } }],
    });
    expect(changed).toBe(false);
    expect(m.toolCalls ?? []).toEqual([]);
  });

  it('允许边界内的最大工具调用 index', () => {
    const m = assistant();
    const changed = applyChatChunk(m, {
      choices: [
        { delta: { tool_calls: [{ index: CHAT_MAX_TOOL_CALLS - 1, function: { name: 'last' } }] } },
      ],
    });
    expect(changed).toBe(true);
    expect(m.toolCalls).toHaveLength(CHAT_MAX_TOOL_CALLS);
    expect(m.toolCalls?.[CHAT_MAX_TOOL_CALLS - 1]?.name).toBe('last');
  });

  it('记录 usage', () => {
    const m = assistant();
    const changed = applyChatChunk(m, { usage: { prompt_tokens: 3, completion_tokens: 4, total_tokens: 7 } });
    expect(changed).toBe(true);
    expect(m.usage).toEqual({ prompt_tokens: 3, completion_tokens: 4, total_tokens: 7 });
  });

  it('忽略脏数据', () => {
    const m = assistant();
    expect(applyChatChunk(m, null)).toBe(false);
    expect(applyChatChunk(m, { choices: [null, { delta: { content: 1 } }] })).toBe(false);
    expect(m.content).toBe('');
  });
});

describe('loadChatSessions', () => {
  afterEach(() => vi.unstubAllGlobals());

  it('丢弃损坏的工具调用字段，避免对象参数触发渲染错误', () => {
    const saved = JSON.stringify([
      {
        id: 's',
        title: 't',
        model: 'm',
        at: 1,
        messages: [
          {
            id: 'a',
            role: 'assistant',
            content: '',
            at: 1,
            toolCalls: [
              { id: 'x', name: 'bad', arguments: { a: 1 } },
              { id: 'y', name: 'good', arguments: '{}' },
            ],
          },
        ],
      },
    ]);
    const store = new Map<string, string>([[LS_CHAT_SESSIONS, saved]]);
    vi.stubGlobal('localStorage', {
      getItem: (k: string) => store.get(k) ?? null,
      setItem: (k: string, v: string) => void store.set(k, v),
      removeItem: (k: string) => void store.delete(k),
    });

    const sessions = loadChatSessions();
    expect(sessions[0]?.messages[0]?.toolCalls).toEqual([
      { id: 'y', name: 'good', arguments: '{}' },
    ]);
  });
});

describe('chatError', () => {
  it('取对象与字符串两种错误形态', () => {
    expect(chatError({ error: { message: '上游繁忙' } })).toBe('上游繁忙');
    expect(chatError({ error: 'boom' })).toBe('boom');
    expect(chatError({ choices: [] })).toBe('');
  });
});

describe('chatWireMessages', () => {
  it('跳过空正文，保留角色顺序', () => {
    const msgs: ChatMessage[] = [
      { id: '1', role: 'user', content: 'hi', at: 0 },
      { id: '2', role: 'assistant', content: '', at: 0 },
      { id: '3', role: 'assistant', content: 'hello', at: 0 },
    ];
    expect(chatWireMessages(msgs)).toEqual([
      { role: 'user', content: 'hi' },
      { role: 'assistant', content: 'hello' },
    ]);
  });
});

describe('chatModelParts', () => {
  it('拆域前缀，未知前缀按裸名', () => {
    expect(chatModelParts('global:gpt-5')).toEqual({ realm: 'global', name: 'gpt-5' });
    expect(chatModelParts('cn:glm-5.2')).toEqual({ realm: 'cn', name: 'glm-5.2' });
    expect(chatModelParts('vendor:name')).toEqual({ realm: 'cn', name: 'vendor:name' });
  });
});

describe('chatTitle', () => {
  it('压缩空白并截断', () => {
    expect(chatTitle('  帮我  看看\n这个  ')).toBe('帮我 看看 这个');
    expect(chatTitle('x'.repeat(40))).toBe('x'.repeat(24) + '…');
    expect(chatTitle('  ')).toBe('新对话');
  });
});
