import { describe, expect, it } from 'vitest';
import { isSafeHref, parseBlocks, parseInline } from './markdown';

describe('parseBlocks', () => {
  it('解析围栏代码块并保留语言', () => {
    expect(parseBlocks('前言\n```ts\nconst a = 1;\n```\n结尾')).toEqual([
      { kind: 'paragraph', text: '前言' },
      { kind: 'code', lang: 'ts', code: 'const a = 1;' },
      { kind: 'paragraph', text: '结尾' },
    ]);
  });

  it('未闭合代码块吞到末尾（流式中间态）', () => {
    expect(parseBlocks('```json\n{"a":')).toEqual([{ kind: 'code', lang: 'json', code: '{"a":' }]);
  });

  it('解析标题、列表与引用', () => {
    expect(parseBlocks('## 标题\n\n- 一\n- 二\n\n> 引用\n> 续行')).toEqual([
      { kind: 'heading', level: 2, text: '标题' },
      { kind: 'list', ordered: false, items: ['一', '二'] },
      { kind: 'quote', text: '引用\n续行' },
    ]);
  });

  it('有序列表与分隔线', () => {
    expect(parseBlocks('1. a\n2. b\n\n---')).toEqual([
      { kind: 'list', ordered: true, items: ['a', 'b'] },
      { kind: 'hr' },
    ]);
  });

  it('解析表格', () => {
    expect(parseBlocks('| 名 | 值 |\n| --- | --- |\n| a | 1 |\n| b | 2 |')).toEqual([
      {
        kind: 'table',
        head: ['名', '值'],
        rows: [
          ['a', '1'],
          ['b', '2'],
        ],
      },
    ]);
  });

  it('段落内单换行保留，遇块起始即断开', () => {
    expect(parseBlocks('第一行\n第二行\n# 标题')).toEqual([
      { kind: 'paragraph', text: '第一行\n第二行' },
      { kind: 'heading', level: 1, text: '标题' },
    ]);
  });
});

describe('parseInline', () => {
  it('解析行内代码、粗体、斜体与删除线', () => {
    expect(parseInline('前 `code` **粗** *斜* ~~删~~ 后')).toEqual([
      { kind: 'text', text: '前 ' },
      { kind: 'code', text: 'code' },
      { kind: 'text', text: ' ' },
      { kind: 'strong', text: '粗' },
      { kind: 'text', text: ' ' },
      { kind: 'em', text: '斜' },
      { kind: 'text', text: ' ' },
      { kind: 'strike', text: '删' },
      { kind: 'text', text: ' 后' },
    ]);
  });

  it('只放行 http(s) 链接', () => {
    expect(parseInline('[文档](https://example.com/a)')).toEqual([
      { kind: 'link', text: '文档', href: 'https://example.com/a' },
    ]);
    expect(parseInline('[点我](javascript:void(0))')).toEqual([
      { kind: 'text', text: '[点我](javascript:void(0))' },
    ]);
    expect(isSafeHref('data:text/html,x')).toBe(false);
  });

  it('代码里的标记不被再次解析', () => {
    expect(parseInline('`**not bold**`')).toEqual([{ kind: 'code', text: '**not bold**' }]);
  });

  it('未闭合标记按文本保留', () => {
    expect(parseInline('a **b')).toEqual([{ kind: 'text', text: 'a **b' }]);
  });
});
